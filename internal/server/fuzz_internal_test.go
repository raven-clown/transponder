package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzBodyEdits runs the estimate and the edits the gateway makes to a caller's body before
// it goes upstream. Whatever the body, a cap must hold however the field is spelled, and the
// Responses API body must leave with store off.
func FuzzBodyEdits(f *testing.F) {
	f.Add([]byte(`{"model":"m","max_tokens":100,"Max_Tokens":900,"max_completion_tokens":"x","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]}]}`), int64(10))
	f.Add([]byte(`{"model":"m","max_tokens":5000,"thinking":{"type":"enabled","budget_tokens":4000},"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`), int64(2048))
	f.Add([]byte(`{"model":"m","input":[{"type":"input_file","file_data":"data:application/pdf;base64,QUJD"}],"STORE":true,"Background":true,"max_output_tokens":1e9}`), int64(64))
	f.Add([]byte(`[1,2,3]`), int64(1))
	f.Fuzz(func(t *testing.T, body []byte, limit int64) {
		if n := estimateInput(body); n < 0 {
			t.Fatalf("estimate %d", n)
		}
		promptTokens(body)
		requestedMax(body, "n")
		if limit <= 0 {
			return
		}
		var m map[string]json.RawMessage
		object := json.Unmarshal(body, &m) == nil && m != nil
		for _, c := range []struct {
			name   string
			out    []byte
			fields []string
		}{
			{"openai", capOpenAI(body, "openai", limit), []string{"max_tokens", "max_completion_tokens"}},
			{"other", capOpenAI(body, "vllm", limit), []string{"max_tokens", "max_completion_tokens"}},
			{"anthropic", capAnthropic(body, limit, limit), []string{"max_tokens"}},
			{"responses", capResponses(body, limit), []string{"max_output_tokens"}},
		} {
			if !object {
				if string(c.out) != string(body) {
					t.Fatalf("%s: a body that is not an object was changed to %s", c.name, c.out)
				}
				continue
			}
			if got := requestedMax(c.out, c.fields...); got > limit {
				t.Fatalf("%s: %s asks for %d tokens, over the cap of %d", c.name, c.out, got, limit)
			}
		}
		if !object {
			return
		}
		var s map[string]json.RawMessage
		if err := json.Unmarshal(stateless(body), &s); err != nil {
			t.Fatal(err)
		}
		for k, v := range s {
			if strings.EqualFold(k, "store") && (k != "store" || string(v) != "false") {
				t.Fatalf("stateless left %s: %s", k, v)
			}
		}
	})
}
