// Package gemini is the adapter for the Gemini API with an API key.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	wire "github.com/raven-clown/transponder/internal/dialect/gemini"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/sse"
)

// DefaultBaseURL is the Generative Language API.
const DefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

const maxResponseBody = 64 << 20

// Adapter talks to Gemini. Every call is translated, since no caller speaks Gemini's format.
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

// Dialect returns "gemini".
func (a *Adapter) Dialect() string { return "gemini" }

// BaseURL returns the API root calls are sent to.
func (a *Adapter) BaseURL() string { return a.baseURL }

// path checks the model and returns the API path for one of its methods.
func path(model, method string) (string, error) {
	p, err := wire.ModelPath(model)
	if err != nil {
		return "", fmt.Errorf("%w: %w", provider.ErrUnsupported, err)
	}
	return p + ":" + method, nil
}

func (a *Adapter) send(ctx context.Context, path string, body []byte) (*http.Response, error) {
	h := http.Header{}
	h.Set("X-Goog-Api-Key", a.apiKey)
	from, ok := provider.HeaderFrom(ctx)
	if !ok {
		from = http.Header{}
	}
	return provider.Send(ctx, a.client, a.name, a.baseURL+"/"+path, body, h, from)
}

func (a *Adapter) read(ctx context.Context, path string, body []byte) ([]byte, error) {
	resp, err := a.send(ctx, path, body)
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

// Chat calls generateContent.
func (a *Adapter) Chat(ctx context.Context, req *schema.ChatRequest) (*schema.ChatResponse, error) {
	body, err := wire.EncodeRequest(req)
	if err != nil {
		return nil, err
	}
	p, err := path(req.Model, "generateContent")
	if err != nil {
		return nil, err
	}
	out, err := a.read(ctx, p, body)
	if err != nil {
		return nil, err
	}
	resp, err := wire.DecodeResponse(out)
	if err != nil {
		return nil, err
	}
	if resp.Model == "" {
		resp.Model = req.Model
	}
	return resp, nil
}

// ChatStream calls streamGenerateContent with server-sent events.
func (a *Adapter) ChatStream(ctx context.Context, req *schema.ChatRequest) (schema.Stream, error) {
	body, err := wire.EncodeRequest(req)
	if err != nil {
		return nil, err
	}
	p, err := path(req.Model, "streamGenerateContent?alt=sse")
	if err != nil {
		return nil, err
	}
	resp, err := a.send(ctx, p, body) //nolint:bodyclose // the stream closes it
	if err != nil {
		return nil, err
	}
	return &stream{body: resp.Body, r: sse.NewReader(resp.Body)}, nil
}

// Embed calls batchEmbedContents.
func (a *Adapter) Embed(ctx context.Context, req *schema.EmbedRequest) (*schema.EmbedResponse, error) {
	body, err := wire.EncodeEmbedRequest(req.Model, req.Input)
	if err != nil {
		return nil, err
	}
	p, err := path(req.Model, "batchEmbedContents")
	if err != nil {
		return nil, err
	}
	out, err := a.read(ctx, p, body)
	if err != nil {
		return nil, err
	}
	return wire.DecodeEmbedResponse(out, req.Model, req.Input)
}

type stream struct {
	body    io.Closer
	r       *sse.Reader
	decoder wire.StreamDecoder
	pending []schema.Event
}

func (s *stream) Recv() (schema.Event, error) {
	for len(s.pending) == 0 {
		ev, err := s.r.Next()
		if errors.Is(err, io.EOF) {
			if s.decoder.Done() {
				return schema.Event{}, io.EOF
			}
			return schema.Event{}, io.ErrUnexpectedEOF
		}
		if err != nil {
			return schema.Event{}, err
		}
		if s.pending, err = s.decoder.Events(ev.Data); err != nil {
			return schema.Event{}, err
		}
	}
	ev := s.pending[0]
	s.pending = s.pending[1:]
	return ev, nil
}

func (s *stream) Close() error { return s.body.Close() }

var _ provider.Provider = (*Adapter)(nil)
