package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// scenario is one kind of call the demo makes, and how often next to the others.
type scenario struct {
	name   string
	weight int
	key    string
	path   string
	model  string
	stream bool
}

// scenarios are chosen so every panel of the dashboard has something to show: two routes,
// streams, embeddings, Responses API calls, a route whose first provider fails, a caller over
// its rate, a caller over its budget, and a few plain mistakes.
var scenarios = []scenario{
	{name: "chat", weight: 35, key: "tp_demo_alice", path: "/v1/chat/completions", model: "fast"},
	{name: "chat stream", weight: 15, key: "tp_demo_bob", path: "/v1/chat/completions", model: "fast", stream: true},
	{name: "responses", weight: 6, key: "tp_demo_alice", path: "/v1/responses", model: "fast"},
	{name: "responses stream", weight: 4, key: "tp_demo_bob", path: "/v1/responses", model: "fast", stream: true},
	{name: "chat with fallback", weight: 20, key: "tp_demo_alice", path: "/v1/chat/completions", model: "smart"},
	{name: "stream with fallback", weight: 5, key: "tp_demo_bob", path: "/v1/chat/completions", model: "smart", stream: true},
	{name: "embeddings over the rate", weight: 15, key: "tp_demo_batch", path: "/v1/embeddings", model: "embed"},
	{name: "chat over the budget", weight: 5, key: "tp_demo_trial", path: "/v1/chat/completions", model: "smart"},
	{name: "unknown model", weight: 3, key: "tp_demo_alice", path: "/v1/chat/completions", model: "no-such-model"},
	{name: "wrong key", weight: 2, key: "tp_demo_nobody", path: "/v1/chat/completions", model: "fast"},
}

var prompts = []string{
	"Say hello.",
	"Summarise the plot of a film about a lighthouse keeper in two sentences.",
	"List five things to check before a database migration, and say why each one matters when the table is large and the service must stay up.",
	"Translate 'the quick brown fox jumps over the lazy dog' into French, Spanish and German, then explain one grammar point in each.",
}

// pick returns the scenario at r, a number in [0, 1), by weight.
func pick(r float64) scenario {
	total := 0
	for _, s := range scenarios {
		total += s.weight
	}
	at := int(r * float64(total))
	for _, s := range scenarios {
		if at < s.weight {
			return s
		}
		at -= s.weight
	}
	return scenarios[len(scenarios)-1]
}

// body returns the request body of s with the prompt at r.
func (s scenario) body(r float64) []byte {
	prompt := prompts[int(r*float64(len(prompts)))%len(prompts)]
	var v map[string]any
	switch s.path {
	case "/v1/embeddings":
		v = map[string]any{"model": s.model, "input": prompt}
	case "/v1/responses":
		v = map[string]any{"model": s.model, "stream": s.stream, "max_output_tokens": 100, "input": prompt}
	default:
		// With limits on, a call reserves the price of its output limit before it runs, so
		// a small one keeps the demo's budget from being spent by the first reservation.
		v = map[string]any{
			"model": s.model, "stream": s.stream, "max_tokens": 100,
			"messages": []any{map[string]string{"role": "user", "content": prompt}},
		}
	}
	b, _ := json.Marshal(v)
	return b
}

// traffic sends calls to a gateway at a steady rate and counts the answers by status.
type traffic struct {
	base   string
	client *http.Client
	rate   float64 // calls per second
	random func() float64
	out    io.Writer
	report time.Duration

	mu     sync.Mutex
	counts map[int]int
}

func runTraffic(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("traffic", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("url", "http://localhost:8080", "base URL of transponder")
	rate := fs.Float64("rate", 4, "calls per second")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *rate <= 0 || *rate > 1000 {
		return errors.New("traffic: -rate must be between 0 and 1000 calls per second")
	}
	t := &traffic{
		base: strings.TrimSuffix(*base, "/"), client: &http.Client{Timeout: time.Minute}, rate: *rate,
		random: rand.Float64, out: stdout, report: 10 * time.Second,
	}
	if err := t.waitReady(ctx, time.Minute); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "traffic: sending %.1f calls a second to %s\n", *rate, t.base)
	t.run(ctx)
	return nil
}

// waitReady waits until the gateway's /readyz answers 200.
func (t *traffic) waitReady(ctx context.Context, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base+"/readyz", nil)
		if err != nil {
			return err
		}
		resp, err := t.client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("transponder at %s was not ready within %s", t.base, limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// run sends calls until ctx ends, then waits for the ones in flight.
func (t *traffic) run(ctx context.Context) {
	tick := time.NewTicker(time.Duration(float64(time.Second) / t.rate))
	defer tick.Stop()
	report := time.NewTicker(t.report)
	defer report.Stop()
	// A slow gateway must not pile up calls without end.
	slots := make(chan struct{}, 64)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-report.C:
			t.summary()
		case <-tick.C:
			s, body := pick(t.random()), t.random()
			select {
			case slots <- struct{}{}:
			default:
				continue
			}
			wg.Go(func() {
				defer func() { <-slots }()
				t.send(ctx, s, s.body(body))
			})
		}
	}
}

// send makes one call and reads the whole answer, as a client of a stream would.
func (t *traffic) send(ctx context.Context, s scenario, body []byte) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+s.path, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Set("Content-Type", "application/json")
	status := 0 // no answer at all
	if resp, err := t.client.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		status = resp.StatusCode
	}
	t.mu.Lock()
	if t.counts == nil {
		t.counts = map[int]int{}
	}
	t.counts[status]++
	t.mu.Unlock()
}

// summary prints how many answers of each status have come back so far.
func (t *traffic) summary() {
	t.mu.Lock()
	codes := make([]int, 0, len(t.counts))
	for code := range t.counts {
		codes = append(codes, code)
	}
	sort.Ints(codes)
	parts := make([]string, len(codes))
	for i, code := range codes {
		parts[i] = fmt.Sprintf("%d: %d", code, t.counts[code])
	}
	t.mu.Unlock()
	_, _ = fmt.Fprintf(t.out, "traffic: answers by status so far: %s\n", strings.Join(parts, ", "))
}
