package anthropic

import "testing"

func FuzzDecodeRequest(f *testing.F) {
	f.Add([]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	f.Add([]byte(`{"model":"m","system":[{"type":"text","text":"s"}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":"ok"}]}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := DecodeRequest(body)
		if err == nil && req == nil {
			t.Fatal("nil request without an error")
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
