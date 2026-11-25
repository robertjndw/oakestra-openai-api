package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

// ErrServerClosing is the cancellation cause of request contexts when
// Serve shuts down.
var ErrServerClosing = errors.New("httpx: server shutting down")

// ShutdownTimeout bounds graceful shutdown. Streaming completions still
// running after it are cut.
const ShutdownTimeout = 10 * time.Second

// Serve runs an HTTP server on addr until ctx is cancelled, then shuts it
// down gracefully. It returns nil after a clean shutdown. It logs through
// slog.Default().
//
// WriteTimeout is deliberately unset: streamed completions run for minutes.
// Handlers that need a bound set one per request with
// http.ResponseController.SetWriteDeadline.
func Serve(ctx context.Context, addr string, h http.Handler) error {
	logger := slog.Default()
	// Requests end with ctx, so streams stop at shutdown.
	// The cause tells WriteError that the server is going away, not the
	// client, so the client still connected gets a retryable 503.
	base, stopRequests := context.WithCancelCause(context.WithoutCancel(ctx))
	defer stopRequests(nil)
	stop := context.AfterFunc(ctx, func() { stopRequests(ErrServerClosing) })
	defer stop()
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return base },
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	logger.Info("http listening", "addr", ln.Addr().String())

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Healthz answers {"status":"ok","version":...}; mount it at /healthz.
func Healthz(version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
	})
}

// NewLogger returns the JSON slog logger every binary uses.
func NewLogger(level slog.Leveler) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
