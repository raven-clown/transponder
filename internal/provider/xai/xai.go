// Package xai configures the OpenAI adapter for xAI (Grok).
package xai

import (
	"net/http"

	"github.com/raven-clown/transponder/internal/provider/openai"
)

// DefaultBaseURL is xAI's OpenAI-compatible API, from https://docs.x.ai/docs/overview.
const DefaultBaseURL = "https://api.x.ai/v1"

// New returns an adapter for xAI (Grok). An empty baseURL means DefaultBaseURL,
// and an empty apiKey sends no Authorization header.
func New(name, baseURL, apiKey string, client *http.Client) *openai.Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return openai.New(openai.Options{Name: name, BaseURL: baseURL, APIKey: apiKey, Client: client})
}
