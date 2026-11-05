package router

import (
	"errors"
	"testing"

	"github.com/raven-clown/transponder/internal/provider/echo"
)

func TestResolve(t *testing.T) {
	a, b := echo.New("a"), echo.New("b")
	r := New([]Route{
		{Match: "fast", Targets: []Target{{Provider: a, Type: "echo", Model: "small"}}},
		{Match: "gpt-*", Targets: []Target{{Provider: b, Type: "openai"}, {Provider: a, Type: "echo"}}},
		{Match: "gpt-4o", Targets: []Target{{Provider: a, Type: "echo"}}},
	})
	tests := []struct {
		model    string
		route    string
		provider string
		typ      string
		upstream string
	}{
		{"fast", "fast", "a", "echo", "small"},
		{"gpt-5", "gpt-*", "b", "openai", "gpt-5"},
		{"gpt-4o", "gpt-*", "b", "openai", "gpt-4o"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got, err := r.Resolve(tt.model)
			if err != nil {
				t.Fatal(err)
			}
			if got.Route != tt.route || got.Provider.Name() != tt.provider || got.Type != tt.typ || got.Model != tt.upstream {
				t.Fatalf("Resolve(%q) = %+v", tt.model, got)
			}
		})
	}
	if _, err := r.Resolve("claude-x"); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("Resolve(claude-x) err = %v", err)
	}
}

func TestBadGlobNeverMatches(t *testing.T) {
	r := New([]Route{{Match: "a[", Targets: []Target{{Provider: echo.New("a"), Type: "echo"}}}})
	if _, err := r.Resolve("ab"); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("err = %v", err)
	}
}
