package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/httpx"
)

// Handler serves the /v1 routes behind the API key check. Health endpoints
// are mounted by the caller.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	for api, path := range map[string]string{
		"chat.completions": "/v1/chat/completions",
		"completions":      "/v1/completions",
		"embeddings":       "/v1/embeddings",
	} {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) { g.serveProxy(w, r, api) })
	}
	mux.HandleFunc("GET /v1/models", g.listModels)
	mux.HandleFunc("GET /v1/models/{id...}", g.getModel)
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, typeInvalidRequest, "not_found", "", "unknown route "+r.Method+" "+r.URL.Path)
	})
	return g.requireKey(mux)
}

func (g *Gateway) requireKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, secret, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		secret = strings.TrimSpace(secret)
		if !ok || !strings.EqualFold(scheme, "Bearer") || secret == "" || !g.opts.Auth.Authenticate(r.Context(), secret) {
			writeError(w, http.StatusUnauthorized, typeAuthentication, codeInvalidAPIKey, "",
				"Incorrect API key provided. Send a valid key as 'Authorization: Bearer <key>'.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (g *Gateway) serveProxy(w http.ResponseWriter, r *http.Request, api string) {
	ctx := r.Context()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.opts.MaxBodyBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, http.StatusRequestEntityTooLarge, typeInvalidRequest, codeRequestTooLarge, "",
				fmt.Sprintf("request body exceeds %d bytes", g.opts.MaxBodyBytes))
			return
		}
		writeError(w, http.StatusBadRequest, typeInvalidRequest, codeInvalidJSON, "", "could not read the request body")
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		writeError(w, http.StatusBadRequest, typeInvalidRequest, codeInvalidJSON, "", "request body must be a JSON object")
		return
	}
	var model string
	if err := json.Unmarshal(fields["model"], &model); err != nil || model == "" {
		writeError(w, http.StatusBadRequest, typeInvalidRequest, codeMissingParameter, "model", "you must provide a model parameter")
		return
	}

	st := g.state.Load()
	auto := model == AutoModel
	eps := st.byModel[model]
	if auto {
		eps = st.auto
	}
	if len(eps) == 0 {
		if auto || (g.opts.KnownModel != nil && g.opts.KnownModel(model)) {
			writeError(w, http.StatusServiceUnavailable, typeServer, codeNoDeployment, "",
				fmt.Sprintf("no deployment is ready for model %q", model))
			return
		}
		writeError(w, http.StatusNotFound, typeInvalidRequest, codeModelNotFound, "model",
			fmt.Sprintf("the model %q does not exist", model))
		return
	}

	ranked := demoteFailed(g.rank(ctx, buildRequest(api, fields, r.Header), eps, auto))
	attempts := min(g.opts.MaxAttempts, len(ranked))
	for _, e := range ranked[:attempts] {
		resp, done, err := g.send(ctx, e, r.Method, r.URL.Path, r.URL.RawQuery, r.Header, fields)
		if err != nil {
			if ctx.Err() != nil {
				httpx.WriteError(w, r, ctx.Err())
				return
			}
			g.log.WarnContext(ctx, "engine unreachable", "deployment", e.DeploymentID, "node", e.Node, "err", err)
			g.failed(e)
			continue
		}
		// A deferred release because relay panics to abort a broken stream.
		func() {
			defer done()
			g.relay(w, resp, e)
		}()
		return
	}
	writeError(w, http.StatusBadGateway, typeServer, codeUpstreamUnreach, "",
		fmt.Sprintf("no engine for model %q could be reached", model))
}

// send forwards one request to e. The returned done func must be called when
// the response body has been consumed; until then the request counts as in
// flight. An error means no response was received.
func (g *Gateway) send(ctx context.Context, e *ep, method, path, rawQuery string, in http.Header,
	fields map[string]json.RawMessage,
) (*http.Response, func(), error) {
	// fields is shared by every attempt, so the rewrite works on a copy.
	out := maps.Clone(fields)
	name, err := json.Marshal(e.ModelName)
	if err != nil {
		return nil, nil, err
	}
	out["model"] = name
	body, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}

	target := e.BaseURL + path
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	copyRequestHeaders(req.Header, in)
	req.Header.Set("Content-Type", "application/json")
	if id := httpx.RequestID(ctx); id != "" {
		req.Header.Set(httpx.RequestIDHeader, id)
	}
	if e.EngineKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.EngineKey)
	}

	e.inflight.Add(1)
	start := time.Now()
	resp, err := g.transport.RoundTrip(req)
	if err != nil {
		e.inflight.Add(-1)
		return nil, nil, err
	}
	e.observeTTFT(time.Since(start))
	e.failedAt.Store(0)
	return resp, func() { e.inflight.Add(-1) }, nil
}

// relay passes the engine's answer to the client unchanged, apart from
// hop-by-hop headers and the SSE tuning headers.
func (g *Gateway) relay(w http.ResponseWriter, resp *http.Response, e *ep) {
	defer func() { _ = resp.Body.Close() }()
	h := w.Header()
	for k, vs := range resp.Header {
		h[k] = slices.Clone(vs)
	}
	stripHopByHop(h)

	sse := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	if sse {
		h.Del("Content-Length")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
	}
	if g.opts.DebugHeaders {
		h.Set("X-OOA-Deployment", e.DeploymentID)
		h.Set("X-OOA-Node", e.Node)
		h.Set("X-OOA-Model", e.ModelName)
	}
	w.WriteHeader(resp.StatusCode)

	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // client gone; closing the body cancels the engine call
			}
			if sse {
				_ = rc.Flush()
			}
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, io.EOF) {
			// Headers are out already. Aborting the connection is the only
			// way to tell the client the stream is incomplete rather than
			// finished.
			panic(http.ErrAbortHandler)
		}
		return
	}
}

var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Proxy-Connection",
}

// stripHopByHop removes the standard hop-by-hop headers and any header the
// Connection header names.
func stripHopByHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for name := range strings.SplitSeq(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, k := range hopByHop {
		h.Del(k)
	}
}

// copyRequestHeaders copies end-to-end headers to the engine request, minus
// the client's credentials and the scheduler hint headers. Content-Length is
// recomputed by net/http because the body is rewritten.
func copyRequestHeaders(dst, src http.Header) {
	h := src.Clone()
	stripHopByHop(h)
	h.Del("Authorization")
	h.Del("Content-Length")
	h.Del("Content-Type")
	for k := range h {
		if strings.HasPrefix(k, hintHeaderPrefix) {
			delete(h, k)
		}
	}
	maps.Copy(dst, h)
}
