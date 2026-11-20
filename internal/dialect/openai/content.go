package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/raven-clown/transponder/internal/schema"
)

type wirePart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
}

type wireImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

func badContent(code, msg string) *RequestError {
	return &RequestError{Param: "messages", Code: code, Message: msg}
}

// messageContent reads the content of a request message: a string, null, or a list of text
// and image_url parts. Parts keep their order, and text parts next to each other are joined.
func messageContent(raw json.RawMessage, role string) ([]schema.Part, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return schema.Text(s), nil
	}
	var parts []wirePart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, badContent("unsupported", "message content must be a string or a list of content parts")
	}
	var out []schema.Part
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = schema.AppendText(out, p.Text)
		case "image_url":
			if role != schema.RoleUser {
				return nil, badContent("unsupported", "image_url parts are only supported in user messages")
			}
			img, err := decodeImage(p.ImageURL)
			if err != nil {
				return nil, err
			}
			out = append(out, schema.Part{Type: schema.PartImage, Image: img})
		default:
			return nil, badContent("unsupported", "only text and image_url content parts are supported")
		}
	}
	return out, nil
}

// decodeImage reads an image_url part: an http or https URL, or a base64 data: URL.
func decodeImage(w *wireImageURL) (*schema.Image, error) {
	if w == nil || w.URL == "" {
		return nil, badContent(schema.CodeInvalidImage, "an image_url part needs image_url.url")
	}
	var img *schema.Image
	switch {
	case strings.HasPrefix(w.URL, "data:"):
		header, data, ok := strings.Cut(w.URL[len("data:"):], ",")
		mediaType, isBase64 := strings.CutSuffix(header, ";base64")
		if !ok || !isBase64 {
			return nil, badContent(schema.CodeInvalidImage, "a data: image URL must look like data:image/png;base64,...")
		}
		// Parameters such as ;charset= say nothing about an image.
		mediaType, _, _ = strings.Cut(mediaType, ";")
		var err error
		if img, err = schema.NewInlineImage(mediaType, data); err != nil {
			var ie *schema.ImageError
			if errors.As(err, &ie) {
				return nil, badContent(ie.Code, ie.Message)
			}
			return nil, err
		}
	case strings.HasPrefix(w.URL, "https://"), strings.HasPrefix(w.URL, "http://"):
		img = &schema.Image{URL: w.URL}
	default:
		return nil, badContent(schema.CodeInvalidImage, "image_url.url must be an http, https or data: URL")
	}
	img.Detail = w.Detail
	return img, nil
}

// encodeContent renders message content. Text alone stays a string, as it was before images;
// with an image it becomes a list of parts in order.
func encodeContent(m schema.Message) (json.RawMessage, error) {
	if !m.HasImages() {
		text := m.Text()
		if text == "" && len(m.ToolCalls) > 0 {
			return json.RawMessage("null"), nil
		}
		return json.Marshal(text)
	}
	parts := make([]wirePart, 0, len(m.Content))
	for _, p := range m.Content {
		switch {
		case p.Type == schema.PartImage && p.Image != nil:
			url := p.Image.URL
			if p.Image.Inline() {
				url = "data:" + p.Image.MediaType + ";base64," + p.Image.Data
			}
			parts = append(parts, wirePart{Type: "image_url", ImageURL: &wireImageURL{URL: url, Detail: p.Image.Detail}})
		case p.Type == schema.PartText && p.Text != "":
			parts = append(parts, wirePart{Type: "text", Text: p.Text})
		}
	}
	return json.Marshal(parts)
}
