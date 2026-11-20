package openai

import (
	"encoding/json"
	"net/http"
)

type wireError struct {
	Error wireErrorBody `json:"error"`
}

type wireErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// WriteError writes an error in the OpenAI shape. Empty param and code are sent as null.
func WriteError(w http.ResponseWriter, status int, errType, code, param, message string) {
	body := wireError{Error: wireErrorBody{Message: message, Type: errType, Param: nullable(param), Code: nullable(code)}}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
