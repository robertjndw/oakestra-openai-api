package gateway

import (
	"net/http"

	"github.com/robertjndw/oakestra-openai-api/internal/httpx"
)

// Error codes of the /v1 API.
const (
	codeInvalidAPIKey    = "invalid_api_key"
	codeModelNotFound    = "model_not_found"
	codeNoDeployment     = "no_deployment_available"
	codeUpstreamUnreach  = "upstream_unreachable"
	codeRequestTooLarge  = "request_too_large"
	codeInvalidJSON      = "invalid_json"
	codeMissingParameter = "missing_required_parameter"
	codeInternal         = "internal_error"
	typeInvalidRequest   = "invalid_request_error"
	typeAuthentication   = "authentication_error"
	typeServer           = "server_error"
)

type apiError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

// writeError renders the OpenAI error shape. param may be empty.
func writeError(w http.ResponseWriter, status int, typ, code, param, msg string) {
	e := apiError{Message: msg, Type: typ, Code: code}
	if param != "" {
		e.Param = &param
	}
	httpx.WriteJSON(w, status, map[string]apiError{"error": e})
}
