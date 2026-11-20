package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/update"
)

// fakeReleases serves v0.9.0 with a manifest whose only change touches ollama.
func fakeReleases(t *testing.T) *update.Checker {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/raven-clown/transponder/releases":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"tag_name": "v0.9.0", "html_url": "https://example.test/v0.9.0",
				"assets": []map[string]string{{"name": update.ManifestName, "browser_download_url": srv.URL + "/m"}},
			}})
		case "/m":
			_ = json.NewEncoder(w).Encode(update.Manifest{Version: "v0.9.0", Changes: []update.Change{
				{Kind: update.KindFixed, Text: "Ollama keys are sent.", PR: 75, Affects: []string{"provider:ollama"}},
			}})
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return &update.Checker{Repo: "raven-clown/transponder", APIBase: srv.URL, Client: srv.Client()}
}

func withVersion(t *testing.T, v string) {
	t.Helper()
	orig := version
	version = v
	t.Cleanup(func() { version = orig })
}

func withChecker(t *testing.T, c *update.Checker) {
	t.Helper()
	orig := updateChecker
	updateChecker = c
	t.Cleanup(func() { updateChecker = orig })
}

func TestRunCheckUpdate(t *testing.T) {
	withVersion(t, "v0.8.0")
	withChecker(t, fakeReleases(t))
	cfgPath := filepath.Join(t.TempDir(), "transponder.yaml")
	cfg := "providers:\n  - {name: local, type: ollama}\nroutes:\n  - {match: '*', targets: [{provider: local}]}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--check-update", "--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{"v0.9.0 is out, 1 release ahead", "Impact: bug fixes", "Affects your setup (1)", "you use provider:ollama"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout misses %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunCheckUpdateBadConfigStillChecks(t *testing.T) {
	withVersion(t, "v0.8.0")
	withChecker(t, fakeReleases(t))
	var stdout, stderr bytes.Buffer
	code := runCheckUpdate(filepath.Join(t.TempDir(), "missing.yaml"), &stdout, &stderr)
	if code != 0 || !strings.Contains(stderr.String(), "no change is matched") || !strings.Contains(stdout.String(), "Other changes (1)") {
		t.Fatalf("code %d\nstdout %s\nstderr %s", code, stdout.String(), stderr.String())
	}
}

func TestRunCheckUpdateFails(t *testing.T) {
	c := fakeReleases(t)
	c.Repo = "raven-clown/elsewhere"
	withChecker(t, c)
	var stdout, stderr bytes.Buffer
	if code := runCheckUpdate("", &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "checking for updates") {
		t.Fatalf("code %d stderr %s", code, stderr.String())
	}
}

func TestUsedFeatures(t *testing.T) {
	got := usedFeatures(&config.Bootstrap{Providers: []config.Provider{{Type: "openai"}, {Type: "ollama"}}})
	if !got["provider:openai"] || !got["provider:ollama"] || got["provider:echo"] {
		t.Fatalf("used %v", got)
	}
	if !usedFeatures(&config.Bootstrap{})["provider:echo"] {
		t.Fatal("a config with no providers runs the echo provider")
	}
}

type syncBuffer struct {
	mu  chan struct{}
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu <- struct{}{}
	defer func() { <-b.mu }()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu <- struct{}{}
	defer func() { <-b.mu }()
	return b.buf.String()
}

func TestWatchUpdatesLogsOnce(t *testing.T) {
	withVersion(t, "v0.8.0")
	out := &syncBuffer{mu: make(chan struct{}, 1)}
	logger := slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watchUpdates(ctx, logger, fakeReleases(t), map[string]bool{"provider:ollama": true}, time.Hour)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "update available") {
		if time.Now().After(deadline) {
			t.Fatalf("no update line: %s", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	for _, want := range []string{`"latest":"v0.9.0"`, `"urgency":"fix"`, `"changes_affecting_config":1`, `"releases_behind":1`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("log misses %s: %s", want, out.String())
		}
	}
}

func TestLogUpdateQuietCases(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}))
	withVersion(t, "v0.9.0")
	logUpdate(context.Background(), logger, fakeReleases(t), nil)
	withVersion(t, "dev")
	logUpdate(context.Background(), logger, fakeReleases(t), nil)
	broken := fakeReleases(t)
	broken.Repo = "raven-clown/elsewhere"
	logUpdate(context.Background(), logger, broken, nil)
	if out.Len() != 0 {
		t.Fatalf("expected no info lines, got %s", out.String())
	}
}
