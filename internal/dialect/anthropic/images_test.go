package anthropic

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/schema"
)

const png = "iVBORw0KGgo="

func text(s string) schema.Part { return schema.Part{Type: schema.PartText, Text: s} }

func inline(mediaType string) schema.Part {
	return schema.Part{Type: schema.PartImage, Image: &schema.Image{MediaType: mediaType, Data: png}}
}

func at(url string) schema.Part {
	return schema.Part{Type: schema.PartImage, Image: &schema.Image{URL: url}}
}

func TestDecodeImageBlocks(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[
		{"role":"user","content":[
			{"type":"text","text":"compare "},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png + `"}},
			{"type":"text","text":"with "},
			{"type":"text","text":"this"},
			{"type":"image","source":{"type":"url","url":"https://example.com/b.webp"},"cache_control":{"type":"ephemeral"}}
		]},
		{"role":"assistant","content":"They differ."},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","content":"ok"},
			{"type":"image","source":{"type":"url","url":"http://example.com/c.gif"}},
			{"type":"text","text":"and this one?"}
		]}
	]}`
	got, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []schema.Message{
		{Role: "user", Content: []schema.Part{text("compare "), inline("image/png"), text("with this"), at("https://example.com/b.webp")}},
		{Role: "assistant", Content: schema.Text("They differ.")},
		{Role: "tool", ToolCallID: "t1", Content: schema.Text("ok")},
		{Role: "user", Content: []schema.Part{at("http://example.com/c.gif"), text("and this one?")}},
	}
	if !reflect.DeepEqual(got.Messages, want) {
		t.Fatalf("messages\n got %+v\nwant %+v", got.Messages, want)
	}
}

func TestDecodeImageBlockErrors(t *testing.T) {
	tooLarge := strings.Repeat("AAAA", schema.MaxInlineImageBytes/3+1)
	user := func(block string) string {
		return `{"model":"m","messages":[{"role":"user","content":[` + block + `]}]}`
	}
	tests := []struct {
		name, body, message string
	}{
		{"no source", user(`{"type":"image"}`), "needs a source"},
		{"unknown source", user(`{"type":"image","source":{"type":"s3"}}`), "base64 or url"},
		{"file source", user(`{"type":"image","source":{"type":"file","file_id":"file_1"}}`), "can only go to an Anthropic target"},
		{"url without a scheme", user(`{"type":"image","source":{"type":"url","url":"example.com/a.png"}}`), "http or https"},
		{"not an image", user(`{"type":"image","source":{"type":"base64","media_type":"application/pdf","data":"QUJD"}}`), "image media type"},
		{"bad base64", user(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"***"}}`), "base64"},
		{"too large", user(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + tooLarge + `"}}`), "over the 5 MiB limit"},
		{"image from the assistant", `{"model":"m","messages":[{"role":"assistant","content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}]}`, "only supported in user messages"},
		{"image in a tool result", user(`{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}`), "can only go to an Anthropic target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeRequest([]byte(tt.body))
			var re *RequestError
			if !errors.As(err, &re) || !strings.Contains(re.Message, tt.message) {
				t.Fatalf("err = %v, want a RequestError with %q", err, tt.message)
			}
		})
	}
}

func TestEncodeImageBlocks(t *testing.T) {
	req := &schema.ChatRequest{Model: "m", Messages: []schema.Message{
		{Role: "user", Content: []schema.Part{text("compare "), inline("image/jpeg"), text(""), text("with"), at("https://example.com/b.webp")}},
		{Role: "user", Content: []schema.Part{inline("image/gif")}},
	}}
	b, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	// Two user turns in a row share one message, as Anthropic wants.
	want := `[{"type":"text","text":"compare "},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` + png + `"}},` +
		`{"type":"text","text":"with"},` +
		`{"type":"image","source":{"type":"url","url":"https://example.com/b.webp"}},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/gif","data":"` + png + `"}}]`
	if len(w.Messages) != 1 || w.Messages[0].Role != "user" || string(w.Messages[0].Content) != want {
		t.Fatalf("messages\n got %s\nwant %s", b, want)
	}
}

func TestEncodeRequestCannotCarry(t *testing.T) {
	tests := []struct {
		name   string
		msg    schema.Message
		reason string
	}{
		{"a media type Anthropic does not take", schema.Message{Role: "user", Content: []schema.Part{inline("image/heic")}}, "not image/heic"},
		{"an image in a system message", schema.Message{Role: "system", Content: []schema.Part{text("x"), at("https://example.com/a.png")}}, "not in system messages"},
		{"an image in a tool result", schema.Message{Role: "tool", ToolCallID: "t", Content: []schema.Part{at("https://example.com/a.png")}}, "not in tool messages"},
		{"an image from the assistant", schema.Message{Role: "assistant", Content: []schema.Part{at("https://example.com/a.png")}}, "not in assistant messages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := EncodeRequest(&schema.ChatRequest{Model: "m", Messages: []schema.Message{tt.msg}})
			var ce *schema.ContentError
			if !errors.As(err, &ce) || !strings.Contains(ce.Reason, tt.reason) {
				t.Fatalf("err = %v, want a ContentError with %q", err, tt.reason)
			}
		})
	}
}

func TestImagesRoundTrip(t *testing.T) {
	want := &schema.ChatRequest{Model: "m", MaxTokens: ptr(64), Messages: []schema.Message{
		{Role: schema.RoleSystem, Content: schema.Text("be brief")},
		{Role: schema.RoleUser, Content: []schema.Part{
			text("what changed between "), inline("image/png"), text(" and "), at("https://example.com/after.png"), text("?")}},
		{Role: schema.RoleAssistant, Content: schema.Text("The sky.")},
		{Role: schema.RoleUser, Content: []schema.Part{at("https://example.com/c.gif")}},
	}}
	b, err := EncodeRequest(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRequest(b)
	if err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip\n got %+v\nwant %+v\nwire %s", got, want, b)
	}
}
