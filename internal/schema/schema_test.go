package schema

import (
	"errors"
	"reflect"
	"strings"
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

func TestAppendText(t *testing.T) {
	img := Part{Type: PartImage, Image: &Image{URL: "https://example.com/a.png"}}
	var c []Part
	c = AppendText(c, "")
	c = AppendText(c, "a")
	c = AppendText(c, "b")
	c = append(c, img)
	c = AppendText(c, "")
	c = AppendText(c, "c")
	want := []Part{{Type: PartText, Text: "ab"}, img, {Type: PartText, Text: "c"}}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("content = %+v, want %+v", c, want)
	}
}

func TestNewInlineImage(t *testing.T) {
	// 5 MiB is two bytes more than a multiple of three.
	atLimit := strings.Repeat("AAAA", MaxInlineImageBytes/3) + "AAA="
	over := strings.Repeat("AAAA", MaxInlineImageBytes/3+1)
	tests := []struct {
		name, mediaType, data string
		code                  string
	}{
		{"png", "image/png", "aGk=", ""},
		{"media type in upper case with spaces", " IMAGE/PNG ", "aGk=", ""},
		{"no padding needed", "image/webp", "QUJD", ""},
		{"two padding characters", "image/jpeg", "QQ==", ""},
		{"plus and slash", "image/gif", "+/+/", ""},
		{"exactly the limit", "image/png", atLimit, ""},
		{"one byte over the limit", "image/png", over, CodeImageTooLarge},
		{"not an image type", "application/pdf", "aGk=", CodeInvalidImage},
		{"image with no subtype", "image/", "aGk=", CodeInvalidImage},
		{"no media type", "", "aGk=", CodeInvalidImage},
		{"no data", "image/png", "", CodeInvalidImage},
		{"missing padding", "image/png", "aGk", CodeInvalidImage},
		{"url-safe alphabet", "image/png", "-_-_", CodeInvalidImage},
		{"a line break", "image/png", "aG\nk", CodeInvalidImage},
		{"padding in the middle", "image/png", "a=Gk", CodeInvalidImage},
		{"three padding characters", "image/png", "a===", CodeInvalidImage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			img, err := NewInlineImage(tt.mediaType, tt.data)
			if tt.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				if img.MediaType != strings.ToLower(strings.TrimSpace(tt.mediaType)) || img.Data != tt.data || !img.Inline() {
					t.Fatalf("image = %+v", img)
				}
				return
			}
			var ie *ImageError
			if !errors.As(err, &ie) || ie.Code != tt.code || ie.Error() == "" {
				t.Fatalf("err = %v, want an ImageError with code %q", err, tt.code)
			}
		})
	}
}

func TestContentError(t *testing.T) {
	var err error = &ContentError{Reason: "no images here"}
	if err.Error() != "no images here" {
		t.Fatalf("Error() = %q", err.Error())
	}
}
