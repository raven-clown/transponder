package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
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
