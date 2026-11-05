package server_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/router"
)

const streamPing = `{"model":"m","stream":true,"messages":[{"role":"user","content":"ping pong"}]}`

// payloads splits an SSE body into its data payloads.
func payloads(t *testing.T, body []byte) []string {
	t.Helper()
	var out []string
	for _, block := range strings.Split(strings.TrimSpace(string(body)), "\n\n") {
		if !strings.HasPrefix(block, "data: ") {
			t.Fatalf("bad frame %q in %q", block, body)
		}
		out = append(out, strings.TrimPrefix(block, "data: "))
	}
	return out
}

func checkTrailers(t *testing.T, res result, in, out, cost string) {
	t.Helper()
	if res.trailer.Get("X-Usage-Input-Tokens") != in || res.trailer.Get("X-Usage-Output-Tokens") != out || res.trailer.Get("X-Usage-Cost-USD") != cost {
		t.Fatalf("trailers %v, want %s %s %s", res.trailer, in, out, cost)
	}
}

func TestStreamEcho(t *testing.T) {
	e := setup(t, echo.New("echo"))
	res := e.post(t, streamPing)
	if res.status != http.StatusOK || res.header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, headers %v, body %s", res.status, res.header, res.body)
	}
	p := payloads(t, res.body)
	if p[len(p)-1] != "[DONE]" {
		t.Fatalf("last payload %q", p[len(p)-1])
	}
	var text strings.Builder
	for _, d := range p[:len(p)-1] {
		if strings.Contains(d, `"usage"`) {
			t.Fatalf("usage chunk sent without include_usage: %s", d)
		}
		var c struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(d), &c); err != nil || c.Object != "chat.completion.chunk" {
			t.Fatalf("chunk %s: %v", d, err)
		}
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
		}
	}
	if text.String() != "echo: ping pong" {
		t.Fatalf("streamed text %q", text.String())
	}
	checkTrailers(t, res, "3", "4", "0.000000")
	r := e.usage(t)[0]
	if r.Status != 200 || r.InputTokens != 3 || r.OutputTokens != 4 || r.Endpoint != "chat.completions" {
		t.Fatalf("record %+v", r)
	}
}

func TestStreamEchoIncludeUsage(t *testing.T) {
	e := setup(t, echo.New("echo"))
	res := e.post(t, `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"ping pong"}]}`)
	p := payloads(t, res.body)
	if !strings.Contains(p[len(p)-2], `"usage":{"prompt_tokens":3,"completion_tokens":4`) {
		t.Fatalf("payloads %q", p)
	}
}

const upstreamChunks = "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":500}}\n\n" +
	"data: [DONE]\n\n"

func streamingUpstream(t *testing.T, gotBody *map[string]any) router.Route {
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, upstreamChunks)
	})
	return openaiRoute("*", "gpt-4o-mini", up)
}

func TestStreamNative(t *testing.T) {
	var got map[string]any
	e := setupRoutes(t, []router.Route{streamingUpstream(t, &got)})
	res := e.post(t, streamPing)
	if res.status != http.StatusOK {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	opts, _ := got["stream_options"].(map[string]any)
	if opts["include_usage"] != true || got["model"] != "gpt-4o-mini" {
		t.Fatalf("upstream body %v", got)
	}
	p := payloads(t, res.body)
	if len(p) != 4 || p[3] != "[DONE]" || strings.Contains(strings.Join(p, ""), "usage") {
		t.Fatalf("payloads %q", p)
	}
	checkTrailers(t, res, "1000", "500", "0.000450")
	r := e.usage(t)[0]
	if r.CostNUSD != 450_000 || r.Estimated || r.Status != 200 || r.Model != "gpt-4o-mini" {
		t.Fatalf("record %+v", r)
	}
}

func TestStreamNativeKeepsUsageChunk(t *testing.T) {
	var got map[string]any
	e := setupRoutes(t, []router.Route{streamingUpstream(t, &got)})
	res := e.post(t, `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"x"}]}`)
	p := payloads(t, res.body)
	if len(p) != 5 || !strings.Contains(p[3], `"usage"`) {
		t.Fatalf("payloads %q", p)
	}
}

func TestStreamUpstreamFailsFirst(t *testing.T) {
	e := setupRoutes(t, []router.Route{openaiRoute("*", "", statusUpstream(t, 500, "oops"))})
	res := e.post(t, streamPing)
	if res.status != http.StatusBadGateway || res.header.Get("Content-Type") != "application/json" || errorCode(t, res) != "upstream_error" {
		t.Fatalf("status %d, headers %v, body %s", res.status, res.header, res.body)
	}
}

func TestStreamUpstreamEmpty(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
	})
	e := setupRoutes(t, []router.Route{openaiRoute("*", "", up)})
	res := e.post(t, streamPing)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	if r := e.usage(t)[0]; r.Status != http.StatusBadGateway {
		t.Fatalf("record %+v", r)
	}
}

func TestStreamUpstreamCut(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial answer\"}}]}\n\n")
		w.(http.Flusher).Flush()
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})
	e := setupRoutes(t, []router.Route{openaiRoute("*", "", up)})
	res := e.post(t, streamPing)
	p := payloads(t, res.body)
	if len(p) != 2 || !strings.Contains(p[0], "partial answer") || !strings.Contains(p[1], `"upstream_error"`) {
		t.Fatalf("payloads %q", p)
	}
	r := e.usage(t)[0]
	if r.Status != 200 || !r.Estimated || r.OutputTokens != 4 {
		t.Fatalf("record %+v", r)
	}
}

func TestStreamNativeMultilineData(t *testing.T) {
	for _, payload := range []string{
		`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		"{\n\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}",
		"{\n\n\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n",
	} {
		t.Run(payload, func(t *testing.T) {
			up := upstream(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+strings.ReplaceAll(payload, "\n", "\ndata: ")+"\n\n"+openai.DoneFrame)
			})
			e := setupRoutes(t, []router.Route{openaiRoute("*", "", up)})
			res := e.post(t, streamPing)
			if res.status != http.StatusOK {
				t.Fatalf("status %d: %s", res.status, res.body)
			}
			reader := openai.NewSSEReader(bytes.NewReader(res.body))
			got, err := reader.Next()
			if err != nil || string(got) != payload {
				t.Fatalf("payload %q, error %v; want %q", got, err, payload)
			}
			if !json.Valid(got) {
				t.Fatalf("invalid JSON: %s", got)
			}
			if _, err := reader.Next(); !errors.Is(err, io.EOF) {
				t.Fatalf("end of stream: %v", err)
			}
		})
	}
}
