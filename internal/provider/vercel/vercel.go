// Package vercel configures the OpenAI adapter for Vercel AI Gateway.
package vercel

import (
	"net/http"

	"github.com/raven-clown/transponder/internal/provider/openai"
)

// DefaultBaseURL is Vercel AI Gateway's OpenAI-compatible API, from
// https://vercel.com/docs/ai-gateway/sdks-and-apis/openai-chat-completions.
const DefaultBaseURL = "https://ai-gateway.vercel.sh/v1"

// New returns an adapter for Vercel AI Gateway. An empty baseURL means DefaultBaseURL,
// and an empty apiKey sends no Authorization header.
func New(name, baseURL, apiKey string, client *http.Client) *openai.Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return openai.New(openai.Options{Name: name, BaseURL: baseURL, APIKey: apiKey, Client: client})
}
