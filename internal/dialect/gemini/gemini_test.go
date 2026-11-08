package gemini

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/schema"
)

func ptr[T any](v T) *T { return &v }

func compact(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func TestEncodeRequest(t *testing.T) {
	id := ToolCallID("gem-1", "sig==")
	req := &schema.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []schema.Message{
			{Role: schema.RoleSystem, Content: "be brief"},
			{Role: schema.RoleUser, Content: "weather?"},
			{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: id, Name: "weather", Arguments: `{"city":"Paris"}`}, {ID: "call_x", Name: "time", Arguments: ""}}},
			{Role: schema.RoleTool, ToolCallID: id, Content: `{"sky":"clear"}`},
			{Role: schema.RoleTool, ToolCallID: "call_x", Content: "noon"},
		},
		Tools:       []schema.Tool{{Name: "weather", Description: "w", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice:  &schema.ToolChoice{Mode: schema.ToolChoiceTool, Name: "weather"},
		MaxTokens:   ptr(50),
		Temperature: ptr(0.2),
		TopP:        ptr(0.9),
		Stop:        []string{"END"},
	}
	b, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
		"systemInstruction": {"parts": [{"text": "be brief"}]},
		"contents": [
			{"role": "user", "parts": [{"text": "weather?"}]},
			{"role": "model", "parts": [
				{"functionCall": {"id": "gem-1", "name": "weather", "args": {"city": "Paris"}}, "thoughtSignature": "sig=="},
				{"functionCall": {"id": "call_x", "name": "time", "args": {}}, "thoughtSignature": "skip_thought_signature_validator"}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"id": "gem-1", "name": "weather", "response": {"sky": "clear"}}},
				{"functionResponse": {"id": "call_x", "name": "time", "response": {"content": "noon"}}}
			]}
		],
		"tools": [{"functionDeclarations": [{"name": "weather", "description": "w", "parametersJsonSchema": {"type": "object"}}]}],
		"toolConfig": {"functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": ["weather"]}},
		"generationConfig": {"maxOutputTokens": 50, "temperature": 0.2, "topP": 0.9, "stopSequences": ["END"]}
	}`
	if compact(t, string(b)) != compact(t, want) {
		t.Fatalf("got\n%s", b)
	}
	for mode, wire := range map[string]string{schema.ToolChoiceAuto: "AUTO", schema.ToolChoiceNone: "NONE", schema.ToolChoiceRequired: "ANY"} {
		b, _ := EncodeRequest(&schema.ChatRequest{Model: "m", Messages: []schema.Message{{Role: "user", Content: "x"}}, ToolChoice: &schema.ToolChoice{Mode: mode}})
		if !strings.Contains(string(b), `"mode":"`+wire+`"`) {
			t.Fatalf("%s: %s", mode, b)
		}
	}
}

func TestToolCallID(t *testing.T) {
	id := ToolCallID("gem-1", "a/b+c==")
	if !strings.HasPrefix(id, "gm_") || strings.ContainsAny(id, "/+=.") {
		t.Fatalf("id %q", id)
	}
	gid, sig := splitID(id)
	if gid != "gem-1" || sig != "a/b+c==" {
		t.Fatalf("split %q %q", gid, sig)
	}
	if gid, sig := splitID("toolu_9"); gid != "toolu_9" || sig != "" {
		t.Fatalf("foreign id split %q %q", gid, sig)
	}
	if id := ToolCallID("", ""); id == "" || !strings.HasPrefix(id, "gm_") {
		t.Fatalf("id without gemini id %q", id)
	}
}

func TestDecodeResponse(t *testing.T) {
	body := `{"responseId":"r1","modelVersion":"gemini-2.5-flash","candidates":[{"content":{"role":"model","parts":[
		{"text":"thinking...","thought":true},
		{"text":"Let me check."},
		{"functionCall":{"id":"gem-1","name":"weather","args":{"city":"Paris"}},"thoughtSignature":"sig=="}
	]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":40,"candidatesTokenCount":10,"thoughtsTokenCount":5}}`
	got, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := &schema.ChatResponse{ID: "r1", Model: "gemini-2.5-flash", FinishReason: schema.FinishToolCalls,
		Message: schema.Message{Role: schema.RoleAssistant, Content: "Let me check.", ToolCalls: []schema.ToolCall{{ID: ToolCallID("gem-1", "sig=="), Name: "weather", Arguments: `{"city":"Paris"}`}}},
		Usage:   schema.Usage{InputTokens: 60, CacheReadTokens: 40, OutputTokens: 15}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	for reason, want := range map[string]string{"MAX_TOKENS": "length", "SAFETY": "content_filter", "PROHIBITED_CONTENT": "content_filter", "OTHER": "stop"} {
		r, err := DecodeResponse([]byte(`{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"` + reason + `"}]}`))
		if err != nil || r.FinishReason != want {
			t.Fatalf("%s: %+v %v", reason, r, err)
		}
	}
	blocked, err := DecodeResponse([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`))
	if err != nil || blocked.FinishReason != schema.FinishContentFilter {
		t.Fatalf("blocked prompt %+v %v", blocked, err)
	}
	if _, err := DecodeResponse([]byte(`nope`)); err == nil {
		t.Fatal("bad body accepted")
	}
}

func TestStreamDecoder(t *testing.T) {
	chunks := []string{
		`{"candidates":[{"content":{"parts":[{"text":"Hel"}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"lo"},{"functionCall":{"name":"weather","args":{"city":"Paris"}}}]}}]}`,
		`{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}`,
	}
	var d StreamDecoder
	var got []schema.Event
	for _, c := range chunks {
		evs, err := d.Events([]byte(c))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, evs...)
	}
	if len(got) != 7 {
		t.Fatalf("events %+v", got)
	}
	types := []schema.EventType{schema.EventStart, schema.EventText, schema.EventText, schema.EventToolStart, schema.EventToolDelta, schema.EventFinish, schema.EventUsage}
	for i, ty := range types {
		if got[i].Type != ty {
			t.Fatalf("event %d is %s, want %s: %+v", i, got[i].Type, ty, got)
		}
	}
	if got[4].ToolArgs != `{"city":"Paris"}` || got[5].FinishReason != schema.FinishToolCalls || got[6].Usage.InputTokens != 8 || !d.Done() {
		t.Fatalf("events %+v", got)
	}
	if _, err := d.Events([]byte(`{`)); err == nil {
		t.Fatal("bad chunk accepted")
	}
}

func TestEmbed(t *testing.T) {
	b, err := EncodeEmbedRequest("gemini-embedding-001", []string{"a", "bb"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"requests":[{"model":"models/gemini-embedding-001","content":{"parts":[{"text":"a"}]}},{"model":"models/gemini-embedding-001","content":{"parts":[{"text":"bb"}]}}]}`
	if string(b) != want {
		t.Fatalf("got %s", b)
	}
	resp, err := DecodeEmbedResponse([]byte(`{"embeddings":[{"values":[0.1,0.2]},{"values":[0.3]}]}`), "gemini-embedding-001", []string{"a", "bb"})
	if err != nil || len(resp.Vectors) != 2 || resp.Vectors[1][0] != 0.3 || resp.Usage.InputTokens != 2 || resp.Model != "gemini-embedding-001" {
		t.Fatalf("resp %+v %v", resp, err)
	}
}

func TestModelPath(t *testing.T) {
	for model, want := range map[string]string{"gemini-2.5-flash": "models/gemini-2.5-flash", "models/gemini-3.1-pro-preview": "models/gemini-3.1-pro-preview", "tuned_model.v2": "models/tuned_model.v2"} {
		if got, err := ModelPath(model); err != nil || got != want {
			t.Errorf("ModelPath(%q) = %q, %v", model, got, err)
		}
	}
	for _, bad := range []string{"", "a/../b", "x:generateContent", "m?key=1", "m#frag", "m%2F", "models/", "../models/x", "a b"} {
		if _, err := ModelPath(bad); err == nil {
			t.Errorf("ModelPath(%q) accepted", bad)
		}
	}
}

func TestEncodeRequestSkipsEmptyTurns(t *testing.T) {
	b, err := EncodeRequest(&schema.ChatRequest{Model: "m", Messages: []schema.Message{
		{Role: schema.RoleUser, Content: "x"},
		{Role: schema.RoleAssistant},
		{Role: schema.RoleUser},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if compact(t, string(b)) != `{"contents":[{"parts":[{"text":"x"}],"role":"user"}]}` {
		t.Fatalf("wire %s", b)
	}
}
