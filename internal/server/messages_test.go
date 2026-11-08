package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/provider/anthropic"
	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/router"
)

func anthropicRoute(match, model string, up *httptest.Server) router.Route {
	a := anthropic.New("claude", up.URL, "sk-ant-gateway", up.Client())
	return router.Route{Match: match, Targets: []router.Target{{Provider: a, Type: "anthropic", Model: model}}}
}

func (e *env) messages(t *testing.T, headers map[string]string, body string) result {
	t.Helper()
	if headers == nil {
		headers = map[string]string{"x-api-key": e.key}
	}
	res, err := e.doPath(http.MethodPost, "/v1/messages", headers, body)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func anthropicErrorType(t *testing.T, res result) string {
	t.Helper()
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.body, &body); err != nil || body.Type != "error" {
		t.Fatalf("not an Anthropic error: %s", res.body)
	}
	return body.Error.Type
}

const claudeCodeBody = `{"model":"sonnet","max_tokens":1024,"system":[{"type":"text","text":"terse","cache_control":{"type":"ephemeral"}}],"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}],"context_management":{"edits":[]}}`

func TestMessagesNative(t *testing.T) {
	var got map[string]json.RawMessage
	var gotHeader http.Header
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header
		if r.URL.Path != "/messages" {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[{"type":"text","text":"hey"}],"stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":100,"cache_read_input_tokens":1000,"cache_creation_input_tokens":1000},"container":null}`)
	})
	e := setupRoutes(t, []router.Route{anthropicRoute("sonnet", "claude-sonnet-5-5", up)})
	res := e.messages(t, map[string]string{"x-api-key": e.key, "anthropic-beta": "context-management-2025-06-27", "anthropic-version": "2023-06-01"}, claudeCodeBody)
	if res.status != http.StatusOK {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	if string(got["model"]) != `"claude-sonnet-5-5"` || got["thinking"] == nil || got["context_management"] == nil || !strings.Contains(string(got["system"]), "cache_control") {
		t.Fatalf("upstream body %v", got)
	}
	if gotHeader.Get("X-Api-Key") != "sk-ant-gateway" || gotHeader.Get("Anthropic-Beta") != "context-management-2025-06-27" {
		t.Fatalf("upstream headers %v", gotHeader)
	}
	if !strings.Contains(string(res.body), `"container":null`) || res.header.Get("X-Usage-Cost-USD") != "0.005600" {
		t.Fatalf("headers %v body %s", res.header, res.body)
	}
	r := e.usage(t)[0]
	if r.Endpoint != "messages" || r.Route != "sonnet" || r.CostNUSD != 5_600_000 || r.CacheReadTokens != 1000 || r.Estimated {
		t.Fatalf("record %+v", r)
	}
}

const anthropicStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":1000,\"output_tokens\":1}}}\n\n" +
	"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hey\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":100}}\n\n"

func TestMessagesNativeStream(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicStream+"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	e := setupRoutes(t, []router.Route{anthropicRoute("*", "claude-sonnet-5-5", up)})
	res := e.messages(t, nil, `{"model":"m","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	want := anthropicStream + "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	if res.status != http.StatusOK || string(res.body) != want {
		t.Fatalf("status %d body\n%s", res.status, res.body)
	}
	checkTrailers(t, res, "1000", "100", "0.003000")
	if r := e.usage(t)[0]; r.Estimated || r.OutputTokens != 100 || r.Status != 200 {
		t.Fatalf("record %+v", r)
	}
}

func TestMessagesNativeStreamCut(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicStream)
	})
	e := setupRoutes(t, []router.Route{anthropicRoute("*", "", up)})
	res := e.messages(t, nil, `{"model":"m","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.HasSuffix(string(res.body), "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"the provider stopped in the middle of the stream\"}}\n\n") {
		t.Fatalf("body\n%s", res.body)
	}
	if r := e.usage(t)[0]; !r.Estimated || r.InputTokens != 1000 {
		t.Fatalf("record %+v", r)
	}
}

func TestMessagesTranslated(t *testing.T) {
	e := setup(t, echo.New("echo"))
	res := e.messages(t, map[string]string{"Authorization": "Bearer " + e.key}, `{"model":"m","max_tokens":10,"system":"be nice","messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}]}`)
	var body struct {
		Type       string `json:"type"`
		Role       string `json:"role"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens int64 `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(res.body, &body); err != nil || res.status != http.StatusOK {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	if body.Type != "message" || body.StopReason != "end_turn" || len(body.Content) != 1 || body.Content[0].Text != "echo: ping" {
		t.Fatalf("body %s", res.body)
	}
	if r := e.usage(t)[0]; r.Endpoint != "messages" || r.Provider != "echo" {
		t.Fatalf("record %+v", r)
	}
}

func TestMessagesTranslatedStream(t *testing.T) {
	e := setup(t, echo.New("echo"))
	res := e.messages(t, nil, `{"model":"m","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"ping pong"}]}`)
	b := string(res.body)
	for _, want := range []string{"event: message_start\n", `"text_delta","text":"echo: "`, "event: message_delta\n", `"stop_reason":"end_turn"`, "event: message_stop\n"} {
		if !strings.Contains(b, want) {
			t.Fatalf("missing %q in\n%s", want, b)
		}
	}
	checkTrailers(t, res, "3", "4", "0.000000")
}

func TestMessagesToOpenAIWithTools(t *testing.T) {
	var got map[string]json.RawMessage
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, `{"id":"c1","model":"gpt-x","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_7","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	})
	e := setupRoutes(t, []router.Route{openaiRoute("*", "gpt-x", up)})
	res := e.messages(t, nil, `{"model":"m","max_tokens":10,"tools":[{"name":"weather","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"weather?"}]}`)
	if res.status != http.StatusOK {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	if !strings.Contains(string(got["tools"]), `"name":"weather"`) || string(got["model"]) != `"gpt-x"` {
		t.Fatalf("upstream body %v", got)
	}
	if !strings.Contains(string(res.body), `{"type":"tool_use","id":"call_7","name":"weather","input":{"city":"Paris"}}`) || !strings.Contains(string(res.body), `"stop_reason":"tool_use"`) {
		t.Fatalf("body %s", res.body)
	}
}

func TestMessagesErrors(t *testing.T) {
	closed := upstream(t, func(http.ResponseWriter, *http.Request) {})
	closed.Close()
	echoOnly := []router.Route{{Match: "m", Targets: []router.Target{{Provider: echo.New("echo"), Type: "echo"}}}}
	tests := []struct {
		name         string
		routes       []router.Route
		noAuth       bool
		body         string
		status       int
		errType      string
		upstreamBody string
	}{
		{name: "no key", routes: echoOnly, noAuth: true, body: `{"model":"m"}`, status: 401, errType: "authentication_error"},
		{name: "bad json", routes: echoOnly, body: `{`, status: 400, errType: "invalid_request_error"},
		{name: "no route", routes: echoOnly, body: `{"model":"other","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`, status: 404, errType: "not_found_error"},
		{name: "image to echo", routes: echoOnly, body: `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`, status: 400, errType: "invalid_request_error"},
		{name: "upstream 400 passes through", routes: []router.Route{anthropicRoute("*", "", statusUpstream(t, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"messages: roles must alternate"}}`))}, body: `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`, status: 400, upstreamBody: `{"type":"error","error":{"type":"invalid_request_error","message":"messages: roles must alternate"}}`},
		{name: "upstream 529", routes: []router.Route{anthropicRoute("*", "", statusUpstream(t, 529, `{}`))}, body: `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`, status: 502, errType: "api_error"},
		{name: "upstream down, streaming", routes: []router.Route{anthropicRoute("*", "", closed)}, body: `{"model":"m","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"x"}]}`, status: 502, errType: "api_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := setupRoutes(t, tt.routes)
			h := map[string]string{"x-api-key": e.key}
			if tt.noAuth {
				h = map[string]string{"x-api-key": "tp_wrong"}
			}
			res := e.messages(t, h, tt.body)
			if res.status != tt.status {
				t.Fatalf("status %d: %s", res.status, res.body)
			}
			if tt.upstreamBody != "" {
				if string(res.body) != tt.upstreamBody {
					t.Fatalf("body %s", res.body)
				}
				return
			}
			if got := anthropicErrorType(t, res); got != tt.errType {
				t.Fatalf("error type %s: %s", got, res.body)
			}
		})
	}
}

func TestCountTokens(t *testing.T) {
	var path, model string
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		var b struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		model = b.Model
		_, _ = io.WriteString(w, `{"input_tokens":42}`)
	})
	e := setupRoutes(t, []router.Route{
		anthropicRoute("sonnet", "claude-sonnet-5-5", up),
		{Match: "*", Targets: []router.Target{{Provider: echo.New("echo"), Type: "echo"}}},
	})
	res, err := e.doPath(http.MethodPost, "/v1/messages/count_tokens", map[string]string{"x-api-key": e.key}, `{"model":"sonnet","messages":[{"role":"user","content":"hi"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusOK || string(res.body) != `{"input_tokens":42}` || path != "/messages/count_tokens" || model != "claude-sonnet-5-5" {
		t.Fatalf("status %d body %s, upstream %s %s", res.status, res.body, path, model)
	}
	res, err = e.doPath(http.MethodPost, "/v1/messages/count_tokens", map[string]string{"x-api-key": e.key}, `{"model":"other","messages":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusNotFound || anthropicErrorType(t, res) != "not_found_error" {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	if n := len(e.usage(t)); n != 0 {
		t.Fatalf("count_tokens wrote %d usage records", n)
	}
}

func TestCountTokensUpstreamErrors(t *testing.T) {
	for status, want := range map[int]int{400: 400, 500: 502} {
		e := setupRoutes(t, []router.Route{anthropicRoute("*", "", statusUpstream(t, status, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`))})
		res, err := e.doPath(http.MethodPost, "/v1/messages/count_tokens", map[string]string{"x-api-key": e.key}, `{"model":"m","messages":[]}`)
		if err != nil {
			t.Fatal(err)
		}
		if res.status != want || anthropicErrorType(t, res) == "" {
			t.Fatalf("upstream %d: status %d %s", status, res.status, res.body)
		}
		if n := len(e.usage(t)); n != 0 {
			t.Fatalf("records %d", n)
		}
	}
}
