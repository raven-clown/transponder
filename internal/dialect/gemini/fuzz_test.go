package gemini

import "testing"

func FuzzDecodeResponse(f *testing.F) {
	f.Add([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2}}`))
	f.Add([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]}}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		resp, err := DecodeResponse(body)
		if err == nil && resp == nil {
			t.Fatal("nil response without an error")
		}
		input := []string{"a", "bb"}
		if e, err := DecodeEmbedResponse(body, "m", input); err == nil && (e == nil || e.Usage.InputTokens <= 0) {
			t.Fatalf("embeddings %+v without usage", e)
		}
	})
}
