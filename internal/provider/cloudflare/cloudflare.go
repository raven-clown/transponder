// Package cloudflare configures the OpenAI adapter for Cloudflare AI Gateway.
package cloudflare

import (
	"net/http"

	"github.com/raven-clown/transponder/internal/provider/openai"
)

// New returns an adapter for a Cloudflare AI Gateway OpenAI-compatible endpoint.
func New(name, baseURL, apiKey string, client *http.Client) *openai.Adapter {
	return openai.New(openai.Options{Name: name, BaseURL: baseURL, APIKey: apiKey, Client: client})
}
