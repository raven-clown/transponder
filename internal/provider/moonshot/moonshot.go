// Package moonshot configures the OpenAI adapter for Moonshot AI (Kimi).
package moonshot

import (
	"net/http"

	"github.com/raven-clown/transponder/internal/provider/openai"
)

// DefaultBaseURL is Moonshot AI's OpenAI-compatible API, from the Kimi API docs at
// https://platform.kimi.ai/docs. The China host is https://api.moonshot.cn/v1.
const DefaultBaseURL = "https://api.moonshot.ai/v1"

// New returns an adapter for Moonshot AI. An empty baseURL means DefaultBaseURL,
// and an empty apiKey sends no Authorization header.
func New(name, baseURL, apiKey string, client *http.Client) *openai.Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return openai.New(openai.Options{Name: name, BaseURL: baseURL, APIKey: apiKey, Client: client})
}
