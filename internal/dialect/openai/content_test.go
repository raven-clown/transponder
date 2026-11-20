package openai

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

func at(url, detail string) schema.Part {
	return schema.Part{Type: schema.PartImage, Image: &schema.Image{URL: url, Detail: detail}}
}

func TestDecodeContentParts(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []schema.Part
	}{
		{"string", `"hi"`, schema.Text("hi")},
		{"null", `null`, nil},
		{"empty string", `""`, nil},
		{"text parts are joined", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, schema.Text("ab")},
		{"image at a url", `[{"type":"image_url","image_url":{"url":"https://example.com/cat.png","detail":"low"}}]`,
			[]schema.Part{at("https://example.com/cat.png", "low")}},
		{"data url", `[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}}]`,
			[]schema.Part{inline("image/png")}},
		{"data url with a parameter and upper case", `[{"type":"image_url","image_url":{"url":"data:IMAGE/JPEG;charset=utf-8;base64,` + png + `"}}]`,
			[]schema.Part{inline("image/jpeg")}},
		{"text and images keep their order", `[
			{"type":"text","text":"compare "},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}},
			{"type":"text","text":"with "},
			{"type":"text","text":"this"},
			{"type":"image_url","image_url":{"url":"http://example.com/b.webp"}},
			{"type":"text","text":""}]`,
			[]schema.Part{text("compare "), inline("image/png"), text("with this"), at("http://example.com/b.webp", "")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := ParseChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":` + tt.content + `}]}`))
			if err != nil {
				t.Fatal(err)
			}
			if got := req.Messages[0].Content; !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("content\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestDecodeContentPartErrors(t *testing.T) {
	tooLarge := strings.Repeat("AAAA", schema.MaxInlineImageBytes/3+1)
	image := func(url string) string {
		return `[{"type":"image_url","image_url":{"url":"` + url + `"}}]`
	}
	tests := []struct {
		name, role, content, code, message string
	}{
		{"not a list", "user", `{"type":"text"}`, "unsupported", "string or a list"},
		{"image_url as a string", "user", `[{"type":"image_url","image_url":"https://example.com/a.png"}]`, "unsupported", "string or a list"},
		{"audio", "user", `[{"type":"input_audio","input_audio":{}}]`, "unsupported", "only text and image_url"},
		{"image in a system message", "system", image("https://example.com/a.png"), "unsupported", "only supported in user messages"},
		{"image in a tool message", "tool", image("https://example.com/a.png"), "unsupported", "only supported in user messages"},
		{"no image_url", "user", `[{"type":"image_url"}]`, "invalid_image", "needs image_url.url"},
		{"empty url", "user", image(""), "invalid_image", "needs image_url.url"},
		{"file url", "user", image("file:///etc/passwd"), "invalid_image", "http, https or data"},
		{"data url without base64", "user", image("data:image/png,abc"), "invalid_image", "data:image/png;base64"},
		{"data url without a comma", "user", image("data:image/png;base64"), "invalid_image", "data:image/png;base64"},
		{"not an image", "user", image("data:application/pdf;base64,QUJD"), "invalid_image", "image media type"},
		{"no media type", "user", image("data:;base64,QUJD"), "invalid_image", "image media type"},
		{"bad base64", "user", image("data:image/png;base64,not base64!"), "invalid_image", "base64"},
		{"base64 without padding", "user", image("data:image/png;base64,QUJDRA"), "invalid_image", "base64"},
		{"empty data", "user", image("data:image/png;base64,"), "invalid_image", "base64"},
		{"too large", "user", image("data:image/png;base64," + tooLarge), "image_too_large", "over the 5 MiB limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseChatRequest([]byte(`{"model":"m","messages":[{"role":"` + tt.role + `","content":` + tt.content + `}]}`))
			var re *RequestError
			if !errors.As(err, &re) {
				t.Fatalf("err = %v, want *RequestError", err)
			}
			if re.Param != "messages" || re.Code != tt.code || !strings.Contains(re.Message, tt.message) {
				t.Fatalf("param %q code %q message %q, want code %q and %q in the message", re.Param, re.Code, re.Message, tt.code, tt.message)
			}
		})
	}
}

func TestInlineImageAtTheLimit(t *testing.T) {
	// 5 MiB is two bytes more than a multiple of three, so the last group is padded.
	data := strings.Repeat("AAAA", schema.MaxInlineImageBytes/3) + "AAA="
	req, err := ParseChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + data + `"}}]}]}`))
	if err != nil {
		t.Fatalf("an image of exactly the limit was refused: %v", err)
	}
	if got := len(req.Messages[0].Content[0].Image.Data); got != len(data) {
		t.Fatalf("data length %d, want %d", got, len(data))
	}
}

func TestEncodeContentParts(t *testing.T) {
	tests := []struct {
		name string
		msg  schema.Message
		want string
	}{
		{"text stays a string", schema.Message{Role: "user", Content: schema.Text("hi")}, `"hi"`},
		{"two text parts stay a string", schema.Message{Role: "user", Content: []schema.Part{text("a"), text("b")}}, `"ab"`},
		{"no content with tool calls is null", schema.Message{Role: "assistant", ToolCalls: []schema.ToolCall{{ID: "c", Name: "f", Arguments: "{}"}}}, `null`},
		{"parts in order", schema.Message{Role: "user", Content: []schema.Part{
			text("compare "), inline("image/png"), text(""), text("with"), at("https://example.com/b.webp", "high")}},
			`[{"type":"text","text":"compare "},` +
				`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}},` +
				`{"type":"text","text":"with"},` +
				`{"type":"image_url","image_url":{"url":"https://example.com/b.webp","detail":"high"}}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := EncodeChatRequest(&schema.ChatRequest{Model: "m", Messages: []schema.Message{tt.msg}})
			if err != nil {
				t.Fatal(err)
			}
			var w struct {
				Messages []struct {
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(b, &w); err != nil {
				t.Fatal(err)
			}
			if got := string(w.Messages[0].Content); got != tt.want {
				t.Fatalf("content\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestImagesRoundTrip(t *testing.T) {
	want := &schema.ChatRequest{Model: "m", Messages: []schema.Message{
		{Role: schema.RoleSystem, Content: schema.Text("be brief")},
		{Role: schema.RoleUser, Content: []schema.Part{
			text("what changed between "), inline("image/jpeg"), text(" and "), at("https://example.com/after.png", "low"), text("?")}},
		{Role: schema.RoleAssistant, Content: schema.Text("The sky.")},
		{Role: schema.RoleUser, Content: []schema.Part{at("https://example.com/c.gif", "")}},
	}}
	b, err := EncodeChatRequest(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseChatRequest(b)
	if err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip\n got %+v\nwant %+v\nwire %s", got, want, b)
	}
}
