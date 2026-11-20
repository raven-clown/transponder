package openai

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/schema"
)

func FuzzDecodeChatRequest(f *testing.F) {
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":"x"}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk=","detail":"low"}},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64"}}]}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := DecodeChatRequest(bytes.NewReader(body))
		if err == nil && req == nil {
			t.Fatal("nil request without an error")
		}
		if err != nil {
			return
		}
		for _, m := range req.Messages {
			for _, p := range m.Content {
				if p.Type != schema.PartImage {
					continue
				}
				// An image that decodes is one or the other, never neither or both.
				if p.Image == nil || (p.Image.URL == "") == (p.Image.Data == "") {
					t.Fatalf("image part %+v", p.Image)
				}
			}
		}
		// What decodes must encode again, since that is how it reaches another provider.
		if _, err := EncodeChatRequest(req); err != nil {
			t.Fatalf("encoding a decoded request: %v", err)
		}
	})
}

// FuzzPeekAndRewrite checks what the gateway reads from a body it passes through. After the
// model is rewritten, the upstream must read the model the gateway set and the stream flag the
// gateway routed on, whatever duplicate or odd keys the caller sent.
func FuzzPeekAndRewrite(f *testing.F) {
	f.Add([]byte(`{"model":"m","stream":true,"stream_options":{"include_usage":false},"messages":[]}`))
	f.Add([]byte(`{"model":"m","Model":"x","STREAM":true,"stream":false}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"` + strings.Repeat("x", 60) + `"}]},{"role":"tool","tool_call_id":"` + strings.Repeat("x", 60) + `"}]}`))
	f.Add([]byte(`{"model":"m","input":["a",[1,2]],"background":false,"store":true}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, _ = ParseEmbedRequest(body)
		_, _ = PeekResponses(body)
		ParseUsage(body)
		_, _ = DecodeChatResponse(body)
		_, _ = DecodeEmbedResponse(body)
		short := ShortenToolCallIDs(body)
		if !bytes.Equal(short, body) && !json.Valid(short) {
			t.Fatalf("tool call ids rewritten into invalid JSON: %s", short)
		}
		peek, err := PeekRequest(body)
		if err != nil {
			return
		}
		for _, force := range []bool{false, true} {
			out, err := RewriteRequest(body, "upstream-model", force)
			if err != nil {
				t.Fatalf("a body that peeks must rewrite: %v", err)
			}
			again, err := PeekRequest(out)
			if err != nil || again.Model != "upstream-model" || again.Stream != peek.Stream || (force && !again.IncludeUsage) {
				t.Fatalf("rewritten %s reads as %+v, %v; the caller sent %+v", out, again, err, peek)
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			for k := range m {
				if k != "model" && strings.EqualFold(k, "model") || k != "stream" && strings.EqualFold(k, "stream") {
					t.Fatalf("key %q would let a case-blind upstream read another value", k)
				}
			}
		}
	})
}

func FuzzSSEReader(f *testing.F) {
	f.Add([]byte("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"))
	f.Add([]byte("data: {}\n\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		r := NewSSEReader(bytes.NewReader(in))
		for i := 0; i <= len(in)+1; i++ {
			data, err := r.Next()
			if err != nil {
				return
			}
			ChunkUsage(data)
		}
		t.Fatal("the reader did not end")
	})
}
