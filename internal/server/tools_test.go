package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/raven-clown/transponder/internal/router"
)

// TestToolCallsAcrossDialects follows tool calls through the gateway between dialects: ids
// another provider made reach Anthropic in a form it takes, an Anthropic tool with no input
// reaches an OpenAI caller with arguments that parse, and parallel calls from a server that
// does not number them reach an Anthropic caller as separate blocks.
func TestToolCallsAcrossDialects(t *testing.T) {
	t.Run("ids from another provider to anthropic", func(t *testing.T) {
		var mu sync.Mutex
		var got []byte
		up := countedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			got = b
			mu.Unlock()
			_, _ = io.WriteString(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"sunny"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		})
		e := setupRoutes(t, []router.Route{anthropicRoute("*", "", up.srv)})
		res := e.post(t, `{"model":"m","messages":[{"role":"user","content":"weather?"},`+
			`{"role":"assistant","tool_calls":[{"id":"functions.get_weather:0","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},`+
			`{"role":"tool","tool_call_id":"functions.get_weather:0","content":"sunny"}]}`)
		if res.status != http.StatusOK {
			t.Fatalf("status %d: %s", res.status, res.body)
		}
		mu.Lock()
		defer mu.Unlock()
		ids := regexp.MustCompile(`"(?:id|tool_use_id)":"([^"]*)"`).FindAllStringSubmatch(string(got), -1)
		if len(ids) != 2 || ids[0][1] != ids[1][1] || !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(ids[0][1]) {
			t.Fatalf("upstream body %s", got)
		}
	})

	t.Run("anthropic tool without input to openai caller", func(t *testing.T) {
		up := countedUpstream(t, replay(http.StatusOK, "text/event-stream",
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n"+
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"now\",\"input\":{}}}\n\n"+
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		e := setupRoutes(t, []router.Route{anthropicRoute("*", "", up.srv)})
		res := e.post(t, streamPing)
		var args strings.Builder
		for _, p := range payloads(t, res.body) {
			var c struct {
				Choices []struct {
					Delta struct {
						ToolCalls []struct {
							Function struct {
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
				} `json:"choices"`
			}
			_ = json.Unmarshal([]byte(p), &c)
			for _, ch := range c.Choices {
				for _, tc := range ch.Delta.ToolCalls {
					args.WriteString(tc.Function.Arguments)
				}
			}
		}
		if !json.Valid([]byte(args.String())) {
			t.Fatalf("arguments %q do not parse:\n%s", args.String(), res.body)
		}
	})

	t.Run("unnumbered parallel calls to anthropic caller", func(t *testing.T) {
		up := countedUpstream(t, replay(http.StatusOK, "text/event-stream",
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":["+
				"{\"id\":\"a\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"x\\\":1}\"}},"+
				"{\"id\":\"b\",\"type\":\"function\",\"function\":{\"name\":\"g\",\"arguments\":\"{\\\"y\\\":2}\"}}]}}]}\n\n"+
				"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"))
		e := setupRoutes(t, []router.Route{openaiRoute("*", "", up.srv)})
		res := e.messages(t, nil, `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"ping"}]}`)
		body := string(res.body)
		if strings.Count(body, `"type":"tool_use"`) != 2 || !strings.Contains(body, `{\"x\":1}`) || !strings.Contains(body, `{\"y\":2}`) || strings.Contains(body, `}{`) {
			t.Fatalf("stream:\n%s", body)
		}
	})
}
