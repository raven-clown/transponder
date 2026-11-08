// Package anthropic is the adapter for Anthropic's Messages API.
package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	wire "github.com/raven-clown/transponder/internal/dialect/anthropic"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/sse"
)

// DefaultBaseURL is Anthropic's API.
const DefaultBaseURL = "https://api.anthropic.com/v1"

// DefaultVersion is sent as anthropic-version when the caller sends none.
const DefaultVersion = "2023-06-01"

const maxResponseBody = 64 << 20

// Adapter talks to the Messages API.
type Adapter struct {
	name, baseURL, apiKey string
	client                *http.Client
}

// New returns an adapter. An empty baseURL means DefaultBaseURL.
func New(name, baseURL, apiKey string, client *http.Client) *Adapter {
	a := &Adapter{name: name, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, client: client}
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

// Dialect returns "anthropic".
func (a *Adapter) Dialect() string { return "anthropic" }

// BaseURL returns the API root calls are sent to.
func (a *Adapter) BaseURL() string { return a.baseURL }

// Native posts the body with the gateway's key. The caller's anthropic-version and
// anthropic-beta go along, so new API features keep working through the gateway.
func (a *Adapter) Native(ctx context.Context, call *provider.NativeCall) (*http.Response, error) {
	h := http.Header{}
	h.Set("X-Api-Key", a.apiKey)
	version := call.Header.Get("Anthropic-Version")
	if version == "" {
		version = DefaultVersion
	}
	h.Set("Anthropic-Version", version)
	for _, beta := range call.Header.Values("Anthropic-Beta") {
		h.Add("Anthropic-Beta", beta)
	}
	return provider.Send(ctx, a.client, a.name, a.baseURL+call.Path, call.Body, h, call.Header)
}

// translated prepares a canonical request for Anthropic: current models reject sampling
// parameters, so they are dropped.
func translated(req *schema.ChatRequest, stream bool) ([]byte, error) {
	r := *req
	r.Temperature, r.TopP, r.Stream = nil, nil, stream
	return wire.EncodeRequest(&r)
}

func headerFrom(ctx context.Context) http.Header {
	if h, ok := provider.HeaderFrom(ctx); ok {
		return h
	}
	return http.Header{}
}

// Chat sends a canonical request as a Messages call.
func (a *Adapter) Chat(ctx context.Context, req *schema.ChatRequest) (*schema.ChatResponse, error) {
	body, err := translated(req, false)
	if err != nil {
		return nil, err
	}
	resp, err := a.Native(ctx, &provider.NativeCall{Path: "/messages", Body: body, Header: headerFrom(ctx)})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, &provider.Error{Provider: a.name, Retryable: true, Err: err}
	}
	return wire.DecodeResponse(out)
}

// ChatStream sends a canonical request as a streamed Messages call.
func (a *Adapter) ChatStream(ctx context.Context, req *schema.ChatRequest) (schema.Stream, error) {
	body, err := translated(req, true)
	if err != nil {
		return nil, err
	}
	resp, err := a.Native(ctx, &provider.NativeCall{Path: "/messages", Body: body, Header: headerFrom(ctx)}) //nolint:bodyclose // the stream closes it
	if err != nil {
		return nil, err
	}
	return &stream{body: resp.Body, r: sse.NewReader(resp.Body)}, nil
}

// Embed is not offered by Anthropic.
func (a *Adapter) Embed(context.Context, *schema.EmbedRequest) (*schema.EmbedResponse, error) {
	return nil, provider.ErrUnsupported
}

type stream struct {
	body    io.Closer
	r       *sse.Reader
	decoder wire.StreamDecoder
	pending []schema.Event
}

func (s *stream) Recv() (schema.Event, error) {
	for len(s.pending) == 0 {
		if s.decoder.Done() {
			return schema.Event{}, io.EOF
		}
		ev, err := s.r.Next()
		if errors.Is(err, io.EOF) {
			return schema.Event{}, io.ErrUnexpectedEOF
		}
		if err != nil {
			return schema.Event{}, err
		}
		if s.pending, err = s.decoder.Events(ev); err != nil {
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
