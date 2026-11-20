package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/server"
	"github.com/raven-clown/transponder/internal/telemetry"
)

func TestServeOps(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	stop, err := serveOps(context.Background(), "127.0.0.1:0", server.Ops(nil, telemetry.NewMetrics("v1")), logger)
	if err != nil {
		t.Fatal(err)
	}
	var line struct {
		Msg  string `json:"msg"`
		Addr string `json:"addr"`
	}
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil || line.Msg != "ops listening" || line.Addr == "" {
		t.Fatalf("log %q: %v", logs.String(), err)
	}
	for path, want := range map[string]string{
		"/metrics": `transponder_build_info{version="v1"} 1`,
		"/healthz": `"status":"ok"`,
		"/readyz":  `"status":"ok"`,
	} {
		if code, body := get(t, "http://"+line.Addr+path, ""); code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: status %d, body %s", path, code, body)
		}
	}
	stop()
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", line.Addr)
	if err == nil {
		_ = conn.Close()
		t.Fatal("the ops listener still accepts connections after stop")
	}
}

func TestServeOpsOffAndBadAddr(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	stop, err := serveOps(context.Background(), "", http.NotFoundHandler(), logger)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if _, err := serveOps(context.Background(), "not-an-address", http.NotFoundHandler(), logger); err == nil || !strings.Contains(err.Error(), "ops_listen_addr") {
		t.Fatalf("err = %v, want it to name ops_listen_addr", err)
	}
}

// freeAddr returns a local address nothing listens on right now.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// On the gateway's port the metrics need a key; on the ops port they do not.
func TestServeMetricsOnBothPorts(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ops := freeAddr(t)
	cfg := &config.Bootstrap{
		ListenAddr: ln.Addr().String(), OpsListenAddr: ops,
		Store: config.Store{SQLite: filepath.Join(t.TempDir(), "t.db")}, LogLevel: "info",
	}
	key, stop := startServe(t, cfg, ln)
	defer stop()
	gateway := "http://" + ln.Addr().String() + "/metrics"

	if code, body := get(t, gateway, ""); code != http.StatusUnauthorized || strings.Contains(body, "transponder_") {
		t.Fatalf("gateway port without a key: status %d, body %s", code, body)
	}
	if code, body := get(t, gateway, key); code != http.StatusOK || !strings.Contains(body, "transponder_build_info{version=\""+version+"\"} 1\n") {
		t.Fatalf("gateway port with the admin key: status %d, body %s", code, body)
	}
	if code, body := get(t, "http://"+ops+"/metrics", ""); code != http.StatusOK || !strings.Contains(body, "transponder_inflight_requests") {
		t.Fatalf("ops port: status %d, body %s", code, body)
	}
	if code, _ := get(t, "http://"+ops+"/v1/models", key); code != http.StatusNotFound {
		t.Fatalf("the gateway API answers on the ops port: status %d", code)
	}
}

func TestServeFailsOnBadOpsAddr(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	cfg := &config.Bootstrap{
		ListenAddr: ln.Addr().String(), OpsListenAddr: "not-an-address",
		Store: config.Store{SQLite: filepath.Join(t.TempDir(), "t.db")}, LogLevel: "info",
	}
	err = serve(context.Background(), cfg, ln, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "ops_listen_addr") {
		t.Fatalf("err = %v, want it to name ops_listen_addr", err)
	}
}
