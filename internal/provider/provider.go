// Package provider defines the interface every backend adapter implements.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/raven-clown/transponder/internal/schema"
)

// ErrUnsupported means the backend cannot do the requested operation.
var ErrUnsupported = errors.New("provider: operation not supported")

// Provider is one configured backend.
type Provider interface {
	Name() string
	Dialect() string
	Chat(ctx context.Context, req *schema.ChatRequest) (*schema.ChatResponse, error)
	ChatStream(ctx context.Context, req *schema.ChatRequest) (schema.Stream, error)
	Embed(ctx context.Context, req *schema.EmbedRequest) (*schema.EmbedResponse, error)
}

// NativeCall is a request forwarded in the provider's own wire format.
type NativeCall struct {
	Path   string
	Body   []byte
	Header http.Header
}

// Native is implemented by adapters that can forward a caller's body unchanged.
type Native interface {
	Native(ctx context.Context, call *NativeCall) (*http.Response, error)
}

// Error is a failed upstream call. Status is 0 when no response arrived.
type Error struct {
	Provider  string
	Status    int
	Retryable bool
	Body      []byte
	Err       error
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("provider %s: %v", e.Provider, e.Err)
	}
	return fmt.Sprintf("provider %s: status %d: %v", e.Provider, e.Status, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// RetryableStatus reports whether another target may succeed where this status failed.
// 401 and 403 count, since they mean the gateway's provider key is bad.
func RetryableStatus(status int) bool {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden,
		status == http.StatusRequestTimeout, status == http.StatusTooManyRequests:
		return true
	case status >= 500:
		return true
	}
	return false
}

type headerKey struct{}

// WithHeader returns a context carrying the headers to forward upstream, such as
// X-Request-ID and traceparent, for adapters called through the canonical methods.
func WithHeader(ctx context.Context, h http.Header) context.Context {
	return context.WithValue(ctx, headerKey{}, h)
}

// HeaderFrom returns the headers stored by WithHeader.
func HeaderFrom(ctx context.Context) (http.Header, bool) {
	h, ok := ctx.Value(headerKey{}).(http.Header)
	return h, ok
}
