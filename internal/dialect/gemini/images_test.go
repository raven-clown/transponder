package gemini

import (
	"encoding/json"
	"errors"
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

func TestEncodeInlineImages(t *testing.T) {
	req := &schema.ChatRequest{Model: "gemini-2.5-flash", Messages: []schema.Message{
		{Role: schema.RoleUser, Content: []schema.Part{text("compare "), inline("image/png"), text(""), text("with"), inline("image/heic")}},
		{Role: schema.RoleAssistant, Content: schema.Text("They differ.")},
		{Role: schema.RoleUser, Content: []schema.Part{inline("image/webp")}},
	}}
	b, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	want := `[{"role":"user","parts":[{"text":"compare "},` +
		`{"inlineData":{"mimeType":"image/png","data":"` + png + `"}},` +
		`{"text":"with"},` +
		`{"inlineData":{"mimeType":"image/heic","data":"` + png + `"}}]},` +
		`{"role":"model","parts":[{"text":"They differ."}]},` +
		`{"role":"user","parts":[{"inlineData":{"mimeType":"image/webp","data":"` + png + `"}}]}]`
	if string(w.Contents) != want {
		t.Fatalf("contents\n got %s\nwant %s", w.Contents, want)
	}
}

func TestEncodeRequestCannotCarry(t *testing.T) {
	tests := []struct {
		name   string
		msg    schema.Message
		reason string
	}{
		{"an image at a URL", schema.Message{Role: "user", Content: []schema.Part{text("x"), at("https://example.com/a.png")}}, "not an image at a URL"},
		{"a media type Gemini does not take", schema.Message{Role: "user", Content: []schema.Part{inline("image/gif")}}, "not image/gif"},
		{"an image in a system message", schema.Message{Role: "system", Content: []schema.Part{inline("image/png")}}, "not in system messages"},
		{"an image in a tool result", schema.Message{Role: "tool", ToolCallID: "t", Content: []schema.Part{inline("image/png")}}, "not in tool messages"},
		{"an image from the assistant", schema.Message{Role: "assistant", Content: []schema.Part{inline("image/png")}}, "not in assistant messages"},
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
