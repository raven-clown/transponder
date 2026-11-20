package schema

import (
	"reflect"
	"testing"
)

func TestText(t *testing.T) {
	if got := Text(""); got != nil {
		t.Fatalf("Text(\"\") = %+v, want nil", got)
	}
	want := []Part{{Type: PartText, Text: "hi"}}
	if got := Text("hi"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Text(\"hi\") = %+v, want %+v", got, want)
	}
}

func TestMessageText(t *testing.T) {
	png := &Image{MediaType: "image/png", Data: "aGk="}
	tests := []struct {
		name    string
		content []Part
		text    string
		images  bool
	}{
		{"empty", nil, "", false},
		{"one text part", Text("hello"), "hello", false},
		{"text parts join in order", []Part{{Type: PartText, Text: "a"}, {Type: PartText, Text: "b"}}, "ab", false},
		{"images are left out of the text", []Part{
			{Type: PartText, Text: "what is "},
			{Type: PartImage, Image: png},
			{Type: PartText, Text: "this?"},
		}, "what is this?", true},
		{"image only", []Part{{Type: PartImage, Image: &Image{URL: "https://example.com/a.png"}}}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Message{Role: RoleUser, Content: tt.content}
			if got := m.Text(); got != tt.text {
				t.Errorf("Text() = %q, want %q", got, tt.text)
			}
			if got := m.HasImages(); got != tt.images {
				t.Errorf("HasImages() = %v, want %v", got, tt.images)
			}
			req := ChatRequest{Messages: []Message{{Role: RoleSystem, Content: Text("s")}, m}}
			if got := req.HasImages(); got != tt.images {
				t.Errorf("ChatRequest.HasImages() = %v, want %v", got, tt.images)
			}
		})
	}
}

func TestImageInline(t *testing.T) {
	if (&Image{URL: "https://example.com/a.png"}).Inline() {
		t.Error("an image at a URL is not inline")
	}
	if !(&Image{MediaType: "image/png", Data: "aGk="}).Inline() {
		t.Error("an image with data is inline")
	}
}
