package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// loadConfig is one run of the load tool against one URL.
type loadConfig struct {
	URL  string
	Key  string
	Body []byte
	// Rate is calls per second. Zero sends as fast as Concurrency allows.
	Rate float64
	// Concurrency caps the calls in flight.
	Concurrency int
	Duration    time.Duration
	// Expect must appear in a body for the call to count as answered.
	Expect string
}

// loadResult is what a run measured. Latency holds the answered calls, sorted.
type loadResult struct {
	Sent     int
	Errors   int
	Elapsed  time.Duration
	Latency  []time.Duration
	Failures map[string]int
}

// Throughput is answered calls per second.
func (r loadResult) Throughput() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(len(r.Latency)) / r.Elapsed.Seconds()
}

// Percentile is the nearest-rank percentile of the answered calls' latency, p in (0, 1].
func (r loadResult) Percentile(p float64) time.Duration {
	if len(r.Latency) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(r.Latency)))) - 1
	return r.Latency[max(0, min(i, len(r.Latency)-1))]
}

// newClient keeps enough idle connections that the load tool never opens a connection per call.
func newClient(concurrency int) *http.Client {
	return &http.Client{
		Timeout: time.Minute,
		Transport: &http.Transport{
			MaxIdleConns:        2 * concurrency,
			MaxIdleConnsPerHost: 2 * concurrency,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
	}
}

// drive sends calls for cfg.Duration and measures each from when it was sent to its last byte.
// At a fixed rate a call waits for a free slot when Concurrency are in flight, so a gateway
// that cannot keep up shows as throughput under the rate.
func drive(ctx context.Context, client *http.Client, cfg loadConfig) loadResult {
	res := loadResult{Failures: map[string]int{}}
	var mu sync.Mutex
	expect := []byte(cfg.Expect)
	one := func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(cfg.Body))
		if err != nil {
			mu.Lock()
			res.Sent++
			res.Errors++
			res.Failures[err.Error()]++
			mu.Unlock()
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if cfg.Key != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.Key)
		}
		start := time.Now()
		failure := ""
		resp, err := client.Do(req)
		if err == nil {
			var body []byte
			body, err = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			switch {
			case resp.StatusCode != http.StatusOK:
				failure = "status " + resp.Status
			case err == nil && !bytes.Contains(body, expect):
				failure = "answer without " + cfg.Expect
			}
		}
		took := time.Since(start)
		if err != nil {
			failure = classify(err)
		}
		mu.Lock()
		defer mu.Unlock()
		res.Sent++
		if failure != "" {
			res.Errors++
			res.Failures[failure]++
			return
		}
		res.Latency = append(res.Latency, took)
	}

	start := time.Now()
	end := start.Add(cfg.Duration)
	var wg sync.WaitGroup
	if cfg.Rate > 0 {
		slots := make(chan struct{}, max(1, cfg.Concurrency))
		interval := time.Duration(float64(time.Second) / cfg.Rate)
	pace:
		for i := 0; ; i++ {
			at := start.Add(time.Duration(i) * interval)
			if !at.Before(end) || !wait(ctx, time.Until(at)) {
				break
			}
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				break pace
			}
			wg.Go(func() {
				defer func() { <-slots }()
				one()
			})
		}
	} else {
		for range max(1, cfg.Concurrency) {
			wg.Go(func() {
				for ctx.Err() == nil && time.Now().Before(end) {
					one()
				}
			})
		}
	}
	wg.Wait()
	res.Elapsed = time.Since(start)
	slices.Sort(res.Latency)
	return res
}

// classify keeps transport errors to a few kinds, so a summary groups them.
func classify(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	msg := err.Error()
	for _, kind := range []string{"connection refused", "connection reset", "EOF", "Only one usage of each socket address", "no buffer space"} {
		if strings.Contains(msg, kind) {
			return kind
		}
	}
	return "transport error"
}

// summary is one line about a result.
func summary(r loadResult) string {
	ms := func(d time.Duration) string { return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond)) }
	line := fmt.Sprintf("sent %d, answered %d, errors %d, %.0f req/s, p50 %s, p95 %s, p99 %s",
		r.Sent, len(r.Latency), r.Errors, r.Throughput(), ms(r.Percentile(0.50)), ms(r.Percentile(0.95)), ms(r.Percentile(0.99)))
	if len(r.Failures) > 0 {
		kinds := make([]string, 0, len(r.Failures))
		for k, n := range r.Failures {
			kinds = append(kinds, fmt.Sprintf("%s x%d", k, n))
		}
		sort.Strings(kinds)
		line += " (" + strings.Join(kinds, ", ") + ")"
	}
	return line
}

func runLoad(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg loadConfig
	fs.StringVar(&cfg.URL, "url", "", "URL to POST to")
	fs.StringVar(&cfg.Key, "key", "", "API key sent as a bearer token")
	bodyFile := fs.String("body", "", "file holding the request body")
	fs.Float64Var(&cfg.Rate, "rate", 200, "calls per second, 0 for as fast as -concurrency allows")
	fs.IntVar(&cfg.Concurrency, "concurrency", 256, "calls in flight at most")
	fs.DurationVar(&cfg.Duration, "duration", 10*time.Second, "how long to send")
	warmup := fs.Duration("warmup", 2*time.Second, "how long to send before measuring")
	fs.StringVar(&cfg.Expect, "expect", "", "text an answer must contain")
	pid := fs.Int("pid", 0, "process whose memory to report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.URL == "" || *bodyFile == "" {
		return errors.New("load: -url and -body are required")
	}
	body, err := os.ReadFile(*bodyFile)
	if err != nil {
		return err
	}
	cfg.Body = body
	client := newClient(cfg.Concurrency)
	if *warmup > 0 {
		w := cfg
		w.Duration = *warmup
		drive(ctx, client, w)
	}
	_, _ = fmt.Fprintln(stdout, summary(drive(ctx, client, cfg)))
	if *pid > 0 {
		cur, peak, err := processMemory(*pid)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "memory: %s now, %s peak\n", mib(cur), mib(peak))
	}
	return nil
}

func mib(n uint64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }
