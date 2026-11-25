package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) ErrorBody {
	t.Helper()
	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body %q is not an error envelope: %v", rec.Body.String(), err)
	}
	return env.Error
}

func TestWriteErrorTyped(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", http.NoBody)
	WriteError(rec, req, NotFound("model %s not found", "m_1"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decodeEnvelope(t, rec)
	if body.Code != CodeNotFound || body.Message != "model m_1 not found" {
		t.Fatalf("body = %+v", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
}

func TestWriteErrorHidesInternalCause(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", http.NoBody)
	WriteError(rec, req, errors.New("database password is hunter2"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatal("internal error message leaked to the client")
	}
	if body := decodeEnvelope(t, rec); body.Code != CodeInternal {
		t.Fatalf("code = %q", body.Code)
	}
}

func TestWriteErrorWrapped(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", http.NoBody)
	err := errors.Join(errors.New("context"), Conflict("already exists"))
	WriteError(rec, req, err)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 from the wrapped *Error", rec.Code)
	}
}

// A request whose client went away ends with the context error; that is a
// disconnect, not a server failure.
func TestWriteErrorClientGone(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/chat/completions", http.NoBody)
	WriteError(rec, req, fmt.Errorf("poll: %w", context.Canceled))
	if rec.Code != StatusClientClosedRequest {
		t.Fatalf("status = %d, want 499", rec.Code)
	}
	if logs.Len() != 0 {
		t.Errorf("a client disconnect was logged: %s", logs.String())
	}

	// At shutdown the client is still there and gets a retryable 503.
	ctx, cancel = context.WithCancelCause(t.Context())
	cancel(ErrServerClosing)
	rec = httptest.NewRecorder()
	WriteError(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/x", http.NoBody), context.Canceled)
	if rec.Code != http.StatusServiceUnavailable || decodeEnvelope(t, rec).Code != CodeUnavailable {
		t.Fatalf("status = %d, want 503 while shutting down", rec.Code)
	}
	if logs.Len() != 0 {
		t.Errorf("a shutdown was logged as a failure: %s", logs.String())
	}

	// The same error while the client is still there is a real failure.
	rec = httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest(http.MethodGet, "/x", http.NoBody), context.Canceled)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 while the client is connected", rec.Code)
	}
}

func TestReadJSON(t *testing.T) {
	type body struct {
		Name string `json:"name"`
	}
	tests := []struct {
		name, ct, in string
		wantStatus   int
	}{
		{"ok", "application/json", `{"name":"a"}`, 0},
		{"no content type", "", `{"name":"a"}`, 0},
		{"charset", "application/json; charset=utf-8", `{"name":"a"}`, 0},
		{"unknown field", "application/json", `{"nmae":"a"}`, http.StatusBadRequest},
		{"syntax", "application/json", `{"name":`, http.StatusBadRequest},
		{"type", "application/json", `{"name":1}`, http.StatusBadRequest},
		{"empty", "application/json", ``, http.StatusBadRequest},
		{"trailing", "application/json", `{"name":"a"} {}`, http.StatusBadRequest},
		{"wrong ct", "text/plain", `{"name":"a"}`, http.StatusUnsupportedMediaType},
		{"too large", "application/json", `{"name":"` + strings.Repeat("a", MaxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.in))
			if tt.ct != "" {
				req.Header.Set("Content-Type", tt.ct)
			}
			var dst body
			err := ReadJSON(req, &dst)
			if tt.wantStatus == 0 {
				if err != nil {
					t.Fatalf("ReadJSON: %v", err)
				}
				if dst.Name != "a" {
					t.Fatalf("Name = %q", dst.Name)
				}
				return
			}
			if got := AsError(err).Status; got != tt.wantStatus {
				t.Fatalf("status = %d, want %d (err %v)", got, tt.wantStatus, err)
			}
		})
	}
}

func TestRequestIDs(t *testing.T) {
	var seen string
	h := RequestIDs(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if seen == "" || rec.Header().Get(RequestIDHeader) != seen {
		t.Fatalf("generated id %q, header %q", seen, rec.Header().Get(RequestIDHeader))
	}

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set(RequestIDHeader, "abc")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "abc" {
		t.Fatalf("incoming id not kept: %q", seen)
	}
}

func TestRecover(t *testing.T) {
	h := Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := decodeEnvelope(t, rec); body.Code != CodeInternal {
		t.Fatalf("code = %q", body.Code)
	}
}

func TestRecoverReraisesAbort(t *testing.T) {
	h := Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		p := recover()
		if err, _ := p.(error); !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", p)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", http.NoBody))
}

func TestLoggingKeepsFlusher(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush through middleware: %v", err)
		}
		w.WriteHeader(http.StatusTeapot)
	}), RequestIDs, Logging(logger), Recover)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", http.NoBody))
	if !rec.Flushed {
		t.Fatal("underlying recorder was not flushed")
	}
	if !strings.Contains(buf.String(), "status=200") {
		t.Fatalf("log line %q lacks status=200 (flush commits a 200)", buf.String())
	}
}

// A request that is still waiting when Serve shuts down gets a 503 the
// client retries, not a 500.
func TestServeShutdownAnswersLongPolls(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	waiting := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(waiting)
		<-r.Context().Done()
		WriteError(w, r, r.Context().Err())
	})
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, addr, h) }()

	type result struct {
		status int
		err    error
	}
	got := make(chan result, 1)
	go func() {
		var resp *http.Response
		var err error
		for range 50 { // until the listener is up
			if resp, err = http.Get("http://" + addr + "/poll"); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			got <- result{err: err}
			return
		}
		_ = resp.Body.Close()
		got <- result{status: resp.StatusCode}
	}()
	select {
	case <-waiting:
	case r := <-got:
		t.Fatalf("request ended early: %+v", r)
	}
	cancel()
	if r := <-got; r.err != nil || r.status != http.StatusServiceUnavailable {
		t.Fatalf("request at shutdown: %+v, want 503", r)
	}
	if err := <-served; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}
