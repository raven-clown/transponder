// Package together configures the OpenAI adapter for Together AI.
package together

import (
	"net/http"

	"github.com/raven-clown/transponder/internal/provider/openai"
)

// DefaultBaseURL is Together AI's OpenAI-compatible API, from
// https://docs.together.ai/docs/openai-api-compatibility.
const DefaultBaseURL = "https://api.together.xyz/v1"

// New returns an adapter for Together AI. An empty baseURL means DefaultBaseURL,
// and an empty apiKey sends no Authorization header.
func New(name, baseURL, apiKey string, client *http.Client) *openai.Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return openai.New(openai.Options{Name: name, BaseURL: baseURL, APIKey: apiKey, Client: client})
}
