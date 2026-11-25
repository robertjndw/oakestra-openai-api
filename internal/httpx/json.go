package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
)

// MaxBodyBytes bounds admin JSON request bodies. Model definitions are the
// largest and stay far below this. The /v1 gateway has its own limit
// (OOA_MAX_BODY_BYTES) because prompts can be large.
const MaxBodyBytes = 1 << 20

// WriteJSON writes v as JSON with the given status. Encoding errors after
// the header is sent cannot be reported to the client, so they are logged.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("httpx: encode response", "err", err)
	}
}

// NoContent writes a bare 204.
func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// ReadJSON decodes the request body into dst. It rejects bodies that are
// not JSON, larger than MaxBodyBytes, contain unknown fields, or carry
// trailing data, returning an *Error ready for WriteError.
//
// Unknown fields are rejected so a typo in a client (say "memroy_mb") fails
// loudly instead of silently falling back to a default.
func ReadJSON(r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			return Errorf(http.StatusUnsupportedMediaType, CodeBadRequest, "content type must be application/json")
		}
	}
	body := http.MaxBytesReader(nil, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return BadRequest("request body must contain a single JSON value")
	}
	return nil
}

func decodeError(err error) error {
	var maxErr *http.MaxBytesError
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &maxErr):
		return Errorf(http.StatusRequestEntityTooLarge, CodeTooLarge, "request body exceeds %d bytes", maxErr.Limit)
	case errors.Is(err, io.EOF):
		return BadRequest("request body is empty")
	case errors.As(err, &syntaxErr):
		return BadRequest("malformed JSON at offset %d", syntaxErr.Offset)
	case errors.As(err, &typeErr):
		return BadRequest("field %q must be %s", typeErr.Field, typeErr.Type)
	default:
		// json reports unknown fields only as a formatted string.
		return &Error{Status: http.StatusBadRequest, Code: CodeBadRequest, Message: fmt.Sprintf("invalid JSON: %v", err), Err: err}
	}
}
