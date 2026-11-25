package oakestra

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Oakestra issues access tokens for 10 minutes and refresh tokens for 7 days
// (system_manager.py JWT_*_EXPIRES). These are the fallbacks when a token is
// not a JWT whose exp claim we can read.
const (
	defaultAccessTTL  = 10 * time.Minute
	defaultRefreshTTL = 7 * 24 * time.Hour
	// refreshMargin renews the access token this long before it expires, so
	// a request that is slow to reach the server does not race the expiry.
	refreshMargin = time.Minute
)

// authCall is one login or refresh in flight. Concurrent callers that need
// a token wait on the same call instead of each logging in.
type authCall struct {
	done  chan struct{}
	token string
	err   error
}

// token returns a usable access token. stale is the token a request was
// just rejected with (or ""); a current token equal to stale is not reused,
// which forces a refresh after a 401 even if our clock thinks it is valid.
func (c *HTTPClient) token(ctx context.Context, stale string) (string, error) {
	c.authMu.Lock()
	if c.access != "" && c.access != stale && c.now().Before(c.accessExp.Add(-refreshMargin)) {
		tok := c.access
		c.authMu.Unlock()
		return tok, nil
	}
	call := c.inflight
	if call == nil {
		call = &authCall{done: make(chan struct{})}
		c.inflight = call
		refresh := ""
		if c.refresh != "" && c.now().Before(c.refreshExp) {
			refresh = c.refresh
		}
		// The login runs detached from the first caller's context: other
		// callers wait on its result and must not fail because the first
		// one gave up.
		go c.authenticate(context.WithoutCancel(ctx), call, refresh)
	}
	c.authMu.Unlock()

	select {
	case <-call.done:
		return call.token, call.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// authenticate refreshes with the refresh token when there is one and falls
// back to a full login when that fails for any reason (expired refresh
// token, user re-created, server secret rotated).
func (c *HTTPClient) authenticate(ctx context.Context, call *authCall, refresh string) {
	var resp LoginResponse
	var err error
	if refresh != "" {
		resp, err = c.refreshToken(ctx, refresh)
		if err != nil {
			c.log.Debug("oakestra: token refresh failed, logging in again", "err", err)
		}
	}
	if refresh == "" || err != nil {
		resp, err = c.login(ctx)
	}

	c.authMu.Lock()
	defer c.authMu.Unlock()
	c.inflight = nil
	if err != nil {
		c.access, c.accessExp = "", time.Time{}
		call.err = err
		close(call.done)
		return
	}
	now := c.now()
	c.access = resp.Token
	c.accessExp = tokenExpiry(resp.Token, now, defaultAccessTTL)
	if resp.RefreshToken != "" {
		c.refresh = resp.RefreshToken
		c.refreshExp = tokenExpiry(resp.RefreshToken, now, defaultRefreshTTL)
	}
	call.token = resp.Token
	close(call.done)
}

func (c *HTTPClient) login(ctx context.Context) (LoginResponse, error) {
	body := LoginRequest{Username: c.opts.User, Password: c.opts.Password, OrganizationName: c.opts.Org}
	var resp LoginResponse
	if err := c.send(ctx, "login", http.MethodPost, c.opts.API+"/api/auth/login", "", body, &resp); err != nil {
		return LoginResponse{}, err
	}
	if resp.Token == "" {
		// The system manager answers 200 {} for some credential problems.
		return LoginResponse{}, &APIError{Op: "login", Status: http.StatusUnauthorized, Message: "no token in login response"}
	}
	return resp, nil
}

func (c *HTTPClient) refreshToken(ctx context.Context, refresh string) (LoginResponse, error) {
	var resp LoginResponse
	if err := c.send(ctx, "refresh token", http.MethodPost, c.opts.API+"/api/auth/refresh", refresh, nil, &resp); err != nil {
		return LoginResponse{}, err
	}
	if resp.Token == "" {
		return LoginResponse{}, errors.New("oakestra: refresh token: no token in response")
	}
	return resp, nil
}

// tokenExpiry reads the exp claim of a JWT without verifying it; we only
// need to know when to renew, the server does the verifying. Tokens that
// are not JWTs get the documented lifetime from now.
func tokenExpiry(tok string, now time.Time, fallback time.Duration) time.Time {
	parts := strings.Split(tok, ".")
	if len(parts) == 3 {
		if raw, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct {
				Exp float64 `json:"exp"`
			}
			if json.Unmarshal(raw, &claims) == nil && claims.Exp > 0 {
				return time.Unix(int64(claims.Exp), 0)
			}
		}
	}
	return now.Add(fallback)
}
