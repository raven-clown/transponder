package anthropic

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/raven-clown/transponder/internal/schema"
)

func FuzzDecodeRequest(f *testing.F) {
	f.Add([]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	f.Add([]byte(`{"model":"m","system":[{"type":"text","text":"s"}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":"ok"}]}]}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}},{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}]}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png"}}]}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		if p, err := PeekRequest(body); err == nil && p.Model == "" {
			t.Fatal("a peek without a model passed")
		}
		req, err := DecodeRequest(body)
		if err == nil && req == nil {
			t.Fatal("nil request without an error")
		}
		if err != nil {
			return
		}
		// A decoded request goes back out to Anthropic targets, or is refused for content
		// they cannot carry.
		if out, err := EncodeRequest(req); err != nil {
			var ce *schema.ContentError
			if !errors.As(err, &ce) {
				t.Fatalf("encoding a decoded request: %v", err)
			}
		} else if !json.Valid(out) {
			t.Fatalf("encoded %s", out)
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
	})
}

func FuzzDecodeResponse(f *testing.F) {
	f.Add([]byte(`{"id":"msg","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		resp, err := DecodeResponse(body)
		if err == nil && resp == nil {
			t.Fatal("nil response without an error")
		}
		ParseUsage(body)
	})
}
