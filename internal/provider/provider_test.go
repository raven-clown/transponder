package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/schema"
)

func TestRetryableStatus(t *testing.T) {
	for status, want := range map[int]bool{
		200: false, 400: false, 404: false, 422: false,
		401: true, 403: true, 408: true, 429: true, 500: true, 503: true, 529: true,
	} {
		if got := RetryableStatus(status); got != want {
			t.Errorf("RetryableStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

func TestErrorMessage(t *testing.T) {
	cause := errors.New("boom")
	err := &Error{Provider: "openai", Status: 503, Err: cause}
	if !strings.Contains(err.Error(), "openai") || !strings.Contains(err.Error(), "503") {
		t.Fatalf("message %q", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Fatal("errors.Is does not reach the cause")
	}
	conn := &Error{Provider: "local", Err: cause}
	if !strings.Contains(conn.Error(), "boom") {
		t.Fatalf("message %q", conn.Error())
	}
}

func TestHeaderContext(t *testing.T) {
	if _, ok := HeaderFrom(context.Background()); ok {
		t.Fatal("bare context has headers")
	}
	h := http.Header{"X-Request-Id": {"r1"}}
	got, ok := HeaderFrom(WithHeader(context.Background(), h))
	if !ok || got.Get("X-Request-ID") != "r1" {
		t.Fatalf("headers %v", got)
	}
}

func TestCannotCarry(t *testing.T) {
	plain := errors.New("boom")
	if got := CannotCarry(plain); got != plain { //nolint:errorlint // the same value must come back, not a wrap of it
		t.Fatalf("a plain error came back as %v", got)
	}
	if CannotCarry(nil) != nil {
		t.Fatal("nil came back as an error")
	}
	err := CannotCarry(fmt.Errorf("encoding: %w", &schema.ContentError{Reason: "no images at a URL"}))
	var ce *schema.ContentError
	if !errors.Is(err, ErrUnsupported) || !errors.As(err, &ce) || ce.Reason != "no images at a URL" {
		t.Fatalf("err = %v", err)
	}
}
