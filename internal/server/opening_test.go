package server_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestStreamFailsInItsOpening checks that a stream which fails after events that carry no
// output yet (a role chunk, message_start, a ping, response.created) still falls back: the
// caller has had nothing, so the next target can answer in full.
func TestStreamFailsInItsOpening(t *testing.T) {
	const (
		anOpening = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: ping\ndata: {\"type\":\"ping\"}\n\n"
		anOverloaded = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
		oaOpening    = "data: {\"choices\":[],\"prompt_filter_results\":[]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"refusal\":null}}]}\n\n"
		oaError    = "data: {\"error\":{\"message\":\"overloaded\",\"type\":\"server_error\"}}\n\n"
		resOpening = "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"r\"}}\n\n" +
			"event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"sequence_number\":1,\"response\":{\"id\":\"r\"}}\n\n"
		resError = "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"x\",\"sequence_number\":2}\n\n"
	)
	cases := []struct {
		name    string
		path    string
		body    string
		first   string
		anFirst bool // the first target speaks Anthropic
		want    string
	}{
		{"messages, error after message_start", "/v1/messages", messagesStreamPing, anOpening + anOverloaded, true, "event: message_stop"},
		{"messages, ends after message_start", "/v1/messages", messagesStreamPing, anOpening, true, "event: message_stop"},
		{"chat from anthropic, error after message_start", "/v1/chat/completions", streamPing, anOpening + anOverloaded, true, "[DONE]"},
		{"chat, error after the role chunk", "/v1/chat/completions", streamPing, oaOpening + oaError, false, "[DONE]"},
		{"chat, ends after the role chunk", "/v1/chat/completions", streamPing, oaOpening, false, "[DONE]"},
		{"messages from openai, error after the role chunk", "/v1/messages", messagesStreamPing, oaOpening + oaError, false, "event: message_stop"},
		{"responses, error after response.created", "/v1/responses", responsesStream, resOpening + resError, false, "response.completed"},
		{"responses, ends after response.created", "/v1/responses", responsesStream, resOpening, false, "response.completed"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			a := countedUpstream(t, replay(http.StatusOK, "text/event-stream", tt.first))
			answer := upstreamChunks
			if tt.path == "/v1/responses" {
				answer = fixture(t, "stream.sse")
			}
			b := countedUpstream(t, replay(http.StatusOK, "text/event-stream", answer))
			first := oaTarget("a", "", a)
			if tt.anFirst {
				first = anTarget("", a)
			}
			e := setupRoutes(t, route("*", first, oaTarget("b", "", b)))
			res, err := e.doPath(http.MethodPost, tt.path, map[string]string{"Authorization": "Bearer " + e.key}, tt.body)
			if err != nil {
				t.Fatal(err)
			}
			body := string(res.body)
			if res.status != http.StatusOK || b.calls.Load() != 1 || res.header.Get("X-Transponder-Provider") != "b" {
				t.Fatalf("status %d, b called %d times, headers %v:\n%s", res.status, b.calls.Load(), res.header, body)
			}
			if strings.Contains(body, "verloaded") || strings.Contains(body, "server_error") || strings.Contains(body, `"input_tokens":9`) || !strings.Contains(body, tt.want) {
				t.Fatalf("the caller saw the first target's stream:\n%s", body)
			}
			if r := e.usage(t)[0]; r.Provider != "b" || r.Fallbacks != 1 {
				t.Fatalf("record %+v", r)
			}
		})
	}
}

// TestStreamFailsAfterOutput checks the other side: once output reached the caller, a failure
// is passed on and nothing falls back.
func TestStreamFailsAfterOutput(t *testing.T) {
	a := countedUpstream(t, replay(http.StatusOK, "text/event-stream",
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n"+anTextDelta("par")+
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"))
	b := countedUpstream(t, replay(http.StatusOK, "text/event-stream", upstreamChunks))
	e := setupRoutes(t, route("*", anTarget("", a), oaTarget("b", "", b)))
	res := e.messages(t, nil, messagesStreamPing)
	body := string(res.body)
	if b.calls.Load() != 0 || !strings.Contains(body, `"text":"par"`) || strings.Count(body, "overloaded_error") != 1 {
		t.Fatalf("b called %d times:\n%s", b.calls.Load(), body)
	}
	if r := e.usage(t)[0]; r.Provider != "claude" || r.Status != http.StatusOK || !r.Estimated {
		t.Fatalf("record %+v", r)
	}
}
