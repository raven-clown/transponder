package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/sqlite"
)

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
		{name: "unknown flag", args: []string{"--nope"}, wantCode: 2},
		{name: "missing config file", args: []string{"--config", missing}, wantCode: 1, wantStderr: "nope.yaml"},
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
			if got.Provider.Name() != tt.provider || got.Type != tt.typ || got.Model != tt.upstream {
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
