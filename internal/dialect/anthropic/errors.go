package anthropic

import (
	"encoding/json"
	"net/http"
)

type wireError struct {
	Type  string          `json:"type"`
	Error wireErrorDetail `json:"error"`
}

type wireErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	// Code is transponder's own reason, such as budget_exceeded, next to Anthropic's type.
	Code string `json:"code,omitempty"`
}

// ErrorType returns the Anthropic error type for an HTTP status.
func ErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusPaymentRequired:
		return "billing_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// WriteError answers with an error in the Anthropic shape.
func WriteError(w http.ResponseWriter, status int, errType, message string) {
	WriteErrorCode(w, status, errType, "", message)
}

// WriteErrorCode answers with an error in the Anthropic shape that also names transponder's
// code, so clients can tell a spent budget from a provider's billing problem.
func WriteErrorCode(w http.ResponseWriter, status int, errType, code, message string) {
	b, _ := json.Marshal(wireError{Type: "error", Error: wireErrorDetail{Type: errType, Message: message, Code: code}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b) //nolint:gosec // JSON we encoded, served as application/json
}
