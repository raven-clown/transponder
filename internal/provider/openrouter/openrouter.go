// Package openrouter configures the OpenAI adapter for OpenRouter.
package openrouter

import (
	"net/http"

	"github.com/raven-clown/transponder/internal/provider/openai"
)

// DefaultBaseURL is OpenRouter's OpenAI-compatible API, from https://openrouter.ai/docs/quickstart.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// New returns an adapter for OpenRouter. An empty baseURL means DefaultBaseURL.
func New(name, baseURL, apiKey string, client *http.Client) *openai.Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return openai.New(openai.Options{Name: name, BaseURL: baseURL, APIKey: apiKey, Client: client})
}
