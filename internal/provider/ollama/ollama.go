// Package ollama configures the OpenAI adapter for a local Ollama server.
package ollama

import (
	"net/http"

	"github.com/raven-clown/transponder/internal/provider/openai"
)

// DefaultBaseURL is Ollama's OpenAI-compatible endpoint on the local machine.
const DefaultBaseURL = "http://localhost:11434/v1"

// New returns an adapter for Ollama. An empty baseURL means DefaultBaseURL,
// and an empty apiKey sends no Authorization header.
func New(name, baseURL, apiKey string, client *http.Client) *openai.Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return openai.New(openai.Options{Name: name, BaseURL: baseURL, APIKey: apiKey, Client: client})
}
