package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/sse"
)

const oaPartial = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"

// TestBadUpstreamEvents checks events the gateway cannot read: one too large or not JSON. In
// the opening the next target answers; after output the stream ends with an error event, and
// the status, headers and usage trailers are those of a stream that began.
func TestBadUpstreamEvents(t *testing.T) {
	old := sse.MaxEventBytes
	sse.MaxEventBytes = 4 << 10
	t.Cleanup(func() { sse.MaxEventBytes = old })
	huge := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + strings.Repeat("x", 8<<10) + "\"}}]}\n\n"
	notJSON := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_d"
	cases := []struct {
		name     string
		first    string
		anFirst  bool
		fellBack bool
	}{
		{"too large, first", huge, false, true},
		{"too large, after output", oaPartial + huge, false, false},
		{"not JSON, first", "data: {\"choices\":[{\"index\n\n", true, true},
		{"not JSON, translated, after output", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n" + anTextDelta("partial") + "event: content_block_delta\ndata: {nope\n\n", true, false},
		{"cut inside an event, translated", notJSON, true, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			a := countedUpstream(t, replay(http.StatusOK, "text/event-stream", tt.first))
			b := countedUpstream(t, replay(http.StatusOK, "text/event-stream", upstreamChunks))
			first := oaTarget("a", "", a)
			if tt.anFirst {
				first = anTarget("", a)
			}
			e := setupRoutes(t, route("*", first, oaTarget("b", "", b)))
			res := e.post(t, streamPing)
			if res.status != http.StatusOK || res.header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("status %d, headers %v", res.status, res.header)
			}
			if res.trailer.Get("X-Usage-Input-Tokens") == "" || res.trailer.Get("X-Usage-Output-Tokens") == "" || res.trailer.Get("X-Usage-Cost-USD") == "" {
				t.Fatalf("headers %v, trailers %v", res.header, res.trailer)
			}
			p := payloads(t, res.body)
			if tt.fellBack {
				if b.calls.Load() != 1 || res.header.Get("X-Transponder-Provider") != "b" || p[len(p)-1] != "[DONE]" {
					t.Fatalf("b called %d times, headers %v, payloads %q", b.calls.Load(), res.header, p)
				}
				return
			}
			if b.calls.Load() != 0 || !strings.Contains(strings.Join(p, ""), "partial") || !strings.Contains(p[len(p)-1], "upstream_error") {
				t.Fatalf("b called %d times, payloads %q", b.calls.Load(), p)
			}
			if r := e.usage(t)[0]; r.Status != http.StatusOK || !r.Estimated || r.OutputTokens == 0 {
				t.Fatalf("record %+v", r)
			}
		})
	}
}

// TestUnicodeAndLargeMessages checks that text in any script, emoji, combining marks, the line
// separators JavaScript treats as newlines and characters JSON escapes reach the upstream and
// come back unchanged, on a body close to the 20 MiB limit, in every dialect pair.
func TestUnicodeAndLargeMessages(t *testing.T) {
	text := "สวัสดี 👋🏽 é    <&> \"q\" \\ \t 日本語 Ωmega"
	// JSON escapes some of these characters to six bytes, so the body is about 16 MiB.
	big := strings.Repeat(text, (12<<20)/len(text))
	cases := []struct {
		name    string
		path    string
		body    func(string) string
		route   func(string, *counted) router.Route
		content func(map[string]any) string // the user text the upstream got
	}{
		{"chat to openai", "/v1/chat/completions", chatBody, oaRoute, oaUserText},
		{"chat to anthropic", "/v1/chat/completions", chatBody, anRoute, anUserText},
		{"messages to anthropic", "/v1/messages", messagesBody, anRoute, anUserText},
		{"messages to openai", "/v1/messages", messagesBody, oaRoute, oaUserText},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var got string
			up := countedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				var m map[string]any
				if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
					t.Error(err)
				}
				mu.Lock()
				got = tt.content(m)
				mu.Unlock()
				reply, _ := json.Marshal(text)
				if strings.HasSuffix(r.URL.Path, "/messages") {
					_, _ = fmt.Fprintf(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":%s}],"usage":{"input_tokens":1,"output_tokens":1}}`, reply)
					return
				}
				_, _ = fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, reply)
			})
			e := setupRoutes(t, []router.Route{tt.route("*", up)})
			res, err := e.doPath(http.MethodPost, tt.path, map[string]string{"Authorization": "Bearer " + e.key}, tt.body(big))
			if err != nil || res.status != http.StatusOK {
				t.Fatalf("status %d, err %v: %.300s", res.status, err, res.body)
			}
			mu.Lock()
			defer mu.Unlock()
			if got != big {
				t.Fatalf("the upstream got %d bytes of text, want %d unchanged", len(got), len(big))
			}
			if !strings.Contains(decoded(t, res.body), text) {
				t.Fatalf("the answer changed: %.300s", res.body)
			}
		})
	}
}

func chatBody(text string) string {
	b, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": text}}})
	return string(b)
}

func messagesBody(text string) string {
	b, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": text}}})
	return string(b)
}

func oaUserText(m map[string]any) string {
	msgs, _ := m["messages"].([]any)
	if len(msgs) == 0 {
		return ""
	}
	msg, _ := msgs[0].(map[string]any)
	s, _ := msg["content"].(string)
	return s
}

func anUserText(m map[string]any) string {
	msgs, _ := m["messages"].([]any)
	if len(msgs) == 0 {
		return ""
	}
	msg, _ := msgs[0].(map[string]any)
	if s, ok := msg["content"].(string); ok {
		return s
	}
	blocks, _ := msg["content"].([]any)
	var b strings.Builder
	for _, bl := range blocks {
		block, _ := bl.(map[string]any)
		s, _ := block["text"].(string)
		b.WriteString(s)
	}
	return b.String()
}

// decoded returns every string in a JSON body, joined, so escapes do not hide the text.
func decoded(t *testing.T, body []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decoding %.300s: %v", body, err)
	}
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			b.WriteString(x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return b.String()
}

// TestConcurrentStreams runs many streams at once through every stream path, each with its own
// marker, and checks that no answer carries another call's text and each call is recorded
// once. Run with -race, it also checks the adapters and decoders share no state.
func TestConcurrentStreams(t *testing.T) {
	echoMarker := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		i := strings.Index(string(b), "marker-")
		marker := string(b[i : i+len("marker-000")])
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n"+
				anTextDelta(marker)+"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\""+marker+"\"}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}
	oa, an := countedUpstream(t, echoMarker), countedUpstream(t, echoMarker)
	e := setupRoutes(t, []router.Route{
		{Match: "oa", Targets: []router.Target{oaTarget("oa", "", oa)}},
		{Match: "an", Targets: []router.Target{anTarget("", an)}},
	})
	const calls = 64
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			marker := fmt.Sprintf("marker-%03d", i)
			model, path := []string{"oa", "an"}[i%2], []string{"/v1/chat/completions", "/v1/messages"}[(i/2)%2]
			body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"stream":true,"messages":[{"role":"user","content":%q}]}`, model, marker)
			res, err := e.doPath(http.MethodPost, path, map[string]string{"Authorization": "Bearer " + e.key}, body)
			switch {
			case err != nil:
				errs <- err
			case res.status != http.StatusOK || strings.Count(string(res.body), "marker-") != 1 || !strings.Contains(string(res.body), marker):
				errs <- fmt.Errorf("call %d (%s via %s): status %d: %s", i, model, path, res.status, res.body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := len(e.usage(t)); n != calls {
		t.Fatalf("%d records, want %d", n, calls)
	}
}
