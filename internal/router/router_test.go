package router

import (
	"errors"
	"reflect"
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

func TestGlobMatchesSlashes(t *testing.T) {
	e := echo.New("a")
	r := New([]Route{
		{Match: "hf.co/*", Targets: []Target{{Provider: e, Type: "echo", Model: "hf"}}},
		{Match: "gpt-?", Targets: []Target{{Provider: e, Type: "echo", Model: "one"}}},
		{Match: "*", Targets: []Target{{Provider: e, Type: "echo"}}},
	})
	for model, want := range map[string]string{
		"hf.co/user/repo":                  "hf",
		"gpt-5":                            "one",
		"meta-llama/Llama-3.1-8B-Instruct": "meta-llama/Llama-3.1-8B-Instruct",
		"gpt-5.1":                          "gpt-5.1",
		"a.b+c(d)":                         "a.b+c(d)",
	} {
		got, err := r.Resolve(model)
		if err != nil || got.Model != want {
			t.Errorf("Resolve(%q) = %+v, %v; want model %q", model, got, err, want)
		}
	}
}

func TestBadGlobNeverMatches(t *testing.T) {
	r := New([]Route{{Match: "a[", Targets: []Target{{Provider: echo.New("a"), Type: "echo"}}}})
	if _, err := r.Resolve("ab"); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("err = %v", err)
	}
}

func TestModels(t *testing.T) {
	e := echo.New("a")
	r := New([]Route{
		{Match: "fast", Targets: []Target{{Provider: e, Model: "gpt-4o-mini"}}},
		{Match: "gpt-*", Targets: []Target{{Provider: e}, {Provider: e, Model: "llama3.2"}}},
		{Match: "embed", Targets: []Target{{Provider: e}}},
		{Match: "fast", Targets: []Target{{Provider: e, Model: "gpt-4o-mini"}}},
		{Match: "q?", Targets: []Target{{Provider: e}}},
	})
	got := r.Models()
	want := []string{"embed", "fast", "gpt-4o-mini", "llama3.2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Models() = %v, want %v", got, want)
	}
	if got := New([]Route{{Match: "*", Targets: []Target{{Provider: e}}}}).Models(); len(got) != 0 {
		t.Fatalf("catch-all lists %v", got)
	}
}
