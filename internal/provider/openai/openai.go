// Package openai is the adapter for OpenAI and any server that speaks its API.
package openai

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	wire "github.com/raven-clown/transponder/internal/dialect/openai"
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

// Chat sends a canonical request as an OpenAI chat completion.
func (a *Adapter) Chat(ctx context.Context, req *schema.ChatRequest) (*schema.ChatResponse, error) {
	r := *req
	r.Stream = false
	body, err := wire.EncodeChatRequest(&r)
	if err != nil {
		return nil, err
	}
	out, err := a.call(ctx, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	return wire.DecodeChatResponse(out)
}

// ChatStream sends a canonical request as a streamed chat completion.
func (a *Adapter) ChatStream(ctx context.Context, req *schema.ChatRequest) (schema.Stream, error) {
	r := *req
	r.Stream = true
	body, err := wire.EncodeChatRequest(&r)
	if err != nil {
		return nil, err
	}
	resp, err := a.Native(ctx, &provider.NativeCall{Path: "/chat/completions", Body: body, Header: headerFrom(ctx)}) //nolint:bodyclose // the stream closes it
	if err != nil {
		return nil, err
	}
	return &stream{body: resp.Body, r: wire.NewSSEReader(resp.Body)}, nil
}

// Embed sends a canonical embeddings request.
func (a *Adapter) Embed(ctx context.Context, req *schema.EmbedRequest) (*schema.EmbedResponse, error) {
	body, err := wire.EncodeEmbedRequest(req)
	if err != nil {
		return nil, err
	}
	out, err := a.call(ctx, "/embeddings", body)
	if err != nil {
		return nil, err
	}
	return wire.DecodeEmbedResponse(out)
}

func (a *Adapter) call(ctx context.Context, path string, body []byte) ([]byte, error) {
	resp, err := a.Native(ctx, &provider.NativeCall{Path: path, Body: body, Header: headerFrom(ctx)})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, &provider.Error{Provider: a.name, Retryable: true, Err: err}
	}
	return out, nil
}

// headerFrom returns the trace headers the server put in the context for upstream calls.
func headerFrom(ctx context.Context) http.Header {
	if h, ok := provider.HeaderFrom(ctx); ok {
		return h
	}
	return http.Header{}
}

const maxResponseBody = 64 << 20

type stream struct {
	body    io.Closer
	r       *wire.SSEReader
	decoder wire.ChunkDecoder
	pending []schema.Event
}

func (s *stream) Recv() (schema.Event, error) {
	for len(s.pending) == 0 {
		data, err := s.r.Next()
		if err != nil {
			return schema.Event{}, err
		}
		if s.pending, err = s.decoder.Events(data); err != nil {
			return schema.Event{}, err
		}
	}
	ev := s.pending[0]
	s.pending = s.pending[1:]
	return ev, nil
}

func (s *stream) Close() error { return s.body.Close() }

var _ interface {
	provider.Provider
	provider.Native
} = (*Adapter)(nil)
