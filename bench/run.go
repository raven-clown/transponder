package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// scenario is one kind of call the suite measures.
type scenario struct {
	Name   string
	Path   string
	Body   string
	Expect string
	// Rate is calls per second; zero runs Concurrency callers as fast as they go.
	Rate        float64
	Concurrency int
	// Upstream is the time the fake upstream spends on the call, taken off the latency.
	Upstream time.Duration
	// Direct sends the call to the fake upstream, not the gateway, to show the floor.
	Direct bool
}

const (
	system = "You are a careful assistant for a team that runs a gateway in front of several language model providers. Answer in short plain sentences, name the file or setting you mean, and say when you are not sure. Do not repeat the question back."
	prompt = "Our gateway sends chat calls to two providers and falls back from the first to the second when the first answers 429 or 5xx. Usage is written to SQLite after each call, and each team has a monthly budget. Over the last hour the p99 latency the gateway adds went from 2 ms to 9 ms while traffic stayed flat at about 400 calls a second. What should we look at first, and what would you measure to tell a slow store from a slow upstream?"
)

func chatBody(model string, stream bool) string {
	b, _ := json.Marshal(map[string]any{
		"model": model, "stream": stream, "max_tokens": 256,
		"messages": []any{map[string]any{"role": "system", "content": system}, map[string]any{"role": "user", "content": prompt}},
	})
	return string(b)
}

func messagesBody(model string) string {
	b, _ := json.Marshal(map[string]any{
		"model": model, "stream": true, "max_tokens": 256, "system": system,
		"messages": []any{map[string]any{"role": "user", "content": prompt}},
	})
	return string(b)
}

func embeddingsBody(model string) string {
	b, _ := json.Marshal(map[string]any{"model": model, "input": []string{prompt[:200], prompt[100:300], prompt[200:400], system[:200]}})
	return string(b)
}

// scenarios is the suite: a floor straight to the upstream, each request path at a fixed
// rate against an upstream with latency, and the most calls a second against one without.
func scenarios(rate float64, concurrency int, up upstreamConfig) []scenario {
	stream := up.Latency + time.Duration(up.Chunks+2)*up.Interval
	return []scenario{
		{Name: "direct chat (floor)", Path: "/v1/chat/completions", Body: chatBody("slow-chat", false), Expect: "chat.completion", Rate: rate, Concurrency: concurrency, Upstream: up.Latency, Direct: true},
		{Name: "chat", Path: "/v1/chat/completions", Body: chatBody("slow-chat", false), Expect: "chat.completion", Rate: rate, Concurrency: concurrency, Upstream: up.Latency},
		{Name: "chat stream", Path: "/v1/chat/completions", Body: chatBody("slow-chat", true), Expect: "[DONE]", Rate: rate, Concurrency: concurrency, Upstream: stream},
		{Name: "messages stream, translated", Path: "/v1/messages", Body: messagesBody("slow-chat"), Expect: "message_stop", Rate: rate, Concurrency: concurrency, Upstream: stream},
		{Name: "embeddings", Path: "/v1/embeddings", Body: embeddingsBody("slow-embed"), Expect: `"embedding"`, Rate: rate, Concurrency: concurrency, Upstream: up.Latency},
		{Name: "chat, max throughput", Path: "/v1/chat/completions", Body: chatBody("quick-chat", false), Expect: "chat.completion", Concurrency: concurrency},
		{Name: "chat stream, max throughput", Path: "/v1/chat/completions", Body: chatBody("quick-chat", true), Expect: "[DONE]", Concurrency: concurrency},
	}
}

// row is one scenario's numbers. Latencies are what the gateway added, in milliseconds.
type row struct {
	Scenario   string         `json:"scenario"`
	TargetRate float64        `json:"target_rate"`
	Throughput float64        `json:"throughput"`
	Sent       int            `json:"sent"`
	Errors     int            `json:"errors"`
	P50        float64        `json:"p50_ms"`
	P95        float64        `json:"p95_ms"`
	P99        float64        `json:"p99_ms"`
	// MemoryMiB is the gateway's working set after the scenario, PeakMiB the highest since it
	// started.
	MemoryMiB  float64        `json:"memory_mib"`
	PeakMiB    float64        `json:"peak_mib"`
	Failures   map[string]int `json:"failures,omitempty"`
}

// report is a whole run, written as JSON so two runs can be compared later.
type report struct {
	Label    string         `json:"label"`
	Binary   string         `json:"binary"`
	Date     time.Time      `json:"date"`
	GOOS     string         `json:"goos"`
	GOARCH   string         `json:"goarch"`
	CPUs     int            `json:"cpus"`
	Duration string         `json:"duration"`
	Count    int            `json:"count"`
	Upstream upstreamConfig `json:"upstream"`
	Rows     []row          `json:"rows"`
}

func toRow(s scenario, r loadResult) row {
	added := func(p float64) float64 {
		return float64(r.Percentile(p)-s.Upstream) / float64(time.Millisecond)
	}
	return row{
		Scenario: s.Name, TargetRate: s.Rate, Throughput: r.Throughput(), Sent: r.Sent, Errors: r.Errors,
		P50: added(0.50), P95: added(0.95), P99: added(0.99), Failures: r.Failures,
	}
}

// median takes, for each number, the middle of several runs of one scenario.
func median(rows []row) row {
	out := rows[0]
	pick := func(get func(row) float64) float64 {
		v := make([]float64, len(rows))
		for i, r := range rows {
			v[i] = get(r)
		}
		slices.Sort(v)
		return v[len(v)/2]
	}
	out.Throughput = pick(func(r row) float64 { return r.Throughput })
	out.P50 = pick(func(r row) float64 { return r.P50 })
	out.P95 = pick(func(r row) float64 { return r.P95 })
	out.P99 = pick(func(r row) float64 { return r.P99 })
	out.MemoryMiB = pick(func(r row) float64 { return r.MemoryMiB })
	// The peak only grows over a process's life, so the first round's is the one that belongs
	// to the scenarios run so far.
	out.PeakMiB = slices.MinFunc(rows, func(a, b row) int { return cmp.Compare(a.PeakMiB, b.PeakMiB) }).PeakMiB
	out.Sent, out.Errors, out.Failures = 0, 0, map[string]int{}
	for _, r := range rows {
		out.Sent += r.Sent
		out.Errors += r.Errors
		for k, n := range r.Failures {
			out.Failures[k] += n
		}
	}
	return out
}

func runSuite(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bin := fs.String("bin", "", "transponder binary to measure")
	label := fs.String("label", "", "name of this run in the report, such as the version")
	out := fs.String("out", "", "file to write the report to as JSON")
	rate := fs.Float64("rate", 500, "calls per second in the fixed-rate scenarios")
	concurrency := fs.Int("concurrency", 64, "calls in flight at most, and callers in the max throughput scenarios")
	duration := fs.Duration("duration", 10*time.Second, "how long each scenario is measured")
	warmup := fs.Duration("warmup", 2*time.Second, "how long each scenario runs before it is measured")
	count := fs.Int("count", 1, "how many times to run the suite; each number is the median")
	only := fs.String("only", "", "run only scenarios whose name contains this")
	pprofAddr := fs.String("pprof", "", "address the gateway serves pprof on, for a binary built with -tags pprof")
	profile := fs.String("profile", "", "scenario to profile through -pprof")
	profileDir := fs.String("profile-dir", ".", "directory for the profiles")
	logPath := fs.String("log", "", "file to keep the gateway's log in; it is thrown away by default")
	up := defaultUpstream()
	upstreamFlags(fs, &up)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bin == "" {
		return errors.New("run: -bin is required")
	}
	if *count < 1 || *concurrency < 1 {
		return errors.New("run: -count and -concurrency must be at least 1")
	}

	slow, err := startUpstream(ctx, up)
	if err != nil {
		return err
	}
	quick := up
	quick.Latency, quick.Interval = 0, 0
	fast, err := startUpstream(ctx, quick)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "transponder-bench-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	var env []string
	if *pprofAddr != "" {
		env = append(env, "TRANSPONDER_PPROF_ADDR="+*pprofAddr)
	}
	logs := io.Discard
	if *logPath != "" {
		f, err := os.Create(*logPath)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		logs = f
	}
	gw, err := startGateway(ctx, *bin, dir, slow, fast, env, logs)
	if err != nil {
		return err
	}
	defer gw.stop()

	rep := report{
		Label: *label, Binary: filepath.Base(*bin), Date: time.Now().UTC(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		CPUs: runtime.NumCPU(), Duration: duration.String(), Count: *count, Upstream: up,
	}
	client := newClient(*concurrency)
	runs := map[string][]row{}
	all := scenarios(*rate, *concurrency, up)
	for range *count {
		for _, s := range all {
			if !strings.Contains(s.Name, *only) {
				continue
			}
			base := gw.url
			if s.Direct {
				base = slow
			}
			cfg := loadConfig{URL: base + s.Path, Key: gw.key, Body: []byte(s.Body), Rate: s.Rate, Concurrency: s.Concurrency, Expect: s.Expect}
			cfg.Duration = *warmup
			drive(ctx, client, cfg)
			cfg.Duration = *duration
			var profiles func() error
			if *pprofAddr != "" && s.Name == *profile {
				profiles = captureProfiles(ctx, "http://"+*pprofAddr, *duration, *profileDir)
			}
			res := drive(ctx, client, cfg)
			if profiles != nil {
				if err := profiles(); err != nil {
					return err
				}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r := toRow(s, res)
			if cur, peak, err := processMemory(gw.pid()); err == nil {
				r.MemoryMiB, r.PeakMiB = float64(cur)/(1<<20), float64(peak)/(1<<20)
			}
			_, _ = fmt.Fprintf(stderr, "%s: %s\n", s.Name, summary(res))
			runs[s.Name] = append(runs[s.Name], r)
		}
	}
	for _, s := range all {
		if rs := runs[s.Name]; len(rs) > 0 {
			rep.Rows = append(rep.Rows, median(rs))
		}
	}
	writeTable(stdout, rep)
	if *out != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(*out, append(b, '\n'), 0o600)
	}
	return nil
}

func writeTable(w io.Writer, rep report) {
	_, _ = fmt.Fprintf(w, "%s on %s/%s, %d CPUs, %s per scenario, median of %d\n\n", rep.Label, rep.GOOS, rep.GOARCH, rep.CPUs, rep.Duration, rep.Count)
	_, _ = fmt.Fprintln(w, "| Scenario | Rate | req/s | Errors | p50 added | p95 added | p99 added | Memory | Peak |")
	_, _ = fmt.Fprintln(w, "|---|---:|---:|---:|---:|---:|---:|---:|---:|")
	for _, r := range rep.Rows {
		rate := "max"
		if r.TargetRate > 0 {
			rate = fmt.Sprintf("%.0f/s", r.TargetRate)
		}
		_, _ = fmt.Fprintf(w, "| %s | %s | %.0f | %d | %.2f ms | %.2f ms | %.2f ms | %.0f MiB | %.0f MiB |\n",
			r.Scenario, rate, r.Throughput, r.Errors, r.P50, r.P95, r.P99, r.MemoryMiB, r.PeakMiB)
	}
}

// startUpstream serves a fake upstream on a free local port until ctx ends.
func startUpstream(ctx context.Context, cfg upstreamConfig) (string, error) {
	u, err := newUpstream(cfg)
	if err != nil {
		return "", err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	go func() { _ = serve(ctx, ln, u.handler()) }()
	return "http://" + ln.Addr().String(), nil
}

// gateway is a transponder process started for the suite.
type gateway struct {
	cmd  *exec.Cmd
	url  string
	key  string
	done chan struct{}
	logs *tail
}

func (g *gateway) pid() int { return g.cmd.Process.Pid }

func (g *gateway) stop() {
	_ = g.cmd.Process.Kill()
	<-g.done
}

// tail keeps the last lines a process wrote, to show when it fails to start.
type tail struct {
	mu    sync.Mutex
	lines []string
}

func (t *tail) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > 20 {
		t.lines = t.lines[1:]
	}
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}

// gatewayConfig routes slow-chat and slow-embed to an upstream with latency and quick-chat to
// one without, and gives every caller a limits profile and a budget high enough never to bite,
// so each call is checked and settled.
func gatewayConfig(addr, db, slow, quick string) string {
	return fmt.Sprintf(`listen_addr: %q
store:
  sqlite: %q
update_check: false
providers:
  - {name: slow, type: openai, base_url: %q, api_key: bench}
  - {name: quick, type: openai, base_url: %q, api_key: bench}
routes:
  - {match: slow-chat, targets: [{provider: slow, model: gpt-4o-mini}]}
  - {match: quick-chat, targets: [{provider: quick, model: gpt-4o-mini}]}
  - {match: slow-embed, targets: [{provider: slow, model: text-embedding-3-small}]}
profiles:
  - name: bench
    limits:
      requests: {rate: 10000000/m}
      llm.tokens: {rate: 10000000000/m}
      llm.cost_usd: {per_day: 1000000}
default_profile: bench
`, addr, filepath.ToSlash(db), slow+"/v1", quick+"/v1")
}

// startGateway runs bin with a config in dir and waits until it answers, reading the admin key
// it prints on its first start.
func startGateway(ctx context.Context, bin, dir, slow, quick string, env []string, logs io.Writer) (*gateway, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if bin, err = filepath.Abs(bin); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "transponder.yaml")
	if err := os.WriteFile(path, []byte(gatewayConfig(addr, filepath.Join(dir, "transponder.db"), slow, quick)), 0o600); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, "--config", path) //nolint:gosec // running the binary the caller named is the point
	cmd.Dir = dir
	cmd.Stdout = logs
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "TRANSPONDER_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	g := &gateway{cmd: cmd, url: "http://" + addr, done: make(chan struct{}), logs: &tail{}}
	keys := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(stderr)
		next := false
		for s.Scan() {
			line := s.Text()
			g.logs.add(line)
			if next {
				keys <- strings.TrimSpace(line)
				next = false
			}
			next = next || strings.Contains(line, "shown once")
		}
	}()
	go func() {
		_ = cmd.Wait()
		close(g.done)
	}()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	select {
	case g.key = <-keys:
	case <-g.done:
		return nil, fmt.Errorf("%s exited before it printed an admin key:\n%s", bin, g.logs)
	case <-deadline.C:
		g.stop()
		return nil, fmt.Errorf("%s printed no admin key in 30s:\n%s", bin, g.logs)
	case <-ctx.Done():
		g.stop()
		return nil, ctx.Err()
	}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.url+"/healthz", nil)
		if err != nil {
			g.stop()
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return g, nil
			}
		}
		select {
		case <-g.done:
			return nil, fmt.Errorf("%s exited before it answered:\n%s", bin, g.logs)
		case <-deadline.C:
			g.stop()
			return nil, fmt.Errorf("%s did not answer /healthz in 30s:\n%s", bin, g.logs)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// captureProfiles starts a CPU profile for d and returns a function that waits for it and
// then saves the heap, allocation, block and mutex profiles next to it.
func captureProfiles(ctx context.Context, base string, d time.Duration, dir string) func() error {
	save := func(path, name string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s answered %s", path, resp.Status)
		}
		f, err := os.Create(filepath.Join(dir, name)) //nolint:gosec // the directory the caller named
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, resp.Body); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}
	errc := make(chan error, 1)
	go func() {
		errc <- save(fmt.Sprintf("/debug/pprof/profile?seconds=%d", max(1, int(d.Seconds()))), "cpu.pprof")
	}()
	return func() error {
		if err := <-errc; err != nil {
			return err
		}
		for _, p := range []string{"heap", "allocs", "block", "mutex"} {
			if err := save("/debug/pprof/"+p, p+".pprof"); err != nil {
				return err
			}
		}
		return nil
	}
}
