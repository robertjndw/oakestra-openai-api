// Package httpx holds the small HTTP helpers every handler shares: JSON
// request and response helpers, the error envelope, the request-id,
// logging and recover middleware, and Serve with graceful shutdown.
//
// Every non-2xx JSON response of the admin API uses one envelope:
//
//	{"error": {"code": "not_found", "message": "model m_01... not found"}}
//
// Handlers return errors as *Error (see the constructors such as NotFound)
// and let WriteError render them; any other error becomes a 500 whose
// message is not leaked to the client. The /v1 gateway uses the same shape
// with Type set, which is what OpenAI clients parse.
//
// The middleware wraps the ResponseWriter but keeps http.Flusher working and
// exposes Unwrap, so SSE passthrough behind it still flushes through
// http.ResponseController.
package httpx
