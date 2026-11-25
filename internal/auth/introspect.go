package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/config"
	"github.com/robertjndw/oakestra-openai-api/internal/httpx"
)

// AdminRole is the system manager's name for the administrator role
// (roles/securityUtils.py, Role.ADMIN). Organization_Admin is a different,
// lesser role and does not qualify.
const AdminRole = "Admin"

const introspectTTL = 60 * time.Second

// maxCachedTokens keeps the cache from growing without bound when many
// short-lived tokens pass through.
const maxCachedTokens = 1024

// User is the authenticated dashboard user.
type User struct {
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
	Admin bool     `json:"admin"`
}

var (
	// ErrInvalidToken means the system manager did not vouch for the token.
	ErrInvalidToken = errors.New("auth: invalid token")
	// ErrIntrospectUnavailable means the system manager could not be asked.
	ErrIntrospectUnavailable = errors.New("auth: token introspection unavailable")
)

// Introspector validates dashboard bearer tokens by asking the system
// manager about the token's own user.
//
// The JWT signature is not verified here (the addon has no key). Instead the
// token is replayed to GET {authURL}/user/{sub}: the system manager only
// answers 200 for a valid token whose identity equals the path user
// (@identity_is_username), and the body's name must match too, so a forged
// payload cannot name somebody else.
type Introspector struct {
	authURL string
	client  *http.Client
	now     func() time.Time

	mu    sync.Mutex
	cache map[[sha256.Size]byte]cachedUser
}

type cachedUser struct {
	user    User
	expires time.Time
}

// NewIntrospector returns an Introspector for the system manager API at
// authURL (for example http://system_manager:10000/api).
func NewIntrospector(authURL string, client *http.Client) *Introspector {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Introspector{
		authURL: strings.TrimRight(authURL, "/"),
		client:  client,
		now:     time.Now,
		cache:   map[[sha256.Size]byte]cachedUser{},
	}
}

// Introspect returns the user behind token, or ErrInvalidToken or
// ErrIntrospectUnavailable. Successful results are cached by token hash; the
// token itself is never stored or logged.
func (i *Introspector) Introspect(ctx context.Context, token string) (User, error) {
	key := sha256.Sum256([]byte(token))
	now := i.now()
	i.mu.Lock()
	c, ok := i.cache[key]
	i.mu.Unlock()
	if ok && now.Before(c.expires) {
		return c.user, nil
	}

	sub, ok := subject(token)
	if !ok {
		return User{}, ErrInvalidToken
	}
	u, err := i.fetch(ctx, token, sub)
	if err != nil {
		return User{}, err
	}

	i.mu.Lock()
	if len(i.cache) >= maxCachedTokens {
		for k, v := range i.cache {
			if !now.Before(v.expires) {
				delete(i.cache, k)
			}
		}
		if len(i.cache) >= maxCachedTokens {
			clear(i.cache)
		}
	}
	i.cache[key] = cachedUser{user: u, expires: now.Add(introspectTTL)}
	i.mu.Unlock()
	return u, nil
}

func (i *Introspector) fetch(ctx context.Context, token, sub string) (User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.authURL+"/user/"+url.PathEscape(sub), http.NoBody)
	if err != nil {
		return User{}, ErrInvalidToken
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := i.client.Do(req)
	if err != nil {
		// url.Error would carry the request URL, which holds the user name
		// only, but keep the message free of anything token-adjacent.
		return User{}, fmt.Errorf("%w: %w", ErrIntrospectUnavailable, errors.Unwrap(err))
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 500:
		return User{}, fmt.Errorf("%w: system manager answered %d", ErrIntrospectUnavailable, resp.StatusCode)
	default:
		// 401 expired, 422 malformed and 403 wrong identity all mean the
		// token does not prove who it claims.
		return User{}, ErrInvalidToken
	}

	var body struct {
		Name  string            `json:"name"`
		Roles []json.RawMessage `json:"roles"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return User{}, ErrInvalidToken
	}
	if body.Name == "" || body.Name != sub {
		return User{}, ErrInvalidToken
	}
	return newUser(body.Name, body.Roles), nil
}

// newUser builds a User from the response's roles. The system manager stores
// plain strings (["Admin"]); other shapes, such as objects with a name, are
// accepted too and anything unrecognised is ignored.
func newUser(name string, raw []json.RawMessage) User {
	u := User{Name: name, Roles: []string{}}
	for _, r := range raw {
		var role string
		if json.Unmarshal(r, &role) != nil {
			var obj struct {
				Name string `json:"name"`
				Role string `json:"role"`
			}
			if json.Unmarshal(r, &obj) != nil {
				continue
			}
			role = obj.Name
			if role == "" {
				role = obj.Role
			}
		}
		if role == "" {
			continue
		}
		u.Roles = append(u.Roles, role)
		if strings.EqualFold(role, AdminRole) {
			u.Admin = true
		}
	}
	return u
}

// subject reads sub from the unverified JWT payload.
func subject(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", false
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Sub == "" {
		return "", false
	}
	return claims.Sub, true
}

type userKey struct{}

// UserFrom returns the user stored by Require.
func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey{}).(User)
	return u, ok
}

// WithUser returns ctx carrying u, for tests and handlers that need one.
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

// devUser is injected when authentication is switched off.
var devUser = User{Name: "dev", Roles: []string{AdminRole}, Admin: true}

// Require authenticates admin requests. In introspect mode the bearer token
// is validated by in; in none mode every request is the fixed dev admin.
// An unknown mode fails closed as introspect.
func Require(mode config.AdminAuthMode, in *Introspector) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mode == config.AdminAuthNone {
				next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), devUser)))
				return
			}
			token, ok := bearer(r)
			if !ok {
				httpx.WriteError(w, r, httpx.Unauthorized("missing bearer token"))
				return
			}
			u, err := in.Introspect(r.Context(), token)
			switch {
			case err == nil:
				next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
			case errors.Is(err, ErrIntrospectUnavailable):
				httpx.WriteError(w, r, &httpx.Error{
					Status: http.StatusServiceUnavailable, Code: httpx.CodeUnavailable,
					Message: "cannot validate the token right now", Err: err,
				})
			case r.Context().Err() != nil:
				httpx.WriteError(w, r, r.Context().Err())
			default:
				httpx.WriteError(w, r, httpx.Unauthorized("invalid or expired token"))
			}
		})
	}
}

// RequireAdmin rejects non-admins with 403. It must run after Require.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := UserFrom(r.Context())
		switch {
		case !ok:
			httpx.WriteError(w, r, httpx.Unauthorized("authentication required"))
		case !u.Admin:
			httpx.WriteError(w, r, httpx.Forbidden("administrator role required"))
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func bearer(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}
