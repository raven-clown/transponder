package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/router"
)

func TestNativeOpenAIGetsShortToolCallIDs(t *testing.T) {
	long := "gm_" + strings.Repeat("eyJpIjoiY2FsbF8x", 12)
	for name, stream := range map[string]bool{"plain": false, "stream": true} {
		t.Run(name, func(t *testing.T) {
			var got struct {
				Messages []struct {
					ToolCallID string `json:"tool_call_id"`
					ToolCalls  []struct {
						ID string `json:"id"`
					} `json:"tool_calls"`
				} `json:"messages"`
				Logprobs bool `json:"logprobs"`
			}
			up := countedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &got)
				if stream {
					_, _ = io.WriteString(w, upstreamChunks)
					return
				}
				_, _ = io.WriteString(w, okChat)
			})
			e := setupRoutes(t, route("*", oaTarget("b", "", up)))
			body := `{"model":"m","logprobs":true,"messages":[{"role":"user","content":"x"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"` + long + `","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"call_short","type":"function","function":{"name":"g","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"` + long + `","content":"1"},{"role":"tool","tool_call_id":"call_short","content":"2"}]`
			if stream {
				body += `,"stream":true`
			}
			if res := e.post(t, body+"}"); res.status != http.StatusOK {
				t.Fatalf("status %d: %s", res.status, res.body)
			}
			id := got.Messages[1].ToolCalls[0].ID
			if len(id) > 40 || id != got.Messages[2].ToolCallID || got.Messages[1].ToolCalls[1].ID != "call_short" || got.Messages[3].ToolCallID != "call_short" || !got.Logprobs {
				t.Fatalf("upstream messages %+v", got)
			}
		})
	}
}

func TestOutageNamesTheTargetThatFailed(t *testing.T) {
	image := `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}]}`
	down := countedUpstream(t, status(500, "x"))
	oa := countedUpstream(t, status(200, okChat))
	e := setupRoutes(t, route("*", anTarget("", down), oaTarget("b", "", oa)))
	res := e.messages(t, nil, image)
	if res.status != http.StatusBadGateway || res.header.Get("X-Transponder-Provider") != "claude" || res.header.Get("X-Transponder-Fallbacks") != "0" {
		t.Fatalf("status %d, headers %v", res.status, res.header)
	}
	if r := e.usage(t)[0]; r.Provider != "claude" || r.Fallbacks != 0 {
		t.Fatalf("record %+v", r)
	}
}

func TestStreamThatStartsWithAnErrorFallsBack(t *testing.T) {
	overloaded := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n")
	})
	b := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, upstreamChunks) })
	e := setupRoutes(t, route("*", anTarget("", overloaded), oaTarget("b", "", b)))
	res := e.messages(t, nil, `{"model":"m","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if res.status != http.StatusOK || strings.Contains(string(res.body), "overloaded_error") || !strings.Contains(string(res.body), "event: message_stop") || res.header.Get("X-Transponder-Provider") != "b" {
		t.Fatalf("status %d, headers %v: %s", res.status, res.header, res.body)
	}

	oaErr := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "data: {\"error\":{\"message\":\"overloaded\",\"type\":\"server_error\"}}\n\n")
	})
	e = setupRoutes(t, route("*", oaTarget("a", "", oaErr), oaTarget("b", "", b)))
	res = e.post(t, streamPing)
	if res.status != http.StatusOK || strings.Contains(string(res.body), "overloaded") || res.header.Get("X-Transponder-Provider") != "b" {
		t.Fatalf("openai: status %d, headers %v: %s", res.status, res.header, res.body)
	}
}

func TestCutStreamsCoolTheTargetDown(t *testing.T) {
	cuts := map[string]struct {
		target func(*counted) router.Target
		first  string
		body   string
		viaMsg bool
	}{
		"openai": {func(c *counted) router.Target { return oaTarget("a", "", c) }, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n", streamPing, false},
		"anthropic": {func(c *counted) router.Target { return anTarget("", c) }, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n",
			`{"model":"m","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, true},
	}
	for name, tt := range cuts {
		t.Run(name, func(t *testing.T) {
			a := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tt.first) })
			b := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, upstreamChunks) })
			e := setupRoutes(t, route("*", tt.target(a), oaTarget("b", "", b)))
			send := func() result {
				if tt.viaMsg {
					return e.messages(t, nil, tt.body)
				}
				return e.post(t, tt.body)
			}
			for range 3 {
				send()
			}
			res := send()
			if a.calls.Load() != 3 || b.calls.Load() != 1 || res.header.Get("X-Transponder-Provider") != "b" {
				t.Fatalf("a called %d times, b %d, headers %v", a.calls.Load(), b.calls.Load(), res.header)
			}
		})
	}
}
