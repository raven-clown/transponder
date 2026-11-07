// Package registry lists every provider type transponder can build. Adding a provider is
// a package under internal/provider and one entry in specs below.
package registry

import (
	"net/http"
	"sort"

	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/provider/cloudflare"
	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/provider/litellm"
	"github.com/raven-clown/transponder/internal/provider/ollama"
	"github.com/raven-clown/transponder/internal/provider/openai"
	"github.com/raven-clown/transponder/internal/provider/openrouter"
	"github.com/raven-clown/transponder/internal/provider/vllm"
)

// Settings are the values a provider is built from.
type Settings struct {
	Name    string
	BaseURL string
	APIKey  string
	Client  *http.Client
}

// Spec describes one provider type.
type Spec struct {
	Type         string
	NeedsBaseURL bool
	Free         bool
	New          func(Settings) provider.Provider
}

var specs = []Spec{
	{Type: "echo", Free: true, New: func(s Settings) provider.Provider { return echo.New(s.Name) }},
	{Type: "openai", New: func(s Settings) provider.Provider {
		return openai.New(openai.Options{Name: s.Name, BaseURL: s.BaseURL, APIKey: s.APIKey, Client: s.Client})
	}},
	{Type: "ollama", Free: true, New: func(s Settings) provider.Provider {
		return ollama.New(s.Name, s.BaseURL, s.APIKey, s.Client)
	}},
	{Type: "litellm", New: func(s Settings) provider.Provider {
		return litellm.New(s.Name, s.BaseURL, s.APIKey, s.Client)
	}},
	{Type: "cloudflare", NeedsBaseURL: true, New: func(s Settings) provider.Provider {
		return cloudflare.New(s.Name, s.BaseURL, s.APIKey, s.Client)
	}},
	{Type: "openrouter", New: func(s Settings) provider.Provider {
		return openrouter.New(s.Name, s.BaseURL, s.APIKey, s.Client)
	}},
	{Type: "vllm", Free: true, New: func(s Settings) provider.Provider {
		return vllm.New(s.Name, s.BaseURL, s.APIKey, s.Client)
	}},
}

// Lookup returns the spec for a provider type.
func Lookup(typ string) (Spec, bool) {
	for _, s := range specs {
		if s.Type == typ {
			return s, true
		}
	}
	return Spec{}, false
}

// Types returns the registered provider types in order.
func Types() []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Type
	}
	sort.Strings(out)
	return out
}
