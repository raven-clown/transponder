// Package github configures the OpenAI adapter for GitHub Models.
package github

import (
	"net/http"

	"github.com/raven-clown/transponder/internal/provider/openai"
)

// DefaultBaseURL is the GitHub Models inference endpoint, OpenAI-compatible.
// https://docs.github.com/en/github-models/use-github-models/prototyping-with-ai-models
const DefaultBaseURL = "https://models.github.ai/inference"

// New returns an adapter for GitHub Models. An empty baseURL means DefaultBaseURL.
// The key is a GitHub token with the models scope, sent as a bearer token.
func New(name, baseURL, apiKey string, client *http.Client) *openai.Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return openai.New(openai.Options{Name: name, BaseURL: baseURL, APIKey: apiKey, Client: client})
}
