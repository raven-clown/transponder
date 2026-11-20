package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/config"
)

func adminCall(t *testing.T, base, key, method, path, body string) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, base+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestServeAdminAPI(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Bootstrap{ListenAddr: ln.Addr().String(), Store: config.Store{SQLite: filepath.Join(t.TempDir(), "t.db")}, LogLevel: "info"}
	adminKey, stop := startServe(t, cfg, ln)
	defer stop()
	base := "http://" + ln.Addr().String()

	code, status := adminCall(t, base, adminKey, http.MethodGet, "/admin/v1/status", "")
	if code != http.StatusOK || status["store"].(map[string]any)["kind"] != "sqlite" {
		t.Fatalf("status %d %v", code, status)
	}
	startVersion := status["config"].(map[string]any)["version"].(float64)

	code, created := adminCall(t, base, adminKey, http.MethodPost, "/admin/v1/keys", `{"name":"logbook","role":"viewer"}`)
	if code != http.StatusCreated {
		t.Fatalf("create %d %v", code, created)
	}
	secret := created["secret"].(string)

	// The new key works on the gateway and, as a viewer, reads the admin API but cannot write.
	if code, _ := adminCall(t, base, secret, http.MethodGet, "/admin/v1/keys", ""); code != http.StatusOK {
		t.Fatalf("viewer read %d", code)
	}
	if code, _ := adminCall(t, base, secret, http.MethodPost, "/admin/v1/keys", `{"name":"x"}`); code != http.StatusForbidden {
		t.Fatalf("viewer write %d", code)
	}
	if code, _ := adminCall(t, base, secret, http.MethodPost, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`); code != http.StatusOK {
		t.Fatalf("gateway call %d", code)
	}

	// The write raised the config version and this replica reloaded before answering.
	_, status = adminCall(t, base, adminKey, http.MethodGet, "/admin/v1/status", "")
	if v := status["config"].(map[string]any)["version"].(float64); v != startVersion+1 {
		t.Fatalf("config version %v, want %v", v, startVersion+1)
	}
	_, audit := adminCall(t, base, adminKey, http.MethodGet, "/admin/v1/audit?resource=keys/logbook", "")
	items := audit["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["request_id"] == "" {
		t.Fatalf("audit %v", audit)
	}
	_, usage := adminCall(t, base, adminKey, http.MethodGet, "/admin/v1/requests", "")
	if items := usage["items"].([]any); len(items) != 1 || items[0].(map[string]any)["user"] != "logbook" {
		t.Fatalf("requests %v", usage)
	}
}
