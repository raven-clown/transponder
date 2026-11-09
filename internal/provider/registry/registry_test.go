package registry

import (
	"net/http"
	"sort"
	"testing"
)

func TestLookup(t *testing.T) {
	tests := []struct {
		typ          string
		needsBaseURL bool
		free         bool
	}{
		{typ: "echo", free: true},
		{typ: "openai"},
		{typ: "ollama", free: true},
		{typ: "litellm"},
		{typ: "cloudflare", needsBaseURL: true},
		{typ: "openrouter"},
		{typ: "anthropic"},
		{typ: "gemini"},
		{typ: "vllm", free: true},
		{typ: "llamacpp", free: true},
	}
	for _, tt := range tests {
		t.Run(tt.typ, func(t *testing.T) {
			s, ok := Lookup(tt.typ)
			if !ok {
				t.Fatal("not registered")
			}
			if s.Type != tt.typ || s.NeedsBaseURL != tt.needsBaseURL || s.Free != tt.free {
				t.Fatalf("spec %+v", s)
			}
			p := s.New(Settings{Name: "n", BaseURL: "http://localhost:1/v1", APIKey: "k", Client: http.DefaultClient})
			if p == nil || p.Name() != "n" {
				t.Fatalf("provider %v", p)
			}
		})
	}
	if _, ok := Lookup("bedrock"); ok {
		t.Fatal("bedrock should not be registered")
	}
}

func TestTypesSorted(t *testing.T) {
	types := Types()
	if len(types) < 5 || !sort.StringsAreSorted(types) {
		t.Fatalf("types %v", types)
	}
	for _, typ := range types {
		if _, ok := Lookup(typ); !ok {
			t.Fatalf("%s listed but not found", typ)
		}
	}
}
