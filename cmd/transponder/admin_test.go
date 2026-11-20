package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/runtime"
	"github.com/raven-clown/transponder/internal/store/postgres/pgtest"
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

// On Postgres a route made through one replica reaches the other within the reload interval,
// with the provider key both open under the shared secrets_key.
func TestServeAdminWriteReachesOtherReplica(t *testing.T) {
	dsn := pgtest.DSN(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-shared-5678" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	listen := func() net.Listener {
		ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return ln
	}
	cfg := &config.Bootstrap{Store: config.Store{Postgres: dsn}, LogLevel: "error", SecretsKey: base64.StdEncoding.EncodeToString(make([]byte, 32))}
	lnA, lnB := listen(), listen()
	adminKey, stopA := startServe(t, cfg, lnA)
	defer stopA()
	defer startReplica(t, cfg, lnB)()

	base := "http://" + lnA.Addr().String()
	for _, w := range []struct{ path, body string }{
		{"/admin/v1/providers", `{"name":"up","type":"openai","base_url":"` + up.URL + `","api_key":"sk-shared-5678"}`},
		{"/admin/v1/routes", `{"match":"shared-*","targets":[{"provider":"up"}]}`},
	} {
		if code, out := adminCall(t, base, adminKey, http.MethodPost, w.path, w.body); code != http.StatusCreated {
			t.Fatalf("%s: %d %v", w.path, code, out)
		}
	}
	chat := `{"model":"shared-1","messages":[{"role":"user","content":"hi"}]}`
	deadline := time.After(runtime.DefaultInterval + 2*time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		status, _ := postChat(t, lnB.Addr().String(), adminKey, chat)
		if status == http.StatusOK {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the other replica still answers %d", status)
		case <-tick.C:
		}
	}
}

func TestOpenSecrets(t *testing.T) {
	dir := t.TempDir()
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	tests := []struct {
		name    string
		cfg     config.Bootstrap
		wantNil bool
		wantErr bool
	}{
		{name: "secrets_key", cfg: config.Bootstrap{SecretsKey: key, Store: config.Store{Postgres: "postgres://x"}}},
		{name: "bad secrets_key", cfg: config.Bootstrap{SecretsKey: "short"}, wantErr: true},
		{name: "postgres without a key", cfg: config.Bootstrap{Store: config.Store{Postgres: "postgres://x"}}, wantNil: true},
		{name: "sqlite makes a key file", cfg: config.Bootstrap{Store: config.Store{SQLite: filepath.Join(dir, "t.db")}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box, err := openSecrets(&tt.cfg)
			if (err != nil) != tt.wantErr || (box == nil) != (tt.wantNil || tt.wantErr) {
				t.Fatalf("box %v, err %v", box, err)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "transponder.key")); err != nil {
		t.Fatal(err)
	}
}

func TestLimiterSource(t *testing.T) {
	lc := limits.Config{Budgets: []limits.Budget{{Subject: "user:*", Window: limits.Month, LimitNUSD: 1}}}
	boom := errors.New("boom")
	failing := &limiterSource{build: func(limits.Config) (*limits.Limiter, func(), error) { return nil, nil, boom }}
	if _, err := failing.limiter(lc); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	builds, stops := 0, 0
	s := &limiterSource{build: func(lc limits.Config) (*limits.Limiter, func(), error) {
		builds++
		return limits.New(lc, limits.NewMemoryStore(), time.Now), func() { stops++ }, nil
	}}
	if l, err := s.limiter(limits.Config{}); l != nil || err != nil {
		t.Fatalf("nothing limited: %v, %v", l, err)
	}
	for range 2 {
		if l, err := s.limiter(lc); l == nil || err != nil {
			t.Fatalf("limiter %v, %v", l, err)
		}
	}
	if l, _ := s.limiter(limits.Config{}); l == nil {
		t.Fatal("once made, the counters stay, so a budget added back starts from them")
	}
	s.close()
	if builds != 1 || stops != 1 {
		t.Fatalf("builds %d, stops %d", builds, stops)
	}
	(&limiterSource{}).close()
}

// A replica that started with no providers, routes or limits is set up through the admin API
// alone, and each write is live when its answer comes back.
func TestServeConfiguredThroughAdminAPI(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-upstream-1234" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000000,"completion_tokens":0}}`)
	}))
	defer up.Close()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := &config.Bootstrap{ListenAddr: ln.Addr().String(), Store: config.Store{SQLite: filepath.Join(dir, "t.db")}, LogLevel: "info"}
	adminKey, stop := startServe(t, cfg, ln)
	defer stop()
	base := "http://" + ln.Addr().String()
	addr := ln.Addr().String()
	chat := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`

	call := func(method, path, body string, want int) map[string]any {
		t.Helper()
		code, out := adminCall(t, base, adminKey, method, path, body)
		if code != want {
			t.Fatalf("%s %s: %d %v", method, path, code, out)
		}
		return out
	}
	call(http.MethodPost, "/admin/v1/providers", `{"name":"up","type":"openai","base_url":"`+up.URL+`","api_key":"sk-upstream-1234"}`, http.StatusCreated)
	route := call(http.MethodPost, "/admin/v1/routes", `{"match":"gpt-*","targets":[{"provider":"up"}]}`, http.StatusCreated)

	// The key is sealed at rest, under the key the first start wrote next to the database.
	if _, err := os.Stat(filepath.Join(dir, "transponder.key")); err != nil {
		t.Fatalf("no secrets key file: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, "t.db"))); err != nil || strings.Contains(string(raw), "sk-upstream-1234") {
		t.Fatalf("the provider key is in the database file in the clear (%v)", err)
	}

	status, out := postChat(t, addr, adminKey, chat)
	if status != http.StatusOK {
		t.Fatalf("chat through the new route: %d %s", status, out)
	}

	// No limits at start, a budget through the API, and the next call is held to it.
	// Each call costs $2.50 of gpt-4o input, so a $1 budget lets at most one more through.
	call(http.MethodPost, "/admin/v1/budgets", `{"subject":"user:*","period":"monthly","limit_usd":1}`, http.StatusCreated)
	limited := false
	for range 3 {
		if status, _ := postChat(t, addr, adminKey, chat); status == http.StatusPaymentRequired {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("a budget added through the API did not hold")
	}

	// A write that would leave a config that does not load is refused and the old one serves.
	call(http.MethodPatch, "/admin/v1/routes/"+route["id"].(string), `{"targets":[{"provider":"ghost"}]}`, http.StatusBadRequest)
	_, st := adminCall(t, base, adminKey, http.MethodGet, "/admin/v1/status", "")
	if cfgStatus := st["config"].(map[string]any); cfgStatus["reload_failed"] != false {
		t.Fatalf("status %v", st)
	}
	call(http.MethodDelete, "/admin/v1/providers/up", "", http.StatusConflict)
}
