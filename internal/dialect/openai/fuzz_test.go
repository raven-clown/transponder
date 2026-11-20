package openai

import (
	"bytes"
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
