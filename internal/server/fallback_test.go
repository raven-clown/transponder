package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/provider/anthropic"
	oaprovider "github.com/raven-clown/transponder/internal/provider/openai"
	"github.com/raven-clown/transponder/internal/router"
)

const okChat = `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"from b"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500}}`

// counted is a fake upstream that counts its calls.
type counted struct {
	srv   *httptest.Server
	calls atomic.Int64
}

func countedUpstream(t *testing.T, h http.HandlerFunc) *counted {
	t.Helper()
	c := &counted{}
	c.srv = upstream(t, func(w http.ResponseWriter, r *http.Request) {
		c.calls.Add(1)
		h(w, r)
	})
	return c
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func oaTarget(name, model string, up *counted) router.Target {
	return router.Target{Provider: oaprovider.New(oaprovider.Options{Name: name, BaseURL: up.srv.URL, APIKey: "k", Client: up.srv.Client()}), Type: "openai", Model: model}
}

func anTarget(model string, up *counted) router.Target {
	return router.Target{Provider: anthropic.New("claude", up.srv.URL, "k", up.srv.Client()), Type: "anthropic", Model: model}
}

func route(match string, targets ...router.Target) []router.Route {
	return []router.Route{{Match: match, Targets: targets}}
}

func TestFallbackOnServerError(t *testing.T) {
	a := countedUpstream(t, status(500, "oops"))
	b := countedUpstream(t, status(200, okChat))
	e := setupRoutes(t, route("*", oaTarget("a", "gpt-5", a), oaTarget("b", "gpt-4o-mini", b)))
	res := e.post(t, ping)
	if res.status != http.StatusOK || !strings.Contains(string(res.body), "from b") {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	h := res.header
	if h.Get("X-Transponder-Provider") != "b" || h.Get("X-Transponder-Model") != "gpt-4o-mini" || h.Get("X-Transponder-Fallbacks") != "1" || h.Get("X-Usage-Cost-USD") != "0.000450" {
		t.Fatalf("headers %v", h)
	}
	recs := e.usage(t)
	if len(recs) != 1 || recs[0].Provider != "b" || recs[0].Model != "gpt-4o-mini" || recs[0].Fallbacks != 1 || recs[0].Status != 200 || recs[0].CostNUSD != 450_000 {
		t.Fatalf("records %+v", recs)
	}
	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Fatalf("calls a=%d b=%d", a.calls.Load(), b.calls.Load())
	}
}

func TestFallbackReasons(t *testing.T) {
	closed := countedUpstream(t, status(200, ""))
	closed.srv.Close()
	for name, first := range map[string]*counted{
		"429":                countedUpstream(t, status(429, "{}")),
		"401 from upstream":  countedUpstream(t, status(401, "{}")),
		"529":                countedUpstream(t, status(529, "{}")),
		"connection refused": closed,
	} {
		t.Run(name, func(t *testing.T) {
			b := countedUpstream(t, status(200, okChat))
			e := setupRoutes(t, route("*", oaTarget("a", "", first), oaTarget("b", "", b)))
			if res := e.post(t, ping); res.status != http.StatusOK || res.header.Get("X-Transponder-Fallbacks") != "1" {
				t.Fatalf("status %d, headers %v: %s", res.status, res.header, res.body)
			}
		})
	}
}

func TestNoFallbackOnBadRequest(t *testing.T) {
	for _, code := range []int{400, 404, 422} {
		a := countedUpstream(t, status(code, `{"error":{"message":"bad"}}`))
		b := countedUpstream(t, status(200, okChat))
		e := setupRoutes(t, route("*", oaTarget("a", "", a), oaTarget("b", "", b)))
		res := e.post(t, ping)
		if res.status != code || b.calls.Load() != 0 {
			t.Fatalf("%d: status %d, b called %d times", code, res.status, b.calls.Load())
		}
		if r := e.usage(t)[0]; r.Provider != "a" || r.Fallbacks != 0 {
			t.Fatalf("record %+v", r)
		}
	}
}

func TestAllTargetsFail(t *testing.T) {
	a := countedUpstream(t, status(500, "x"))
	b := countedUpstream(t, status(503, "y"))
	e := setupRoutes(t, route("*", oaTarget("a", "", a), oaTarget("b", "", b)))
	res := e.post(t, ping)
	if res.status != http.StatusBadGateway || errorCode(t, res) != "upstream_error" || res.header.Get("X-Transponder-Provider") != "b" {
		t.Fatalf("status %d, headers %v: %s", res.status, res.header, res.body)
	}
	recs := e.usage(t)
	if len(recs) != 1 || recs[0].Provider != "b" || recs[0].Fallbacks != 1 || recs[0].Status != 502 {
		t.Fatalf("records %+v", recs)
	}
}

func TestStreamFallbackBeforeFirstEvent(t *testing.T) {
	for name, first := range map[string]http.HandlerFunc{
		"error status": status(500, "x"),
		"empty stream": func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("Content-Type", "text/event-stream") },
	} {
		t.Run(name, func(t *testing.T) {
			a := countedUpstream(t, first)
			b := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, upstreamChunks) })
			e := setupRoutes(t, route("*", oaTarget("a", "", a), oaTarget("b", "gpt-4o-mini", b)))
			res := e.post(t, streamPing)
			p := payloads(t, res.body)
			if res.status != http.StatusOK || len(p) != 4 || p[3] != "[DONE]" || res.header.Get("X-Transponder-Provider") != "b" {
				t.Fatalf("status %d, headers %v, payloads %q", res.status, res.header, p)
			}
			checkTrailers(t, res, "1000", "500", "0.000450")
			if r := e.usage(t)[0]; r.Provider != "b" || r.Fallbacks != 1 {
				t.Fatalf("record %+v", r)
			}
		})
	}
}

func TestStreamNoFallbackAfterFirstByte(t *testing.T) {
	a := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
	})
	b := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, upstreamChunks) })
	e := setupRoutes(t, route("*", oaTarget("a", "", a), oaTarget("b", "", b)))
	res := e.post(t, streamPing)
	p := payloads(t, res.body)
	if len(p) != 2 || !strings.Contains(p[1], "upstream_error") || b.calls.Load() != 0 {
		t.Fatalf("payloads %q, b called %d times", p, b.calls.Load())
	}
}

func TestMessagesFallbackAcrossDialects(t *testing.T) {
	a := countedUpstream(t, status(529, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
	b := countedUpstream(t, status(200, okChat))
	e := setupRoutes(t, route("claude-*", anTarget("", a), oaTarget("b", "gpt-4o-mini", b)))
	res := e.messages(t, nil, `{"model":"claude-sonnet-5-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if res.status != http.StatusOK || !strings.Contains(string(res.body), `"type":"message"`) || !strings.Contains(string(res.body), "from b") {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	if r := e.usage(t)[0]; r.Provider != "b" || r.Fallbacks != 1 || r.Route != "claude-*" {
		t.Fatalf("record %+v", r)
	}

	stream := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, upstreamChunks) })
	e = setupRoutes(t, route("*", anTarget("", a), oaTarget("b", "", stream)))
	res = e.messages(t, nil, `{"model":"m","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if res.status != http.StatusOK || !strings.Contains(string(res.body), "event: message_stop") || !strings.Contains(string(res.body), `"text":"hi"`) {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
}

func TestEmbeddingsSkipTargetWithoutEmbeddings(t *testing.T) {
	a := countedUpstream(t, status(200, "{}"))
	b := countedUpstream(t, status(200, `{"object":"list","data":[{"index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":3}}`))
	e := setupRoutes(t, route("*", anTarget("", a), oaTarget("b", "", b)))
	res := e.embed(t, e.auth(), `{"model":"e","input":"hello"}`)
	if res.status != http.StatusOK || a.calls.Load() != 0 || res.header.Get("X-Transponder-Provider") != "b" {
		t.Fatalf("status %d, a called %d times: %s", res.status, a.calls.Load(), res.body)
	}
	only := setupRoutes(t, route("*", anTarget("", a)))
	res = only.embed(t, only.auth(), `{"model":"e","input":"hello"}`)
	if res.status != http.StatusBadRequest || errorCode(t, res) != "unsupported" {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
}

func TestIncompatibleTargets(t *testing.T) {
	image := `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}]}`
	down := countedUpstream(t, status(500, "x"))
	oa := countedUpstream(t, status(200, okChat))

	// The Anthropic target is down and OpenAI cannot take the image: the outage is the answer.
	e := setupRoutes(t, route("*", anTarget("", down), oaTarget("b", "", oa)))
	res := e.messages(t, nil, image)
	if res.status != http.StatusBadGateway || oa.calls.Load() != 0 {
		t.Fatalf("status %d, openai called %d times: %s", res.status, oa.calls.Load(), res.body)
	}

	// OpenAI cannot take it, the Anthropic target after it can.
	up := countedUpstream(t, status(200, `{"type":"message","content":[{"type":"text","text":"a cat"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	e = setupRoutes(t, route("*", oaTarget("b", "", oa), anTarget("", up)))
	res = e.messages(t, nil, image)
	if res.status != http.StatusOK || !strings.Contains(string(res.body), "a cat") || oa.calls.Load() != 0 {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
}

func TestNoFallbackAfterCallerLeaves(t *testing.T) {
	release := make(chan struct{})
	a := countedUpstream(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	b := countedUpstream(t, status(200, okChat))
	e := setupRoutes(t, route("*", oaTarget("a", "", a), oaTarget("b", "", b)))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+"/v1/chat/completions", strings.NewReader(ping))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	resp, err := e.srv.Client().Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the request did not time out")
	}
	close(release)
	e.srv.Close()
	if b.calls.Load() != 0 {
		t.Fatalf("b was called %d times after the caller left", b.calls.Load())
	}
	recs := e.usage(t)
	if len(recs) != 1 || recs[0].Provider != "a" || recs[0].Fallbacks != 0 {
		t.Fatalf("the gateway moved on after the caller left: %+v", recs)
	}
}

func TestRequestDeadlineStopsFallback(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	a := countedUpstream(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	b := countedUpstream(t, status(200, okChat))
	e := setupWith(t, route("*", oaTarget("a", "", a), oaTarget("b", "", b)), 150*time.Millisecond, 0)
	res := e.post(t, ping)
	if res.status != http.StatusBadGateway || b.calls.Load() != 0 || res.header.Get("X-Transponder-Provider") != "a" {
		t.Fatalf("status %d, headers %v, b called %d times", res.status, res.header, b.calls.Load())
	}
	if r := e.usage(t)[0]; r.Provider != "a" || r.Fallbacks != 0 {
		t.Fatalf("the gateway moved on after the deadline: %+v", r)
	}
}

func TestCooldownSkipsFailingTarget(t *testing.T) {
	a := countedUpstream(t, status(500, "x"))
	b := countedUpstream(t, status(200, okChat))
	e := setupRoutes(t, route("*", oaTarget("a", "", a), oaTarget("b", "", b)))
	for i := range 3 {
		if res := e.post(t, ping); res.status != 200 || res.header.Get("X-Transponder-Fallbacks") != "1" {
			t.Fatalf("call %d: status %d, headers %v", i, res.status, res.header)
		}
	}
	res := e.post(t, ping)
	if res.status != 200 || res.header.Get("X-Transponder-Fallbacks") != "0" || res.header.Get("X-Transponder-Provider") != "b" || a.calls.Load() != 3 {
		t.Fatalf("status %d, headers %v, a called %d times", res.status, res.header, a.calls.Load())
	}
}

func TestCountTokensUsesFirstAnthropicTarget(t *testing.T) {
	oa := countedUpstream(t, status(200, okChat))
	an := countedUpstream(t, status(200, `{"input_tokens":42}`))
	e := setupRoutes(t, route("*", oaTarget("b", "", oa), anTarget("claude-sonnet-5-5", an)))
	res, err := e.doPath(http.MethodPost, "/v1/messages/count_tokens", map[string]string{"x-api-key": e.key}, `{"model":"m","messages":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusOK || string(res.body) != `{"input_tokens":42}` || oa.calls.Load() != 0 {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
}
