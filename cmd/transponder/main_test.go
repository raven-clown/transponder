package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/runtime"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/postgres"
	"github.com/raven-clown/transponder/internal/store/postgres/pgtest"
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

func TestServeFailsOnUnreachablePostgres(t *testing.T) {
	dsn := "postgres://u:secret@127.0.0.1:1/db?connect_timeout=2" //nolint:gosec // a made-up password, to check it stays out of the error
	cfg := &config.Bootstrap{Store: config.Store{Postgres: dsn}, LogLevel: "info"}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	err = serve(context.Background(), cfg, ln, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "opening postgres") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v, want it to say postgres could not be opened, without the password", err)
	}
}

// chat sends one chat call with key and returns the status.
func chat(t *testing.T, addr, key string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr+"/v1/chat/completions",
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
	return resp.StatusCode
}

// Two replicas on one Postgres share the admin key and the usage records.
func TestServeTwoReplicasOnPostgres(t *testing.T) {
	dsn := pgtest.DSN(t)
	listen := func() net.Listener {
		ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return ln
	}
	cfg := &config.Bootstrap{Store: config.Store{Postgres: dsn}, LogLevel: "info"}
	first := listen()
	key, stopFirst := startServe(t, cfg, first)
	defer stopFirst()

	// The second replica finds the admin key in the database and prints none of its own.
	second := listen()
	ctx, cancel := context.WithCancel(context.Background())
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, second, io.Discard, &stderr) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve returned %v", err)
		}
		if stderr.Len() != 0 {
			t.Errorf("the second replica printed %q", stderr.String())
		}
	}()

	for _, ln := range []net.Listener{first, second} {
		if status := chat(t, ln.Addr().String(), key); status != http.StatusOK {
			t.Fatalf("status %d from %s", status, ln.Addr())
		}
	}
	db, err := postgres.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	recs, err := db.RecentUsage(context.Background(), store.DefaultOrg, 10)
	if err != nil || len(recs) != 2 {
		t.Fatalf("usage records = %+v, %v, want one from each replica", recs, err)
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

// A setup with providers and no routes, as after the last route is deleted through the admin
// API, must not quietly answer every model from the free echo provider.
func TestBuildRouterProvidersWithoutRoutes(t *testing.T) {
	cfg := &config.Bootstrap{Providers: []config.Provider{{Name: "oa", Type: "openai", APIKey: "k"}}}
	r, err := buildRouter(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.Resolve("gpt-4o"); !errors.Is(err, router.ErrNoRoute) {
		t.Fatalf("resolved %+v, %v; want no route", got, err)
	}
	bare, err := buildRouter(&config.Bootstrap{}, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := bare.Resolve("anything"); err != nil || got.Type != "echo" {
		t.Fatalf("a bare start resolved %+v, %v; want echo", got, err)
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
	l, _, err := buildLimiter(ctx, cfg, db, slog.New(slog.DiscardHandler), func() time.Time { return now })
	if err != nil || l == nil {
		t.Fatalf("limiter %v, %v", l, err)
	}
	if _, d := l.Admit(ctx, limits.Caller{User: "alice"}, limits.Estimate{CostNUSD: 200_000_000}); d == nil {
		t.Fatal("this month's spend was not seeded")
	}
	if _, d := l.Admit(ctx, limits.Caller{User: "bob"}, limits.Estimate{CostNUSD: 200_000_000}); d != nil {
		t.Fatalf("bob denied: %+v", d)
	}
	none, _, err := buildLimiter(ctx, &config.Bootstrap{}, db, slog.New(slog.DiscardHandler), time.Now)
	if err != nil || none != nil {
		t.Fatalf("limiter without limits %v, %v", none, err)
	}
}

// With shared limits the first replica fills the counters from the usage table and the next
// one finds them filled, so last month's spend is not counted twice.
func TestBuildLimiterSharedSeedsOnce(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx := context.Background()
	db, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	now := time.Now()
	if err := db.RecordUsage(ctx, store.UsageRecord{OrgID: store.DefaultOrg, Time: now, User: "alice", CostNUSD: 600_000_000}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Bootstrap{Store: config.Store{Postgres: dsn}, Budgets: []config.BudgetConfig{{Subject: "user:*", Period: "monthly", LimitUSD: 1}}}
	var stops []func()
	defer func() {
		for _, stop := range stops {
			stop()
		}
	}()
	var replicas []*limits.Limiter
	for range 2 {
		l, stop, err := buildLimiter(ctx, cfg, db, slog.New(slog.DiscardHandler), func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		stops = append(stops, stop)
		replicas = append(replicas, l)
	}
	for _, l := range replicas {
		st := l.Status(ctx, limits.Caller{User: "alice"})
		if len(st) != 1 || st[0].Used != 600_000_000 {
			t.Fatalf("status %+v, want this month's spend counted once", st)
		}
	}
	if _, d := replicas[1].Admit(ctx, limits.Caller{User: "alice"}, limits.Estimate{CostNUSD: 500_000_000}); d == nil {
		t.Fatal("the second replica let alice past her budget")
	}

	cfg.Limits.Store = "memory"
	l, stop, err := buildLimiter(ctx, cfg, db, slog.New(slog.DiscardHandler), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if st := l.Status(ctx, limits.Caller{User: "alice"}); len(st) != 1 || st[0].Used != 600_000_000 {
		t.Fatalf("memory limits on Postgres: status %+v", st)
	}
}

// startReplica runs serve on ln for a replica that finds the admin key already made, and
// returns a function that stops it.
func startReplica(t *testing.T, cfg *config.Bootstrap, ln net.Listener) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, ln, io.Discard, io.Discard) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve returned %v", err)
		}
	}
}

// Two transponder replicas on one Postgres take calls for one user at the same time. Each call
// costs what it reserved, and together they must stay within the user's budget.
func TestServeTwoReplicasShareABudget(t *testing.T) {
	dsn := pgtest.DSN(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":500}}`)
	}))
	defer up.Close()
	const budgetUSD = 0.05
	cfg := &config.Bootstrap{
		Store:     config.Store{Postgres: dsn},
		LogLevel:  "error",
		Providers: []config.Provider{{Name: "oa", Type: "openai", BaseURL: up.URL, APIKey: "sk-test"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "oa"}}}},
		Budgets:   []config.BudgetConfig{{Subject: "user:*", Period: "monthly", LimitUSD: budgetUSD}},
	}
	listen := func() net.Listener {
		ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return ln
	}
	first, second := listen(), listen()
	key, stopFirst := startServe(t, cfg, first)
	defer stopFirst()
	defer startReplica(t, cfg, second)()

	var mu sync.Mutex
	statuses := map[int]int{}
	var wg sync.WaitGroup
	for i := range 40 {
		addr := []net.Listener{first, second}[i%2].Addr().String()
		wg.Go(func() {
			status, _ := postChat(t, addr, key, `{"model":"gpt-5","max_tokens":500,"messages":[{"role":"user","content":"ping"}]}`)
			mu.Lock()
			statuses[status]++
			mu.Unlock()
		})
	}
	wg.Wait()
	if statuses[http.StatusOK] == 0 || statuses[http.StatusPaymentRequired] == 0 || statuses[http.StatusOK]+statuses[http.StatusPaymentRequired] != 40 {
		t.Fatalf("statuses %v, want some calls served and the rest refused for the budget", statuses)
	}

	db, err := postgres.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	recs, err := db.RecentUsage(context.Background(), store.DefaultOrg, 100)
	if err != nil {
		t.Fatal(err)
	}
	var spent int64
	var served int
	for _, r := range recs {
		if r.Status == http.StatusOK {
			spent += r.CostNUSD
			served++
		}
	}
	if served != statuses[http.StatusOK] || spent > int64(budgetUSD*1e9) {
		t.Fatalf("%d calls spent %d nano-USD of a %v USD budget", served, spent, budgetUSD)
	}
}

// serveLogged runs serve on a fresh port and returns its log and a function that stops it.
func serveLogged(t *testing.T, cfg *config.Bootstrap) (*lockedBuffer, func()) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, ln, logs, io.Discard) }()
	return logs, func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve returned %v", err)
		}
	}
}

// waitFor polls until every log contains want, or fails after within.
func waitFor(t *testing.T, within time.Duration, want string, logs ...*lockedBuffer) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(within)
	for _, l := range logs {
		for !strings.Contains(l.String(), want) {
			select {
			case <-tick.C:
			case <-deadline:
				t.Fatalf("no %q within %s in:\n%s", want, within, l)
			}
		}
	}
}

// A write that raises config_version reaches every replica within the reload interval.
func TestReloadReachesEveryReplica(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		replicas int
		cfg      func(t *testing.T) *config.Bootstrap
		open     func(t *testing.T, cfg *config.Bootstrap) store.ConfigStore
	}{
		{"sqlite", 1, func(t *testing.T) *config.Bootstrap {
			return &config.Bootstrap{Store: config.Store{SQLite: filepath.Join(t.TempDir(), "t.db")}, LogLevel: "info"}
		}, func(t *testing.T, cfg *config.Bootstrap) store.ConfigStore {
			db, err := sqlite.Open(ctx, cfg.Store.SQLite)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return db
		}},
		{"postgres", 2, func(t *testing.T) *config.Bootstrap {
			return &config.Bootstrap{Store: config.Store{Postgres: pgtest.DSN(t)}, LogLevel: "info"}
		}, func(t *testing.T, cfg *config.Bootstrap) store.ConfigStore {
			db, err := postgres.Open(ctx, cfg.Store.Postgres)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return db
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg(t)
			var logs []*lockedBuffer
			for range tt.replicas {
				l, stop := serveLogged(t, cfg)
				defer stop()
				logs = append(logs, l)
			}
			waitFor(t, 10*time.Second, `"msg":"listening"`, logs...)
			db := tt.open(t, cfg)
			v, err := db.UpdateConfig(ctx, func(context.Context) error { return nil })
			if err != nil || v != 2 {
				t.Fatalf("UpdateConfig = %d, %v", v, err)
			}
			waitFor(t, runtime.DefaultInterval+time.Second, `"from_version":1,"version":2`, logs...)
		})
	}
}

func TestLoadLive(t *testing.T) {
	base := &limiterSource{base: limits.New(limits.Config{}, limits.NewMemoryStore(), time.Now)}
	cfg := &config.Bootstrap{Budgets: []config.BudgetConfig{{Subject: "user:*", Period: "monthly", LimitUSD: 1}}}
	l, err := loadLive(cfg, http.DefaultClient, base)
	if err != nil || l.router == nil || l.limiter == nil || len(l.limiter.Status(context.Background(), limits.Caller{User: "a"})) != 1 {
		t.Fatalf("live %+v, %v", l, err)
	}
	if l, err := loadLive(&config.Bootstrap{}, http.DefaultClient, &limiterSource{}); err != nil || l.limiter != nil {
		t.Fatalf("live without limits %+v, %v", l, err)
	}
	bad := &config.Bootstrap{Providers: []config.Provider{{Name: "p", Type: "nope"}}, Routes: []config.Route{{Match: "*", Targets: []config.Target{{Provider: "p"}}}}}
	if _, err := loadLive(bad, http.DefaultClient, &limiterSource{}); err == nil {
		t.Fatal("a config with an unknown provider type loaded")
	}
	badLimits := &config.Bootstrap{DefaultProfile: "gone"}
	if _, err := loadLive(badLimits, http.DefaultClient, base); err == nil {
		t.Fatal("a config with an unknown default profile loaded")
	}
	badAllow := &config.Bootstrap{Providers: []config.Provider{{Name: "p", Type: "echo"}}, Routes: []config.Route{{Match: "*", Allow: []string{"group:x"}, Targets: []config.Target{{Provider: "p"}}}}}
	if _, err := loadLive(badAllow, http.DefaultClient, &limiterSource{}); err == nil {
		t.Fatal("a route with an unknown allow entry loaded")
	}
}

// A reload after any admin write must not send traffic straight back to a target in cooldown.
func TestLoadLiveKeepsCooldowns(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "a", Type: "echo"}, {Name: "b", Type: "echo"}},
		Routes:    []config.Route{{Match: "*", Targets: []config.Target{{Provider: "a"}, {Provider: "b"}}}},
		Settings:  config.Settings{CooldownFailures: 2, Cooldown: time.Minute},
	}
	src := &limiterSource{}
	first, err := loadLive(cfg, http.DefaultClient, src)
	if err != nil {
		t.Fatal(err)
	}
	primary, _ := first.router.Resolve("m")
	first.router.Report(primary, false)
	first.router.Report(primary, false)
	next, err := loadLive(cfg, http.DefaultClient, src)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := next.router.Resolve("m"); got.Provider.Name() != "b" {
		t.Fatalf("after a reload the first target is %s, want b while a cools down", got.Provider.Name())
	}
}

// A reload builds the routes again from the config, so each route keeps its allow list.
func TestLoadLiveKeepsRouteAccess(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "p", Type: "echo"}},
		Routes: []config.Route{
			{Match: "private-*", Allow: []string{"team:ml", "role:admin"}, Targets: []config.Target{{Provider: "p"}}},
			{Match: "*", Targets: []config.Target{{Provider: "p"}}},
		},
	}
	for i := range 2 {
		l, err := loadLive(cfg, http.DefaultClient, &limiterSource{})
		if err != nil {
			t.Fatal(err)
		}
		private, err := l.router.Plan("private-1")
		if err != nil {
			t.Fatal(err)
		}
		if got := private.Access.Entries(); len(got) != 2 {
			t.Fatalf("load %d: allow %v, want team:ml and role:admin", i, got)
		}
		if private.Access.Permits(auth.Identity{User: "bob", Role: auth.RoleMember}) ||
			!private.Access.Permits(auth.Identity{User: "ann", Teams: []string{"ml"}, Role: auth.RoleMember}) {
			t.Fatalf("load %d: the allow list does not hold", i)
		}
		open, err := l.router.Plan("other")
		if err != nil || !open.Access.Open() {
			t.Fatalf("load %d: an open route got an allow list: %v %v", i, open.Access.Entries(), err)
		}
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

	badListenCfg := writeYAML("bad_listen.yaml", `
listen_addr: "8080"
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
		{
			name:       "listen address without a port fails and names the key",
			args:       []string{"--config-test", "--config", badListenCfg},
			wantCode:   1,
			wantStderr: "listen_addr \"8080\" must be host:port",
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
