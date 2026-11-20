// Package lmstudio configures the OpenAI adapter for LM Studio.
package lmstudio

import (
	"net/http"

	"github.com/raven-clown/transponder/internal/provider/openai"
)

// DefaultBaseURL is LM Studio's OpenAI-compatible API on the local machine, from
// https://lmstudio.ai/docs/app/api/endpoints/openai. An api key is optional.
const DefaultBaseURL = "http://localhost:1234/v1"

// New returns an adapter for LM Studio. An empty baseURL means DefaultBaseURL,
// and an empty apiKey sends no Authorization header.
func New(name, baseURL, apiKey string, client *http.Client) *openai.Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return openai.New(openai.Options{Name: name, BaseURL: baseURL, APIKey: apiKey, Client: client})
}
