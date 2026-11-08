package openai

import (
	"bytes"
	"testing"
)

func FuzzDecodeChatRequest(f *testing.F) {
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":"x"}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := DecodeChatRequest(bytes.NewReader(body))
		if err == nil && req == nil {
			t.Fatal("nil request without an error")
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
