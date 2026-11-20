package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	_ "net/http/pprof" //nolint:gosec // served only by the fake gateway the tests start
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for a transponder binary, so run is tested end to end.
func TestMain(m *testing.M) {
	if os.Getenv("BENCH_FAKE_GATEWAY") == "1" {
		os.Exit(fakeGateway(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeGateway reads the listen address from the config it is given, prints an admin key the
// way transponder does, and answers every call with a body that passes every scenario.
func fakeGateway(args []string) int {
	if len(args) != 2 || args[0] != "--config" {
		return 2
	}
	raw, err := os.ReadFile(args[1]) //nolint:gosec // the config the test wrote
	if err != nil {
		return 1
	}
	addr := regexp.MustCompile(`listen_addr: "([^"]+)"`).FindSubmatch(raw)
	if addr == nil {
		return 1
	}
	if p := os.Getenv("TRANSPONDER_PPROF_ADDR"); p != "" {
		go func() { _ = http.ListenAndServe(p, nil) }() //nolint:gosec // test helper
	}
	_, _ = fmt.Fprint(os.Stderr, "transponder: created an admin API key. It is shown once, so save it now:\ntp-fake\n")
	mux := http.NewServeMux()
	// Should the free port the test picked for pprof be handed to the gateway too, it is here.
	mux.Handle("GET /debug/pprof/", http.DefaultServeMux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tp-fake" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `chat.completion [DONE] message_stop "embedding"`)
	})
	srv := &http.Server{Addr: string(addr[1]), Handler: mux, ReadHeaderTimeout: time.Second}
	if err := srv.ListenAndServe(); err != nil {
		return 1
	}
	return 0
}

func testUpstream(t *testing.T, cfg upstreamConfig) *httptest.Server {
	t.Helper()
	u, err := newUpstream(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(u.handler())
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestUpstream(t *testing.T) {
	cfg := upstreamConfig{Size: 100, Chunks: 4, Dims: 3, Interval: time.Microsecond}
	srv := testUpstream(t, cfg)
	tests := []struct {
		name, path, body string
		status           int
		check            func(t *testing.T, body string)
	}{
		{"chat", "/v1/chat/completions", `{"model":"m"}`, 200, func(t *testing.T, body string) {
			var resp struct {
				Choices []struct{ Message struct{ Content string } }
				Usage   struct {
					PromptTokens int `json:"prompt_tokens"`
				}
			}
			if err := json.Unmarshal([]byte(body), &resp); err != nil {
				t.Fatal(err)
			}
			if len(resp.Choices[0].Message.Content) != cfg.Size || resp.Usage.PromptTokens != promptTokens {
				t.Fatalf("answer %+v", resp)
			}
		}},
		{"chat stream", "/v1/chat/completions", `{"model":"m","stream":true}`, 200, func(t *testing.T, body string) {
			events := strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n")
			// role, four text chunks, finish, usage, [DONE]
			if len(events) != cfg.Chunks+4 || events[len(events)-1] != "data: [DONE]" || !strings.Contains(events[len(events)-2], `"usage"`) {
				t.Fatalf("stream %q", body)
			}
			var text strings.Builder
			for _, e := range events[1 : cfg.Chunks+1] {
				var c struct {
					Choices []struct{ Delta struct{ Content string } }
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(e, "data: ")), &c); err != nil {
					t.Fatal(err)
				}
				text.WriteString(c.Choices[0].Delta.Content)
			}
			if text.Len() != cfg.Size {
				t.Fatalf("text is %d bytes", text.Len())
			}
		}},
		{"one embedding", "/v1/embeddings", `{"model":"m","input":"a"}`, 200, func(t *testing.T, body string) {
			if strings.Count(body, `"embedding":[`) != 1 || !strings.Contains(body, `"prompt_tokens":8`) {
				t.Fatalf("body %s", body)
			}
		}},
		{"three embeddings", "/v1/embeddings", `{"model":"m","input":["a","b","c"]}`, 200, func(t *testing.T, body string) {
			var resp struct {
				Data []struct{ Embedding []float64 }
			}
			if err := json.Unmarshal([]byte(body), &resp); err != nil {
				t.Fatal(err)
			}
			if len(resp.Data) != 3 || len(resp.Data[2].Embedding) != cfg.Dims {
				t.Fatalf("body %s", body)
			}
		}},
		{"not json", "/v1/chat/completions", `{`, 400, nil},
		{"embeddings not json", "/v1/embeddings", `nope`, 400, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := post(t, srv.URL+tt.path, tt.body)
			if status != tt.status {
				t.Fatalf("status %d: %s", status, body)
			}
			if tt.check != nil {
				tt.check(t, body)
			}
		})
	}
}

func TestUpstreamCallerGone(t *testing.T) {
	u, err := newUpstream(upstreamConfig{Latency: time.Hour, Size: 1, Chunks: 1, Dims: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/embeddings"} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(`{"model":"m"}`))
		rec := httptest.NewRecorder()
		u.handler().ServeHTTP(rec, req)
		if rec.Body.Len() != 0 {
			t.Fatalf("%s answered a caller that left: %s", path, rec.Body)
		}
	}
	if wait(t.Context(), 0) != true {
		t.Fatal("wait(0) on a live context should go on")
	}
}

func TestNewUpstreamRejects(t *testing.T) {
	for _, cfg := range []upstreamConfig{
		{Latency: -1, Size: 1, Chunks: 1, Dims: 1},
		{Interval: -1, Size: 1, Chunks: 1, Dims: 1},
		{Size: 0, Chunks: 1, Dims: 1},
		{Size: 1, Chunks: 0, Dims: 1},
		{Size: 1, Chunks: 1, Dims: 0},
	} {
		if _, err := newUpstream(cfg); err == nil {
			t.Errorf("%+v was accepted", cfg)
		}
	}
}

func TestDrive(t *testing.T) {
	ok := testUpstream(t, upstreamConfig{Size: 10, Chunks: 1, Dims: 1})
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(failing.Close)
	body := []byte(`{"model":"m"}`)
	tests := []struct {
		name    string
		cfg     loadConfig
		errors  bool
		failure string
	}{
		{"fixed rate", loadConfig{URL: ok.URL + "/v1/chat/completions", Key: "k", Body: body, Rate: 400, Concurrency: 8, Duration: 100 * time.Millisecond, Expect: "chat.completion"}, false, ""},
		{"closed loop", loadConfig{URL: ok.URL + "/v1/chat/completions", Body: body, Concurrency: 4, Duration: 50 * time.Millisecond, Expect: "chat.completion"}, false, ""},
		{"bad status", loadConfig{URL: failing.URL, Body: body, Rate: 200, Concurrency: 2, Duration: 50 * time.Millisecond}, true, "status 418 I'm a teapot"},
		{"answer without marker", loadConfig{URL: ok.URL + "/v1/chat/completions", Body: body, Rate: 200, Concurrency: 2, Duration: 50 * time.Millisecond, Expect: "nope"}, true, "answer without nope"},
		{"bad url", loadConfig{URL: "::", Body: body, Rate: 200, Concurrency: 2, Duration: 20 * time.Millisecond}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := drive(t.Context(), newClient(8), tt.cfg)
			if res.Sent == 0 {
				t.Fatal("nothing was sent")
			}
			if (res.Errors > 0) != tt.errors {
				t.Fatalf("errors %d: %s", res.Errors, summary(res))
			}
			if !tt.errors && len(res.Latency) != res.Sent {
				t.Fatalf("%d answered of %d", len(res.Latency), res.Sent)
			}
			if tt.failure != "" && res.Failures[tt.failure] != res.Errors {
				t.Fatalf("failures %v", res.Failures)
			}
		})
	}
}

func TestDriveStopsWithContext(t *testing.T) {
	srv := testUpstream(t, upstreamConfig{Latency: time.Hour, Size: 1, Chunks: 1, Dims: 1})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	res := drive(ctx, newClient(1), loadConfig{URL: srv.URL + "/v1/chat/completions", Body: []byte(`{}`), Rate: 1000, Concurrency: 1, Duration: time.Hour})
	if res.Errors == 0 || res.Elapsed > 10*time.Second {
		t.Fatalf("%s in %s", summary(res), res.Elapsed)
	}
}

func TestPercentile(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	r := loadResult{Elapsed: 2 * time.Second}
	for i := 1; i <= 100; i++ {
		r.Latency = append(r.Latency, ms(i))
	}
	for _, tt := range []struct {
		p    float64
		want time.Duration
	}{{0.5, ms(50)}, {0.95, ms(95)}, {0.99, ms(99)}, {1, ms(100)}, {0.001, ms(1)}} {
		if got := r.Percentile(tt.p); got != tt.want {
			t.Errorf("p%v = %s, want %s", tt.p, got, tt.want)
		}
	}
	if r.Throughput() != 50 {
		t.Errorf("throughput %v", r.Throughput())
	}
	if (loadResult{}).Percentile(0.5) != 0 || (loadResult{}).Throughput() != 0 {
		t.Error("an empty result should be all zero")
	}
}

func TestClassify(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "timeout"},
		{fmt.Errorf("x: %w", context.Canceled), "cancelled"},
		{errors.New("dial tcp: connection refused"), "connection refused"},
		{errors.New("read: connection reset by peer"), "connection reset"},
		{io.ErrUnexpectedEOF, "EOF"},
		{errors.New("something else"), "transport error"},
	} {
		if got := classify(tt.err); got != tt.want {
			t.Errorf("classify(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestMedian(t *testing.T) {
	rows := []row{
		{Scenario: "s", Throughput: 10, P50: 3, P95: 6, P99: 9, MemoryMiB: 30, PeakMiB: 40, Sent: 10, Errors: 1, Failures: map[string]int{"timeout": 1}},
		{Scenario: "s", Throughput: 30, P50: 1, P95: 4, P99: 7, MemoryMiB: 10, PeakMiB: 50, Sent: 10, Failures: map[string]int{}},
		{Scenario: "s", Throughput: 20, P50: 2, P95: 5, P99: 8, MemoryMiB: 20, PeakMiB: 45, Sent: 10, Errors: 2, Failures: map[string]int{"timeout": 2}},
	}
	got := median(rows)
	if got.Throughput != 20 || got.P50 != 2 || got.P95 != 5 || got.P99 != 8 || got.MemoryMiB != 20 || got.PeakMiB != 40 || got.Sent != 30 || got.Errors != 3 || got.Failures["timeout"] != 3 {
		t.Fatalf("median %+v", got)
	}
}

func TestToRow(t *testing.T) {
	res := loadResult{Sent: 2, Elapsed: time.Second, Latency: []time.Duration{21 * time.Millisecond, 23 * time.Millisecond}}
	r := toRow(scenario{Name: "chat", Rate: 5, Upstream: 20 * time.Millisecond}, res)
	if r.P50 != 1 || r.P99 != 3 || r.TargetRate != 5 || r.Throughput != 2 {
		t.Fatalf("row %+v", r)
	}
}

func TestProcessMemory(t *testing.T) {
	cur, peak, err := processMemory(os.Getpid())
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		if err == nil {
			t.Fatal("want an error where reading memory is not supported")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if cur == 0 || peak < cur {
		t.Fatalf("now %d, peak %d", cur, peak)
	}
	if _, _, err := processMemory(-1); err == nil {
		t.Fatal("want an error for a process that does not exist")
	}
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"load", "-h"}} {
		var out, errb bytes.Buffer
		if code := run(t.Context(), args, &out, &errb); code != 2 {
			t.Errorf("run %v = %d, want 2", args, code)
		}
	}
	var out, errb bytes.Buffer
	if code := run(t.Context(), []string{"load"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "-url and -body are required") {
		t.Errorf("load without flags: %d %s", code, errb.String())
	}
}

func TestRunLoad(t *testing.T) {
	srv := testUpstream(t, upstreamConfig{Size: 10, Chunks: 1, Dims: 1})
	body := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(body, []byte(`{"model":"m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	args := []string{"load", "-url", srv.URL + "/v1/chat/completions", "-body", body, "-rate", "200", "-duration", "50ms", "-warmup", "10ms", "-expect", "chat.completion"}
	if runtime.GOOS == "windows" || runtime.GOOS == "linux" {
		args = append(args, "-pid", fmt.Sprint(os.Getpid()))
	}
	if code := run(t.Context(), args, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "errors 0") {
		t.Fatalf("output %s", out.String())
	}
	if code := run(t.Context(), []string{"load", "-url", "http://x", "-body", filepath.Join(t.TempDir(), "missing")}, &out, &errb); code != 1 {
		t.Fatalf("a missing body file should fail, got %d", code)
	}
}

// lineWriter hands each write to a channel, so a test can wait for a server's first line.
type lineWriter chan string

func (w lineWriter) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}

func TestRunUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	lines := make(lineWriter, 1)
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"upstream", "-addr", "127.0.0.1:0", "-latency", "0"}, lines, io.Discard)
	}()
	line := <-lines
	addr := strings.TrimSpace(strings.TrimPrefix(line, "upstream: listening on "))
	if status, body := post(t, "http://"+addr+"/v1/chat/completions", `{"model":"m"}`); status != 200 || !strings.Contains(body, "chat.completion") {
		t.Fatalf("status %d: %s", status, body)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
	if code := run(t.Context(), []string{"upstream", "-size", "0"}, io.Discard, io.Discard); code != 1 {
		t.Fatalf("a zero size should fail, got %d", code)
	}
}

func TestRunSuite(t *testing.T) {
	t.Setenv("BENCH_FAKE_GATEWAY", "1")
	dir := t.TempDir()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pprofAddr := ln.Addr().String()
	_ = ln.Close()
	var outs []string
	for i, label := range []string{"before", "after"} {
		out := filepath.Join(dir, label+".json")
		args := []string{"run", "-bin", os.Args[0], "-label", label, "-out", out, "-rate", "100", "-concurrency", "4",
			"-duration", "50ms", "-warmup", "10ms", "-count", "2", "-latency", "1ms"}
		if i == 0 {
			args = append(args, "-pprof", pprofAddr, "-profile", "chat", "-profile-dir", dir)
		}
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "| chat, max throughput | max |") || !strings.Contains(stdout.String(), label+" on ") {
			t.Fatalf("table:\n%s", stdout.String())
		}
		rep, err := readReport(out)
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Rows) != 7 || rep.Count != 2 {
			t.Fatalf("report %+v", rep)
		}
		for _, r := range rep.Rows {
			if r.Errors != 0 || r.Sent == 0 {
				t.Fatalf("row %+v", r)
			}
		}
		outs = append(outs, out)
	}
	for _, p := range []string{"cpu", "heap", "allocs", "block", "mutex"} {
		if _, err := os.Stat(filepath.Join(dir, p+".pprof")); err != nil {
			t.Error(err)
		}
	}
	var table bytes.Buffer
	if code := run(t.Context(), append([]string{"compare"}, outs...), &table, io.Discard); code != 0 {
		t.Fatal("compare failed")
	}
	if n := strings.Count(table.String(), "\n"); n != 9 {
		t.Fatalf("compare printed %d lines:\n%s", n, table.String())
	}
	merged := filepath.Join(dir, "merged.json")
	if code := run(t.Context(), append([]string{"merge", merged}, outs...), io.Discard, io.Discard); code != 0 {
		t.Fatal("merge failed")
	}
	rep, err := readReport(merged)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count != 4 || len(rep.Rows) != 7 || rep.Rows[0].Scenario != "direct chat (floor)" {
		t.Fatalf("merged %+v", rep)
	}
}

func TestRunSuiteRejects(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for _, args := range [][]string{
		{"run"},
		{"run", "-bin", "x", "-count", "0"},
		{"run", "-bin", missing, "-duration", "1ms"},
		{"compare", "one"},
		{"compare", missing, missing},
		{"merge", "out.json", "one.json"},
		{"merge", filepath.Join(t.TempDir(), "out.json"), missing, missing},
	} {
		if code := run(t.Context(), args, io.Discard, io.Discard); code != 1 {
			t.Errorf("run %v = %d, want 1", args, code)
		}
	}
}

func TestStartGatewayExits(t *testing.T) {
	// The test binary without BENCH_FAKE_GATEWAY rejects --config and exits at once.
	_, err := startGateway(t.Context(), os.Args[0], t.TempDir(), "http://a", "http://b", nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "exited before") {
		t.Fatalf("err %v", err)
	}
}

func TestReadReportBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readReport(path); err == nil {
		t.Fatal("want an error")
	}
	if change(0, 1) != "n/a" || change(100, 110) != "+10%" {
		t.Fatal("change")
	}
}

func TestTail(t *testing.T) {
	var tl tail
	for i := range 25 {
		tl.add(fmt.Sprint(i))
	}
	s := bufio.NewScanner(strings.NewReader(tl.String()))
	n := 0
	for s.Scan() {
		n++
	}
	if n != 20 || !strings.HasPrefix(tl.String(), "5\n") {
		t.Fatalf("tail %q", tl.String())
	}
}
