// Package openai is the adapter for OpenAI and any server that speaks its API.
package openai

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/schema"
)

// DefaultBaseURL is the OpenAI API.
const DefaultBaseURL = "https://api.openai.com/v1"

const maxErrorBody = 1 << 20

var forwardHeaders = []string{"X-Request-ID", "Traceparent", "Tracestate"}

// Options configure an adapter.
type Options struct {
	Name    string
	BaseURL string
	APIKey  string
	Client  *http.Client
}

// Adapter talks to an OpenAI-compatible API.
type Adapter struct {
	name    string
	baseURL string
	apiKey  string
	client  *http.Client
}

// New returns an adapter. An empty BaseURL means the OpenAI API, an empty APIKey sends no Authorization header.
func New(o Options) *Adapter {
	a := &Adapter{name: o.Name, baseURL: strings.TrimRight(o.BaseURL, "/"), apiKey: o.APIKey, client: o.Client}
	if a.baseURL == "" {
		a.baseURL = DefaultBaseURL
	}
	if a.client == nil {
		a.client = http.DefaultClient
	}
	return a
}

// Name returns the configured provider name.
func (a *Adapter) Name() string { return a.name }

// Dialect returns "openai".
func (a *Adapter) Dialect() string { return "openai" }

// BaseURL returns the API root calls are sent to.
func (a *Adapter) BaseURL() string { return a.baseURL }

// Native posts the body to the API and returns the response when it is 2xx.
func (a *Adapter) Native(ctx context.Context, call *provider.NativeCall) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+call.Path, bytes.NewReader(call.Body))
	if err != nil {
		return nil, &provider.Error{Provider: a.name, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	for _, h := range forwardHeaders {
		if v := call.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, &provider.Error{Provider: a.name, Retryable: true, Err: err}
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return nil, &provider.Error{
		Provider:  a.name,
		Status:    resp.StatusCode,
		Retryable: provider.RetryableStatus(resp.StatusCode),
		Body:      body,
		Err:       fmt.Errorf("upstream answered %s", resp.Status),
	}
}

var errCanonical = fmt.Errorf("%w: canonical calls to openai adapters arrive with translation", provider.ErrUnsupported)

// Chat is not available yet; OpenAI callers use Native.
func (a *Adapter) Chat(context.Context, *schema.ChatRequest) (*schema.ChatResponse, error) {
	return nil, errCanonical
}

// ChatStream is not available yet; OpenAI callers use Native.
func (a *Adapter) ChatStream(context.Context, *schema.ChatRequest) (schema.Stream, error) {
	return nil, errCanonical
}

// Embed is not available yet; OpenAI callers use Native.
func (a *Adapter) Embed(context.Context, *schema.EmbedRequest) (*schema.EmbedResponse, error) {
	return nil, errCanonical
}

var _ interface {
	provider.Provider
	provider.Native
} = (*Adapter)(nil)
