package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// Error codes used in the envelope. Clients branch on the code, never on
// the message.
const (
	CodeBadRequest      = "bad_request"
	CodeUnauthorized    = "unauthorized"
	CodeForbidden       = "forbidden"
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeUnprocessable   = "unprocessable"
	CodeTooLarge        = "too_large"
	CodeRateLimited     = "rate_limited"
	CodeUnavailable     = "unavailable"
	CodeInternal        = "internal"
	CodeInvalidState    = "invalid_state"
	CodeUpstreamFailure = "upstream_failure"
)

// Error is an error that knows how to present itself to an HTTP client.
type Error struct {
	// Status is the HTTP status code.
	Status int
	// Code is the stable machine-readable code (one of the Code* constants).
	Code string
	// Message is safe to show to the user.
	Message string
	// Type is only set on the OpenAI-compatible /v1 endpoints, where clients
	// expect error.type (for example "invalid_request_error").
	Type string
	// Details is optional structured context, such as per-field validation
	// errors.
	Details any
	// Err is the underlying cause. It is logged, never sent.
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// Envelope is the JSON body of every error response.
type Envelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the inner object of Envelope.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Details any    `json:"details,omitempty"`
}

// Errorf builds an *Error with a formatted message.
func Errorf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// BadRequest is a 400 bad_request.
func BadRequest(format string, args ...any) *Error {
	return Errorf(http.StatusBadRequest, CodeBadRequest, format, args...)
}

// Unauthorized is a 401 unauthorized.
func Unauthorized(format string, args ...any) *Error {
	return Errorf(http.StatusUnauthorized, CodeUnauthorized, format, args...)
}

// Forbidden is a 403 forbidden.
func Forbidden(format string, args ...any) *Error {
	return Errorf(http.StatusForbidden, CodeForbidden, format, args...)
}

// NotFound is a 404 not_found.
func NotFound(format string, args ...any) *Error {
	return Errorf(http.StatusNotFound, CodeNotFound, format, args...)
}

// Conflict is a 409 conflict.
func Conflict(format string, args ...any) *Error {
	return Errorf(http.StatusConflict, CodeConflict, format, args...)
}

// Unprocessable is a 422 unprocessable, for syntactically valid requests
// that fail validation.
func Unprocessable(format string, args ...any) *Error {
	return Errorf(http.StatusUnprocessableEntity, CodeUnprocessable, format, args...)
}

// Unavailable is a 503 unavailable.
func Unavailable(format string, args ...any) *Error {
	return Errorf(http.StatusServiceUnavailable, CodeUnavailable, format, args...)
}

// Internal wraps an unexpected error as a 500 whose message hides the cause.
func Internal(err error) *Error {
	return &Error{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "internal error", Err: err}
}

// AsError converts any error to an *Error. Errors that are not (and do not
// wrap) an *Error become a 500 internal error.
func AsError(err error) *Error {
	if he, ok := errors.AsType[*Error](err); ok {
		return he
	}
	return Internal(err)
}

// StatusClientClosedRequest is nginx's 499: the client hung up before the
// response was ready.
const StatusClientClosedRequest = 499

// WriteError renders err as the JSON error envelope. 5xx causes are logged
// with the request id so the hidden cause can still be found.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	// Handlers return the context error when the request context ends. A
	// client that hung up (a closed browser tab, a cancelled completion)
	// reads no response; a server shutting down (see Serve) owes the client
	// one it will retry. Neither is a server failure worth an error log.
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		if errors.Is(context.Cause(r.Context()), ErrServerClosing) {
			WriteJSON(w, http.StatusServiceUnavailable, Envelope{Error: ErrorBody{
				Code: CodeUnavailable, Message: "the server is shutting down; retry"}})
		} else {
			w.WriteHeader(StatusClientClosedRequest)
		}
		return
	}
	he := AsError(err)
	if he.Status >= 500 {
		slog.ErrorContext(r.Context(), "request failed",
			"method", r.Method, "path", r.URL.Path,
			"request_id", RequestID(r.Context()),
			"status", he.Status, "err", err)
	}
	WriteJSON(w, he.Status, Envelope{Error: ErrorBody{
		Code:    he.Code,
		Message: he.Message,
		Type:    he.Type,
		Details: he.Details,
	}})
}
