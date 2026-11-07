// Package vllm configures the OpenAI adapter for a vLLM server.
package vllm

import (
	"net/http"

	"github.com/raven-clown/transponder/internal/provider/openai"
)

// DefaultBaseURL is vLLM's default OpenAI-compatible endpoint on the local machine.
const DefaultBaseURL = "http://localhost:8000/v1"

// New returns an adapter for vLLM. An empty baseURL means DefaultBaseURL,
// and an empty apiKey sends no Authorization header.
func New(name, baseURL, apiKey string, client *http.Client) *openai.Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return openai.New(openai.Options{Name: name, BaseURL: baseURL, APIKey: apiKey, Client: client})
}
