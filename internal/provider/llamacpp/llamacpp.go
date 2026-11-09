// Package llamacpp configures the OpenAI adapter for a llama.cpp server.
package llamacpp

import (
	"net/http"

	"github.com/raven-clown/transponder/internal/provider/openai"
)

// DefaultBaseURL is llama.cpp server's default OpenAI-compatible endpoint.
// Note that llama-server listens on :8080 by default.
const DefaultBaseURL = "http://localhost:8080/v1"

// New returns an adapter for llama.cpp server. An empty baseURL means DefaultBaseURL,
// and an empty apiKey sends no Authorization header.
func New(name, baseURL, apiKey string, client *http.Client) *openai.Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return openai.New(openai.Options{Name: name, BaseURL: baseURL, APIKey: apiKey, Client: client})
}
