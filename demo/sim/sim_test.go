package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/pricing"
	oaprovider "github.com/raven-clown/transponder/internal/provider/openai"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/server"
	"github.com/raven-clown/transponder/internal/store/sqlite"
	"github.com/raven-clown/transponder/internal/telemetry"
)

// quick is an upstream that answers at once, with every random choice in the middle.
func quick(failRate float64) *upstream {
	return &upstream{failRate: failRate, random: func() float64 { return 0.5 }}
}

func post(t *testing.T, h http.Handler, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const chatBody = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Say hello to the demo."}]}`

func TestUpstreamChat(t *testing.T) {
	rec := post(t, quick(0).handler(), "/v1/chat/completions", "", chatBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role, Content string
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "gpt-4o-mini" || len(out.Choices) != 1 || out.Choices[0].Message.Role != "assistant" ||
		out.Choices[0].Message.Content == "" || out.Choices[0].FinishReason != "stop" {
		t.Fatalf("answer %s", rec.Body)
	}
	// The gateway reads usage the way it does from a real provider.
	u, ok := openai.ParseUsage(rec.Body.Bytes())
	if !ok || u.InputTokens == 0 || u.OutputTokens == 0 {
		t.Fatalf("usage %+v, found %v", u, ok)
	}
}

func TestUpstreamStream(t *testing.T) {
	rec := post(t, quick(0).handler(), "/v1/chat/completions", "", `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("the stream does not end with [DONE]:\n%s", rec.Body)
	}
	rd := openai.NewSSEReader(rec.Body)
	var text strings.Builder
	chunks, usageOnly := 0, 0
	for {
		data, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		chunks++
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			t.Fatalf("chunk %s: %v", data, err)
		}
		for _, c := range chunk.Choices {
			text.WriteString(c.Delta.Content)
		}
		if u, ok, only := openai.ChunkUsage(data); ok {
			if !only || u.InputTokens == 0 || u.OutputTokens == 0 {
				t.Fatalf("usage chunk %s", data)
			}
			usageOnly++
		}
	}
	if chunks < 4 || usageOnly != 1 || !strings.HasPrefix(answer, text.String()) || text.Len() == 0 {
		t.Fatalf("%d chunks, %d with usage, text %q", chunks, usageOnly, text.String())
	}
}

func TestUpstreamResponses(t *testing.T) {
	const body = `{"model":"gpt-4o-mini","input":"Say hello to the demo."}`
	t.Run("plain", func(t *testing.T) {
		rec := post(t, quick(0).handler(), "/v1/responses", "", body)
		var out struct {
			Object string `json:"object"`
			Status string `json:"status"`
			Output []struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		if out.Object != "response" || out.Status != "completed" || len(out.Output) != 1 ||
			len(out.Output[0].Content) != 1 || out.Output[0].Content[0].Type != "output_text" || !strings.HasPrefix(answer, out.Output[0].Content[0].Text) {
			t.Fatalf("answer %+v", out)
		}
		assertResponsesUsage(t, rec.Body.Bytes())
	})
	t.Run("stream", func(t *testing.T) {
		rec := post(t, quick(0).handler(), "/v1/responses", "", `{"model":"gpt-4o-mini","stream":true,"input":"hi"}`)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
			t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
		}
		var lastEvent, lastData string
		var deltas int
		for _, block := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n") {
			event, data, ok := strings.Cut(block, "\ndata: ")
			if !ok || !strings.HasPrefix(event, "event: ") {
				t.Fatalf("block that is not an event: %q", block)
			}
			if event == "event: response.output_text.delta" {
				deltas++
			}
			lastEvent, lastData = event, data
		}
		if deltas == 0 || lastEvent != "event: response.completed" {
			t.Fatalf("%d deltas, the last event %q", deltas, lastEvent)
		}
		var completed struct {
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal([]byte(lastData), &completed); err != nil {
			t.Fatal(err)
		}
		assertResponsesUsage(t, completed.Response)
	})
}

// assertResponsesUsage checks the usage of a Responses API response, with its cached and
// reasoning tokens, the way the gateway reads it.
func assertResponsesUsage(t *testing.T, body []byte) {
	t.Helper()
	var out struct {
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokens        int `json:"output_tokens"`
			OutputTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	u := out.Usage
	if u.InputTokens == 0 || u.OutputTokens == 0 || u.TotalTokens != u.InputTokens+u.OutputTokens {
		t.Fatalf("usage %+v", u)
	}
	if u.InputTokensDetails.CachedTokens > u.InputTokens || u.OutputTokensDetails.ReasoningTokens == 0 {
		t.Fatalf("usage %+v has no cached or reasoning tokens in range", u)
	}
}

func TestUpstreamEmbeddings(t *testing.T) {
	for name, tt := range map[string]struct {
		input string
		want  int
	}{
		"one string": {`"hello"`, 1},
		"a list":     {`["hello","there","demo"]`, 3},
	} {
		t.Run(name, func(t *testing.T) {
			rec := post(t, quick(0).handler(), "/v1/embeddings", "", `{"model":"text-embedding-3-small","input":`+tt.input+`}`)
			var out struct {
				Data []struct {
					Index     int       `json:"index"`
					Embedding []float64 `json:"embedding"`
				} `json:"data"`
			}
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Data) != tt.want {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			for i, d := range out.Data {
				if d.Index != i || len(d.Embedding) != 8 {
					t.Fatalf("item %d: %+v", i, d)
				}
			}
			if u, ok := openai.ParseUsage(rec.Body.Bytes()); !ok || u.InputTokens == 0 {
				t.Fatalf("usage %+v, found %v", u, ok)
			}
		})
	}
}

func TestUpstreamRefusals(t *testing.T) {
	h := quick(0).handler()
	for name, tt := range map[string]struct {
		path, body string
	}{
		"chat that is not JSON":       {"/v1/chat/completions", "{"},
		"chat without a model":        {"/v1/chat/completions", `{"messages":[]}`},
		"responses that are not JSON": {"/v1/responses", "{"},
		"responses without a model":   {"/v1/responses", `{"input":"x"}`},
		"embeddings that are no JSON": {"/v1/embeddings", "nope"},
		"embeddings without a model":  {"/v1/embeddings", `{"input":"x"}`},
		"embeddings without input":    {"/v1/embeddings", `{"model":"m"}`},
		"embeddings of an empty list": {"/v1/embeddings", `{"model":"m","input":[]}`},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := post(t, h, tt.path, "", tt.body); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_request_error") {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
		})
	}
	// An upstream set to fail answers 503 for chat and for embeddings.
	failing := quick(1).handler()
	for _, tt := range []struct{ path, body string }{
		{"/v1/chat/completions", chatBody},
		{"/v1/embeddings", `{"model":"m","input":"x"}`},
	} {
		if rec := post(t, failing, tt.path, "", tt.body); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status %d, want 503", tt.path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: status %d", rec.Code)
	}
}

// A caller that leaves while the upstream is still waiting gets no answer written.
func TestUpstreamStopsWhenTheCallerLeaves(t *testing.T) {
	u := &upstream{minLatency: time.Hour, maxLatency: time.Hour, random: func() float64 { return 0.5 }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
	rec := httptest.NewRecorder()
	u.handler().ServeHTTP(rec, req)
	if rec.Body.Len() != 0 {
		t.Fatalf("wrote %q to a caller that had left", rec.Body)
	}
}

func TestSeedIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	for i, want := range []int{len(demoKeys), 0} {
		created, err := seed(ctx, path, time.Now())
		if err != nil || created != want {
			t.Fatalf("run %d: %d keys created, err %v; want %d", i+1, created, err, want)
		}
	}
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	authn := auth.NewAuthenticator(db, time.Now)
	for _, k := range demoKeys {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+k.secret())
		id, err := authn.Authenticate(req)
		if err != nil || id.Key != k.name || id.Role != k.role {
			t.Errorf("key %s: identity %+v, err %v", k.name, id, err)
		}
	}
	// The gateway still makes its own admin key at first start: the demo adds none.
	if has, err := db.HasAdminKey(ctx, "default", time.Now()); err != nil || has {
		t.Fatalf("an admin key was seeded: %v, err %v", has, err)
	}
	if _, err := seed(ctx, filepath.Join(t.TempDir(), "missing", "t.db"), time.Now()); err == nil {
		t.Fatal("seeding a store in a folder that does not exist returned no error")
	}
}

// demoGateway is transponder with the routes of the demo, in front of one fake upstream.
func demoGateway(t *testing.T) (http.Handler, *telemetry.Metrics) {
	t.Helper()
	ctx := context.Background()
	up := httptest.NewServer(quick(0).handler())
	t.Cleanup(up.Close)
	path := filepath.Join(t.TempDir(), "t.db")
	if _, err := seed(ctx, path, time.Now()); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	steady := oaprovider.New(oaprovider.Options{Name: "steady", BaseURL: up.URL + "/v1", APIKey: "demo", Client: up.Client()})
	target := func(model string) []router.Target {
		return []router.Target{{Provider: steady, Type: "openai", Model: model}}
	}
	m := telemetry.NewMetrics("test")
	return server.New(server.Deps{
		Auth: auth.NewAuthenticator(db, time.Now),
		Router: router.New([]router.Route{
			{Match: "fast", Targets: target("gpt-4o-mini")},
			{Match: "embed", Targets: target("text-embedding-3-small")},
		}),
		Prices:  pricing.Default(),
		Usage:   db,
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Now:     time.Now,
		Metrics: m,
	}), m
}

// The fake upstream has to pass for a provider: the gateway must read its answers, its
// streams and its usage, or the dashboard stays empty.
func TestGatewayReadsTheUpstream(t *testing.T) {
	h, m := demoGateway(t)
	for _, s := range scenarios {
		if s.model != "fast" && s.model != "embed" || s.key == "tp_demo_nobody" {
			continue
		}
		rec := post(t, h, s.path, s.key, string(s.body(0.6)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", s.name, rec.Code, rec.Body)
		}
		if rec.Header().Get("X-Transponder-Provider") != "steady" {
			t.Fatalf("%s: answered by %q", s.name, rec.Header().Get("X-Transponder-Provider"))
		}
		finish := "[DONE]"
		if s.path == "/v1/responses" {
			finish = "event: response.completed"
		}
		if s.stream && !strings.Contains(rec.Body.String(), finish) {
			t.Fatalf("%s: the stream did not finish:\n%s", s.name, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	got := rec.Body.String()
	for _, want := range []string{
		`transponder_requests_total{endpoint="chat.completions",route="fast",provider="steady",model="gpt-4o-mini",code="200"} 2`,
		`transponder_requests_total{endpoint="responses",route="fast",provider="steady",model="gpt-4o-mini",code="200"} 2`,
		`transponder_requests_total{endpoint="embeddings",route="embed",provider="steady",model="text-embedding-3-small",code="200"} 1`,
		`transponder_tokens_total{direction="output",provider="steady",model="gpt-4o-mini"} `,
		`transponder_tokens_total{direction="input",provider="steady",model="text-embedding-3-small"} `,
		`transponder_cost_usd_total{provider="steady",model="gpt-4o-mini"} `,
		`transponder_unpriced_requests_total 0`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if t.Failed() {
		t.Fatalf("metrics:\n%s", got)
	}
}

// The config, the keys and the traffic are three files that have to agree.
func TestDemoConfigMatchesTheTraffic(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "transponder.yaml"), nil)
	if err != nil {
		t.Fatalf("demo/transponder.yaml does not load: %v", err)
	}
	if cfg.OpsListenAddr == "" || cfg.OTel.Endpoint == "" {
		t.Fatalf("the demo config has no ops port or no collector: %+v, %+v", cfg.OpsListenAddr, cfg.OTel)
	}
	routes := map[string]bool{}
	for _, r := range cfg.Routes {
		routes[r.Match] = true
	}
	keys := map[string]bool{}
	for _, k := range demoKeys {
		keys[k.secret()] = true
	}
	for _, s := range scenarios {
		if known := routes[s.model]; known != (s.model != "no-such-model") {
			t.Errorf("scenario %q asks for model %q; the config has a route for it: %v", s.name, s.model, known)
		}
		if known := keys[s.key]; known != (s.key != "tp_demo_nobody") {
			t.Errorf("scenario %q uses key %q; it is a demo key: %v", s.name, s.key, known)
		}
	}
	names := map[string]bool{}
	for _, k := range demoKeys {
		names["key:"+k.name] = true
	}
	for _, b := range cfg.ProfileBindings {
		if !names[b.Subject] {
			t.Errorf("profile binding for %q, which is not a demo key", b.Subject)
		}
	}
	for _, b := range cfg.Budgets {
		if !names[b.Subject] {
			t.Errorf("budget for %q, which is not a demo key", b.Subject)
		}
	}
	if len(cfg.ProfileBindings) == 0 || len(cfg.Budgets) == 0 {
		t.Error("the demo config has no rate limit or no budget, so the limits panel would stay empty")
	}
}

var metricName = regexp.MustCompile(`transponder_[a-z_]+`)

// Every query of the dashboard must name a metric the gateway serves, and the dashboard
// must cover what the demo promises to show.
func TestDashboardQueriesExistingMetrics(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "grafana", "dashboards", "transponder.json"))
	if err != nil {
		t.Fatal(err)
	}
	var dash struct {
		UID    string `json:"uid"`
		Panels []struct {
			Title      string `json:"title"`
			Datasource struct {
				UID string `json:"uid"`
			} `json:"datasource"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("the dashboard is not valid JSON: %v", err)
	}
	rec := httptest.NewRecorder()
	telemetry.NewMetrics("test").Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	served := rec.Body.String()
	sources, err := os.ReadFile(filepath.Join("..", "grafana", "provisioning", "datasources", "datasources.yml"))
	if err != nil {
		t.Fatal(err)
	}

	used := map[string]bool{}
	for _, p := range dash.Panels {
		if len(p.Targets) == 0 {
			t.Errorf("panel %q has no query", p.Title)
		}
		if !strings.Contains(string(sources), "uid: "+p.Datasource.UID+"\n") {
			t.Errorf("panel %q reads from data source %q, which is not provisioned", p.Title, p.Datasource.UID)
		}
		for _, target := range p.Targets {
			names := metricName.FindAllString(target.Expr, -1)
			if len(names) == 0 {
				t.Errorf("panel %q: the query %q names no transponder metric", p.Title, target.Expr)
			}
			for _, name := range names {
				// A histogram is queried through its _bucket series.
				name = strings.TrimSuffix(name, "_bucket")
				used[name] = true
				if !strings.Contains(served, "# TYPE "+name+" ") {
					t.Errorf("panel %q queries %s, which the gateway does not serve", p.Title, name)
				}
			}
		}
	}
	for _, want := range []string{
		"transponder_requests_total", "transponder_request_duration_seconds", "transponder_inflight_requests",
		"transponder_tokens_total", "transponder_cost_usd_total", "transponder_fallbacks_total", "transponder_limited_total",
	} {
		if !used[want] {
			t.Errorf("no panel shows %s", want)
		}
	}
	if dash.UID != "transponder" {
		t.Errorf("dashboard uid %q; the README links to /d/transponder", dash.UID)
	}
}

func TestPick(t *testing.T) {
	if got := pick(0); got.name != scenarios[0].name {
		t.Errorf("pick(0) = %q, want the first scenario", got.name)
	}
	if got := pick(0.999999); got.name != scenarios[len(scenarios)-1].name {
		t.Errorf("pick(0.999999) = %q, want the last scenario", got.name)
	}
	seen := map[string]int{}
	const steps = 1000
	for i := range steps {
		seen[pick(float64(i)/steps).name]++
	}
	total := 0
	for _, s := range scenarios {
		total += s.weight
	}
	for _, s := range scenarios {
		if want := steps * s.weight / total; seen[s.name] < want-2 || seen[s.name] > want+2 {
			t.Errorf("scenario %q picked %d times in %d, want about %d", s.name, seen[s.name], steps, want)
		}
	}
}

func TestScenarioBodies(t *testing.T) {
	for _, s := range scenarios {
		var body map[string]any
		if err := json.Unmarshal(s.body(0.3), &body); err != nil || body["model"] != s.model {
			t.Fatalf("%s: body %s, err %v", s.name, s.body(0.3), err)
		}
		_, hasInput := body["input"]
		if wantInput := s.path == "/v1/embeddings" || s.path == "/v1/responses"; wantInput != hasInput {
			t.Errorf("%s: path %s, input present %v", s.name, s.path, hasInput)
		}
		switch s.path {
		case "/v1/responses":
			if body["stream"] != s.stream || body["max_output_tokens"] != float64(100) {
				t.Errorf("%s: stream %v, max_output_tokens %v", s.name, body["stream"], body["max_output_tokens"])
			}
		case "/v1/chat/completions":
			if body["stream"] != s.stream || body["max_tokens"] != float64(100) {
				t.Errorf("%s: stream %v, max_tokens %v", s.name, body["stream"], body["max_tokens"])
			}
		}
	}
	// The top of the range still names a prompt.
	if len(scenarios[0].body(0.999999)) == 0 {
		t.Fatal("no body at the top of the range")
	}
}

// gateway is a stand-in for transponder that counts what the traffic sends it.
type gateway struct {
	mu    sync.Mutex
	paths map[string]int
	keys  map[string]int
	ready bool
	full  chan struct{}
	want  int
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.URL.Path == "/readyz" {
		if !g.ready {
			// Ready from the second probe on, as a gateway that is still starting.
			g.ready = true
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		return
	}
	g.paths[r.URL.Path]++
	g.keys[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]++
	if total := g.paths["/v1/chat/completions"] + g.paths["/v1/embeddings"]; total == g.want {
		close(g.full)
	}
	if r.Header.Get("Authorization") == "Bearer tp_demo_nobody" {
		w.WriteHeader(http.StatusUnauthorized)
	}
}

func TestTraffic(t *testing.T) {
	g := &gateway{paths: map[string]int{}, keys: map[string]int{}, full: make(chan struct{}), want: 300}
	srv := httptest.NewServer(g)
	defer srv.Close()
	var out bytes.Buffer
	n := 0
	tr := &traffic{
		base: srv.URL, client: srv.Client(), rate: 1000, out: &out, report: time.Hour,
		// Walks through every scenario and prompt in turn.
		random: func() float64 { n++; return float64(n%97) / 97 },
	}
	if err := tr.waitReady(context.Background(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.run(ctx)
	}()
	select {
	case <-g.full:
	case <-time.After(30 * time.Second):
		t.Fatal("the traffic did not reach 300 calls")
	}
	cancel()
	<-done
	tr.summary()

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paths["/v1/chat/completions"] == 0 || g.paths["/v1/embeddings"] == 0 || g.paths["/v1/responses"] == 0 {
		t.Fatalf("paths called: %v", g.paths)
	}
	for _, s := range scenarios {
		if g.keys[s.key] == 0 {
			t.Errorf("no call was made with the key of scenario %q", s.name)
		}
	}
	if !strings.Contains(out.String(), "answers by status so far: ") || !strings.Contains(out.String(), "200: ") || !strings.Contains(out.String(), "401: ") {
		t.Fatalf("summary %q", out.String())
	}
}

func TestTrafficWaitsForTheGateway(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	tr := &traffic{base: down.URL, client: down.Client()}
	if err := tr.waitReady(context.Background(), time.Millisecond); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("err = %v, want it to say the gateway was not ready", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tr.waitReady(ctx, time.Hour); err == nil {
		t.Fatal("waitReady returned no error after its context ended")
	}
	// A call that cannot be sent is counted as having no answer.
	down.Close()
	tr.send(context.Background(), scenarios[0], scenarios[0].body(0))
	if tr.counts[0] != 1 {
		t.Fatalf("counts %v, want one call with no answer", tr.counts)
	}
}

// stopOnStart ends the run once the traffic says it has started.
type stopOnStart struct {
	bytes.Buffer
	stop context.CancelFunc
}

func (s *stopOnStart) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("traffic: sending")) {
		s.stop()
	}
	return s.Buffer.Write(p)
}

func TestRunTrafficUntilStopped(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ok.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout := &stopOnStart{stop: cancel}
	var stderr bytes.Buffer
	// A trailing slash on the URL is taken off.
	if code := run(ctx, []string{"traffic", "-url", ok.URL + "/", "-rate", "50"}, stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d: %s", code, stderr.String())
	}
	if want := "traffic: sending 50.0 calls a second to " + ok.URL + "\n"; !strings.Contains(stdout.String(), want) {
		t.Fatalf("output %q, want it to contain %q", stdout.String(), want)
	}
}

func TestRun(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ok.Close()
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer refusing.Close()
	db := filepath.Join(t.TempDir(), "t.db")
	stopped, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name   string
		ctx    context.Context
		args   []string
		code   int
		output string
	}{
		{"no command", nil, nil, 2, "usage: sim"},
		{"unknown command", nil, []string{"dance"}, 2, `unknown command "dance"`},
		{"help", nil, []string{"seed", "-h"}, 2, "-db"},
		{"probe ok", nil, []string{"probe", ok.URL}, 0, ""},
		{"probe refused", nil, []string{"probe", refusing.URL}, 1, "503"},
		{"probe without a URL", nil, []string{"probe"}, 1, "one URL"},
		{"probe of nothing", nil, []string{"probe", "http://127.0.0.1:1/"}, 1, "sim:"},
		{"seed", nil, []string{"seed", "-db", db}, 0, "5 of 5 demo keys created"},
		{"seed again", nil, []string{"seed", "-db", db}, 0, "0 of 5 demo keys created"},
		{"seed nowhere", nil, []string{"seed", "-db", filepath.Join(t.TempDir(), "missing", "t.db")}, 1, "sim:"},
		{"upstream with a bad rate", nil, []string{"upstream", "-fail", "2"}, 1, "-fail must be between 0 and 1"},
		{"upstream with a bad address", nil, []string{"upstream", "-addr", "not-an-address"}, 1, "sim:"},
		{"upstream until stopped", stopped, []string{"upstream", "-addr", "127.0.0.1:0", "-min-latency", "0", "-max-latency", "0"}, 0, "upstream: listening on"},
		{"traffic with a bad rate", nil, []string{"traffic", "-rate", "0"}, 1, "-rate must be"},
		{"traffic stopped while waiting", stopped, []string{"traffic", "-url", refusing.URL}, 1, "sim:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			var stdout, stderr bytes.Buffer
			code := run(ctx, tt.args, &stdout, &stderr)
			if all := stdout.String() + stderr.String(); code != tt.code || !strings.Contains(all, tt.output) {
				t.Fatalf("exit code %d, want %d; output %q, want it to contain %q", code, tt.code, all, tt.output)
			}
		})
	}
}
