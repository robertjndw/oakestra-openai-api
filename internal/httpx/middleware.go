package httpx

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"time"
)

// RequestIDHeader carries the request id in both directions. An incoming
// value is kept (so a request can be traced through the dashboard nginx and
// the addon nginx), otherwise a new one is generated.
const RequestIDHeader = "X-Request-ID"

type ctxKey int

const requestIDKey ctxKey = iota

// RequestID returns the id stored by the RequestIDs middleware, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// WithRequestID returns ctx carrying id, for code that starts work outside
// an HTTP request but wants log lines to correlate.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// Middleware is the usual func(http.Handler) http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so that the first one listed is the outermost.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for _, m := range slices.Backward(mw) {
		h = m(h)
	}
	return h
}

// RequestIDs assigns every request an id, exposes it in the response
// header and stores it in the context.
func RequestIDs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" || len(id) > 128 {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Logging logs one line per request at the end of it. Health probes are
// logged at debug level only, because Docker and the UI poll them
// constantly.
func Logging(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			level := slog.LevelInfo
			switch {
			// A request whose context ended (client gone, shutdown) did
			// not fail on our side.
			case rec.status >= 500 && r.Context().Err() == nil:
				level = slog.LevelError
			case r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/health":
				level = slog.LevelDebug
			}
			logger.LogAttrs(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.Status()),
				slog.Int64("bytes", rec.bytes),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", RequestID(r.Context())),
			)
		})
	}
}

// Recover turns a handler panic into a 500 envelope (if nothing was written
// yet) and logs the stack. http.ErrAbortHandler is re-raised: it is the
// standard way to abort a streaming response and must reach net/http.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w}
		defer func() {
			if p := recover(); p != nil {
				handlePanic(rec, r, p)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

func handlePanic(rec *recorder, r *http.Request, p any) {
	if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(p)
	}
	slog.ErrorContext(r.Context(), "handler panic",
		"method", r.Method, "path", r.URL.Path,
		"request_id", RequestID(r.Context()),
		"panic", fmt.Sprint(p), "stack", string(debug.Stack()))
	if !rec.wroteHeader {
		WriteError(rec, r, Internal(fmt.Errorf("panic: %v", p)))
	}
}

// recorder captures the status and size for logging. It forwards Flush and
// Hijack and implements Unwrap so http.ResponseController reaches the real
// writer, which the streaming gateway depends on.
type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Status returns the written status, 200 if the handler wrote a body
// without a header, or 0 if it wrote nothing at all.
func (r *recorder) Status() int { return r.status }

func (r *recorder) Flush() {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(r.ResponseWriter).Hijack()
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
