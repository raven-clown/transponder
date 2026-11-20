package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/sqlite"
)

func TestVersionString(t *testing.T) {
	vcs := func(settings map[string]string) *debug.BuildInfo {
		info := &debug.BuildInfo{}
		for k, v := range settings {
			info.Settings = append(info.Settings, debug.BuildSetting{Key: k, Value: v})
		}
		return info
	}
	const rev, when = "1a2b3c4d5e6f7a8b9c0d", "2026-11-04T10:00:00Z"
	tests := []struct {
		name    string
		version string
		info    *debug.BuildInfo
		ok      bool
		want    string
	}{
		{"full info", "dev", vcs(map[string]string{"vcs.revision": rev, "vcs.time": when, "vcs.modified": "false"}), true,
			"transponder dev (1a2b3c4, 2026-11-04T10:00:00Z)"},
		{"modified tree", "dev", vcs(map[string]string{"vcs.revision": rev, "vcs.time": when, "vcs.modified": "true"}), true,
			"transponder dev (1a2b3c4, 2026-11-04T10:00:00Z, modified)"},
		{"short revision is kept", "dev", vcs(map[string]string{"vcs.revision": "abc", "vcs.time": when}), true,
			"transponder dev (abc, 2026-11-04T10:00:00Z)"},
		{"no vcs settings", "dev", vcs(map[string]string{"GOOS": "linux"}), true, "transponder dev"},
		{"no revision", "dev", vcs(map[string]string{"vcs.time": when}), true, "transponder dev"},
		{"no build info", "dev", nil, false, "transponder dev"},
		{"ok false ignores info", "dev", vcs(map[string]string{"vcs.revision": rev, "vcs.time": when}), false, "transponder dev"},
		{"release version", "v1.2.3", vcs(map[string]string{"vcs.revision": rev, "vcs.time": when}), true,
			"transponder v1.2.3 (1a2b3c4, 2026-11-04T10:00:00Z)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := versionString(tt.version, tt.info, tt.ok); got != tt.want {
				t.Fatalf("versionString = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunVersionShowsBuildInfo(t *testing.T) {
	orig := readBuildInfo
	t.Cleanup(func() { readBuildInfo = orig })
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "1a2b3c4d5e6f"},
			{Key: "vcs.time", Value: "2026-11-04T10:00:00Z"},
		}}, true
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr.String())
	}
	if got, want := stdout.String(), "transponder dev (1a2b3c4, 2026-11-04T10:00:00Z)\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRun(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version flag", args: []string{"--version"}, wantCode: 0, wantStdout: "transponder dev"},
		{name: "config-test defaults", args: []string{"--config-test"}, wantCode: 0, wantStdout: "transponder: config ok"},
		{name: "unknown flag", args: []string{"--nope"}, wantCode: 2},
		{name: "missing config file", args: []string{"--config", missing}, wantCode: 1, wantStderr: "nope.yaml"},
		{name: "config-test missing config file", args: []string{"--config-test", "--config", missing}, wantCode: 1, wantStderr: "nope.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Fatalf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestServeRejectsPostgres(t *testing.T) {
	cfg := &config.Bootstrap{Store: config.Store{Postgres: "postgres://x"}}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	err = serve(context.Background(), cfg, ln, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "postgres store is not supported yet") {
		t.Fatalf("err = %v", err)
	}
}

// startServe runs serve on ln and returns the printed admin key and a function that stops it.
func startServe(t *testing.T, cfg *config.Bootstrap, ln net.Listener) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, cfg, ln, io.Discard, pw)
		_ = pw.Close()
	}()
	keys := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "tp_") {
				keys <- sc.Text()
			}
		}
	}()
	var key string
	select {
	case key = <-keys:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("no admin key printed")
	}
	return key, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("serve did not stop")
		}
	}
}

func TestServeEndToEnd(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Bootstrap{ListenAddr: ln.Addr().String(), Store: config.Store{SQLite: filepath.Join(t.TempDir(), "t.db")}, LogLevel: "info"}
	key, stop := startServe(t, cfg, ln)
	defer stop()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+ln.Addr().String()+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Usage-Cost-USD") == "" {
		t.Fatalf("status %d, headers %v", resp.StatusCode, resp.Header)
	}
}

func TestBuildRouter(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.Bootstrap
		model    string
		provider string
		typ      string
		upstream string
	}{
		{name: "no providers", model: "anything", provider: "echo", typ: "echo", upstream: "anything"},
		{
			name: "openai route",
			cfg: config.Bootstrap{
				Providers: []config.Provider{{Name: "oa", Type: "openai", APIKey: "k"}},
				Routes:    []config.Route{{Match: "fast", Targets: []config.Target{{Provider: "oa", Model: "gpt-4o-mini"}}}},
			},
			model: "fast", provider: "oa", typ: "openai", upstream: "gpt-4o-mini",
		},
		{
			name: "ollama route",
			cfg: config.Bootstrap{
				Providers: []config.Provider{{Name: "local", Type: "ollama"}, {Name: "e", Type: "echo"}},
				Routes:    []config.Route{{Match: "llama*", Targets: []config.Target{{Provider: "local"}}}, {Match: "*", Targets: []config.Target{{Provider: "e"}}}},
			},
			model: "llama3.2", provider: "local", typ: "ollama", upstream: "llama3.2",
		},
		{
			name: "cloudflare route",
			cfg: config.Bootstrap{
				Providers: []config.Provider{{Name: "cf", Type: "cloudflare", BaseURL: "https://gateway.ai.cloudflare.com/v1/account/gateway/compat", APIKey: "token"}},
				Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "cf"}}}},
			},
			model: "m", provider: "cf", typ: "cloudflare", upstream: "m",
		},
		{
			name: "litellm route",
			cfg: config.Bootstrap{
				Providers: []config.Provider{{Name: "llm", Type: "litellm", APIKey: "sk-litellm"}},
				Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "llm"}}}},
			},
			model: "m", provider: "llm", typ: "litellm", upstream: "m",
		},
		{
			name: "vllm route",
			cfg: config.Bootstrap{
				Providers: []config.Provider{{Name: "v", Type: "vllm", APIKey: "sk-vllm"}},
				Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "v"}}}},
			},
			model: "m", provider: "v", typ: "vllm", upstream: "m",
		},
		{
			name: "llamacpp route",
			cfg: config.Bootstrap{
				Providers: []config.Provider{{Name: "l", Type: "llamacpp", APIKey: "k"}},
				Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "l"}}}},
			},
			model: "m", provider: "l", typ: "llamacpp", upstream: "m",
		},
		{
			name: "xai route",
			cfg: config.Bootstrap{
				Providers: []config.Provider{{Name: "x", Type: "xai", APIKey: "xai-test"}},
				Routes:    []config.Route{{Match: "grok-*", Targets: []config.Target{{Provider: "x"}}}},
			},
			model: "grok-4.7", provider: "x", typ: "xai", upstream: "grok-4.7",
		},
		{
			name: "vercel route",
			cfg: config.Bootstrap{
				Providers: []config.Provider{{Name: "v", Type: "vercel", APIKey: "k"}},
				Routes:    []config.Route{{Match: "anthropic/*", Targets: []config.Target{{Provider: "v"}}}},
			},
			model: "anthropic/claude-opus-5", provider: "v", typ: "vercel", upstream: "anthropic/claude-opus-5",
		},
		{
			name: "mistral route",
			cfg: config.Bootstrap{
				Providers: []config.Provider{{Name: "mistral", Type: "mistral", APIKey: "k"}},
				Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "mistral"}}}},
			},
			model: "m", provider: "mistral", typ: "mistral", upstream: "m",
		},
		{
			name: "nvidia route",
			cfg: config.Bootstrap{
				Providers: []config.Provider{{Name: "nvidia", Type: "nvidia", APIKey: "k"}},
				Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "nvidia"}}}},
			},
			model: "m", provider: "nvidia", typ: "nvidia", upstream: "m",
		},
		{
			name: "groq route",
			cfg: config.Bootstrap{
				Providers: []config.Provider{{Name: "groq", Type: "groq", APIKey: "k"}},
				Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "groq"}}}},
			},
			model: "m", provider: "groq", typ: "groq", upstream: "m",
		},
		{
			name: "cerebras route",
			cfg: config.Bootstrap{
				Providers: []config.Provider{{Name: "cerebras", Type: "cerebras", APIKey: "k"}},
				Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "cerebras"}}}},
			},
			model: "m", provider: "cerebras", typ: "cerebras", upstream: "m",
		},
		{
			name: "siliconflow route",
			cfg: config.Bootstrap{
				Providers: []config.Provider{{Name: "siliconflow", Type: "siliconflow", APIKey: "k"}},
				Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "siliconflow"}}}},
			},
			model: "m", provider: "siliconflow", typ: "siliconflow", upstream: "m",
		},
		{
			name: "lmstudio route",
			cfg: config.Bootstrap{
				Providers: []config.Provider{{Name: "lmstudio", Type: "lmstudio", APIKey: "k"}},
				Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "lmstudio"}}}},
			},
			model: "m", provider: "lmstudio", typ: "lmstudio", upstream: "m",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := buildRouter(&tt.cfg, http.DefaultClient)
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.Resolve(tt.model)
			if err != nil {
				t.Fatal(err)
			}
			if got.Provider.Name() != tt.provider || got.Type != tt.typ || got.Model != tt.upstream || got.Free != (tt.typ == "echo" || tt.typ == "ollama" || tt.typ == "vllm" || tt.typ == "llamacpp" || tt.typ == "lmstudio") {
				t.Fatalf("resolved %+v", got)
			}
		})
	}
}

func TestBuildRouterOllamaDefault(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "local", Type: "ollama"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "local"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("x")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok || a.BaseURL() != "http://localhost:11434/v1" {
		t.Fatalf("provider %T", got.Provider)
	}
}

func TestBuildRouterLiteLLMDefault(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "local", Type: "litellm"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "local"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("x")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok || a.BaseURL() != "http://localhost:4000/v1" {
		t.Fatalf("provider %T", got.Provider)
	}
}

func TestBuildRouterVLLMDefault(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "local", Type: "vllm"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "local"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("x")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok || a.BaseURL() != "http://localhost:8000/v1" {
		t.Fatalf("provider %T", got.Provider)
	}
}

func TestBuildRouterXAIDefault(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "xai", Type: "xai", APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "xai"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("grok-4.7")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok {
		t.Fatalf("provider %T", got.Provider)
	}
	if a.BaseURL() != "https://api.x.ai/v1" || got.Free {
		t.Fatalf("base %q free=%v", a.BaseURL(), got.Free)
	}
}

func TestBuildRouterVercelDefault(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "vercel", Type: "vercel", APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "vercel"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("anthropic/claude-opus-5")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok {
		t.Fatalf("provider %T", got.Provider)
	}
	if a.BaseURL() != "https://ai-gateway.vercel.sh/v1" || got.Free {
		t.Fatalf("base %q free=%v", a.BaseURL(), got.Free)
	}
}

func TestBuildRouterVercelBaseURLOverride(t *testing.T) {
	const custom = "https://gateway.example/v1"
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "vercel", Type: "vercel", BaseURL: custom, APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "vercel"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("anthropic/claude-opus-5")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok || a.BaseURL() != custom {
		t.Fatalf("provider %T", got.Provider)
	}
}

func TestBuildRouterLmstudioDefault(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "lmstudio", Type: "lmstudio", APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "lmstudio"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("m")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok {
		t.Fatalf("provider %T", got.Provider)
	}
	if a.BaseURL() != "http://localhost:1234/v1" || !got.Free {
		t.Fatalf("base %q free=%v", a.BaseURL(), got.Free)
	}
}

func TestBuildRouterLmstudioBaseURLOverride(t *testing.T) {
	const custom = "http://gateway.example/v1"
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "lmstudio", Type: "lmstudio", BaseURL: custom, APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "lmstudio"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("m")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok || a.BaseURL() != custom {
		t.Fatalf("provider %T", got.Provider)
	}
}

func TestBuildRouterMistralDefault(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "mistral", Type: "mistral", APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "mistral"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("m")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok {
		t.Fatalf("provider %T", got.Provider)
	}
	if a.BaseURL() != "https://api.mistral.ai/v1" || got.Free {
		t.Fatalf("base %q free=%v", a.BaseURL(), got.Free)
	}
}

func TestBuildRouterMistralBaseURLOverride(t *testing.T) {
	const custom = "https://gateway.example/v1"
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "mistral", Type: "mistral", BaseURL: custom, APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "mistral"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("m")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok || a.BaseURL() != custom {
		t.Fatalf("provider %T", got.Provider)
	}
}

func TestBuildRouterNvidiaDefault(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "nvidia", Type: "nvidia", APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "nvidia"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("m")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok {
		t.Fatalf("provider %T", got.Provider)
	}
	if a.BaseURL() != "https://integrate.api.nvidia.com/v1" || got.Free {
		t.Fatalf("base %q free=%v", a.BaseURL(), got.Free)
	}
}

func TestBuildRouterNvidiaBaseURLOverride(t *testing.T) {
	const custom = "https://gateway.example/v1"
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "nvidia", Type: "nvidia", BaseURL: custom, APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "nvidia"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("m")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok || a.BaseURL() != custom {
		t.Fatalf("provider %T", got.Provider)
	}
}

func TestBuildRouterGroqDefault(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "groq", Type: "groq", APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "groq"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("m")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok {
		t.Fatalf("provider %T", got.Provider)
	}
	if a.BaseURL() != "https://api.groq.com/openai/v1" || got.Free {
		t.Fatalf("base %q free=%v", a.BaseURL(), got.Free)
	}
}

func TestBuildRouterGroqBaseURLOverride(t *testing.T) {
	const custom = "https://gateway.example/openai/v1"
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "groq", Type: "groq", BaseURL: custom, APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "groq"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("m")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok || a.BaseURL() != custom {
		t.Fatalf("provider %T", got.Provider)
	}
}

func TestBuildRouterCerebrasDefault(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "cerebras", Type: "cerebras", APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "cerebras"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("m")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok {
		t.Fatalf("provider %T", got.Provider)
	}
	if a.BaseURL() != "https://api.cerebras.ai/v1" || got.Free {
		t.Fatalf("base %q free=%v", a.BaseURL(), got.Free)
	}
}

func TestBuildRouterCerebrasBaseURLOverride(t *testing.T) {
	const custom = "https://gateway.example/v1"
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "cerebras", Type: "cerebras", BaseURL: custom, APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "cerebras"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("m")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok || a.BaseURL() != custom {
		t.Fatalf("provider %T", got.Provider)
	}
}

func TestBuildRouterSiliconflowDefault(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "siliconflow", Type: "siliconflow", APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "siliconflow"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("m")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok {
		t.Fatalf("provider %T", got.Provider)
	}
	if a.BaseURL() != "https://api.siliconflow.com/v1" || got.Free {
		t.Fatalf("base %q free=%v", a.BaseURL(), got.Free)
	}
}

func TestBuildRouterSiliconflowBaseURLOverride(t *testing.T) {
	const custom = "https://gateway.example/v1"
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "siliconflow", Type: "siliconflow", BaseURL: custom, APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "siliconflow"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("m")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok || a.BaseURL() != custom {
		t.Fatalf("provider %T", got.Provider)
	}
}

func TestBuildRouterXAIBaseURLOverride(t *testing.T) {
	const custom = "https://us.api.x.ai/v1"
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "xai", Type: "xai", BaseURL: custom, APIKey: "k"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "xai"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("grok-4.7")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok || a.BaseURL() != custom {
		t.Fatalf("provider %T", got.Provider)
	}
}

func TestBuildRouterLlamaCppDefault(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "local", Type: "llamacpp"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "local"}}}},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("x")
	a, ok := got.Provider.(interface{ BaseURL() string })
	if !ok || a.BaseURL() != "http://localhost:8080/v1" {
		t.Fatalf("provider %T", got.Provider)
	}
}

func TestServeWithOpenAIProvider(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("authorization %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500}}`)
	}))
	defer up.Close()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Bootstrap{
		Store:     config.Store{SQLite: filepath.Join(t.TempDir(), "t.db")},
		LogLevel:  "info",
		Providers: []config.Provider{{Name: "oa", Type: "openai", BaseURL: up.URL, APIKey: "sk-test"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "oa"}}}},
	}
	key, stop := startServe(t, cfg, ln)
	defer stop()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+ln.Addr().String()+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Usage-Cost-USD") != "0.000450" {
		t.Fatalf("status %d, headers %v", resp.StatusCode, resp.Header)
	}
}

func TestServeRecordsOpenStreamOnShutdown(t *testing.T) {
	old := shutdownTimeout
	shutdownTimeout = 200 * time.Millisecond
	defer func() { shutdownTimeout = old }()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer up.Close()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "t.db")
	cfg := &config.Bootstrap{
		Store:     config.Store{SQLite: dbPath},
		LogLevel:  "info",
		Providers: []config.Provider{{Name: "oa", Type: "openai", BaseURL: up.URL}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "oa"}}}},
	}
	key, stop := startServe(t, cfg, ln)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+ln.Addr().String()+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	stop()

	db, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	recs, err := db.RecentUsage(context.Background(), store.DefaultOrg, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || !recs[0].Estimated || recs[0].OutputTokens != 2 {
		t.Fatalf("records %+v", recs)
	}
}

func TestStopOnContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	called := make(chan struct{}, 1)
	go stopOnContextDone(ctx, func() { called <- struct{}{} })
	cancel()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("stop was not called after context cancellation")
	}
}

func timeoutConfig(t *testing.T, upURL string) *config.Bootstrap {
	return &config.Bootstrap{
		Store:     config.Store{SQLite: filepath.Join(t.TempDir(), "t.db")},
		LogLevel:  "info",
		Providers: []config.Provider{{Name: "oa", Type: "openai", BaseURL: upURL}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "oa"}}}},
		Settings:  config.Settings{AttemptTimeout: 150 * time.Millisecond, RequestTimeout: time.Minute},
	}
}

func postChat(t *testing.T, addr, key, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

func TestAttemptTimeout(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer up.Close()
	defer close(release)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	key, stop := startServe(t, timeoutConfig(t, up.URL), ln)
	defer stop()
	start := time.Now()
	status, body := postChat(t, ln.Addr().String(), key, `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`)
	if status != http.StatusBadGateway || time.Since(start) > 10*time.Second {
		t.Fatalf("status %d after %s: %s", status, time.Since(start), body)
	}
}

func TestSlowStreamNotCut(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for i := range 4 {
			_, _ = io.WriteString(w, fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"%d\"}}]}\n\n", i))
			w.(http.Flusher).Flush()
			<-tick.C
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	key, stop := startServe(t, timeoutConfig(t, up.URL), ln)
	defer stop()
	status, body := postChat(t, ln.Addr().String(), key, `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`)
	if status != http.StatusOK || !strings.HasSuffix(string(body), "data: [DONE]\n\n") || strings.Contains(string(body), "upstream_error") {
		t.Fatalf("status %d: %s", status, body)
	}
}

func TestBuildLimiterSeedsFromUsage(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	now := time.Now()
	if err := db.RecordUsage(ctx, store.UsageRecord{OrgID: store.DefaultOrg, Time: now, User: "alice", CostNUSD: 900_000_000}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Bootstrap{Budgets: []config.BudgetConfig{{Subject: "user:*", Period: "monthly", LimitUSD: 1}}}
	l, err := buildLimiter(ctx, cfg, db, func() time.Time { return now })
	if err != nil || l == nil {
		t.Fatalf("limiter %v, %v", l, err)
	}
	if _, d := l.Admit(ctx, limits.Caller{User: "alice"}, limits.Estimate{CostNUSD: 200_000_000}); d == nil {
		t.Fatal("this month's spend was not seeded")
	}
	if _, d := l.Admit(ctx, limits.Caller{User: "bob"}, limits.Estimate{CostNUSD: 200_000_000}); d != nil {
		t.Fatalf("bob denied: %+v", d)
	}
	none, err := buildLimiter(ctx, &config.Bootstrap{}, db, time.Now)
	if err != nil || none != nil {
		t.Fatalf("limiter without limits %v, %v", none, err)
	}
}

func TestBuildRouterCooldownSettings(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "a", Type: "echo"}, {Name: "b", Type: "echo"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "a"}, {Provider: "b"}}}},
		Settings:  config.Settings{CooldownFailures: 1, Cooldown: time.Hour},
	}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := r.Plan("m")
	if len(p.Targets) != 2 || p.Targets[0].Provider.Name() != "a" {
		t.Fatalf("plan %+v", p)
	}
	r.Report(p.Targets[0], false)
	if p, _ = r.Plan("m"); p.Targets[0].Provider.Name() != "b" {
		t.Fatalf("one failure did not cool the target with cooldown_failures 1: %+v", p)
	}
}

func TestConfigTest(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "transponder.db")

	writeYAML := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	validCfg := writeYAML("valid.yaml", fmt.Sprintf(`
listen_addr: ":8080"
store:
  sqlite: %q
providers:
  - name: my-echo
    type: echo
routes:
  - match: "*"
    targets:
      - provider: my-echo
`, dbPath))

	unknownProviderTypeCfg := writeYAML("unknown_type.yaml", `
providers:
  - name: p1
    type: nonexistent_type
routes:
  - match: "*"
    targets:
      - provider: p1
`)

	missingProviderCfg := writeYAML("missing_provider.yaml", `
providers:
  - name: p1
    type: echo
routes:
  - match: "*"
    targets:
      - provider: p2
`)

	badEnvCfg := writeYAML("bad_env.yaml", `
listen_addr: ${UNDEFINED_TRANSPONDER_VAR}
`)

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "valid config succeeds and prints ok",
			args:       []string{"--config-test", "--config", validCfg},
			wantCode:   0,
			wantStdout: "transponder: config ok\n",
		},
		{
			name:       "unknown provider type fails validation",
			args:       []string{"--config-test", "--config", unknownProviderTypeCfg},
			wantCode:   1,
			wantStderr: "unknown type",
		},
		{
			name:       "route pointing to missing provider fails",
			args:       []string{"--config-test", "--config", missingProviderCfg},
			wantCode:   1,
			wantStderr: "uses provider \"p2\", which is not declared",
		},
		{
			name:       "bad env expansion in config fails",
			args:       []string{"--config-test", "--config", badEnvCfg},
			wantCode:   1,
			wantStderr: "UNDEFINED_TRANSPONDER_VAR",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr.String())
			}
			if tt.wantStdout != "" && !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Fatalf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}

	// Verify that --config-test never created the database file
	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database file %s was created by --config-test, want it not to exist", dbPath)
	}
}
