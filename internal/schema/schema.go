// Package schema holds the provider-neutral request and response types every adapter converts to and from.
package schema

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Roles of a message.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Finish reasons.
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolCalls     = "tool_calls"
	FinishContentFilter = "content_filter"
)

// Content part types.
const (
	PartText  = "text"
	PartImage = "image"
)

// Part is one piece of a message's content. Text is set for PartText and Image for PartImage.
type Part struct {
	Type  string
	Text  string
	Image *Image
}

// Image is a picture in a message: one at a URL, or an inline one held as Data with its
// MediaType. Data stays base64, as every dialect carries it, so a large image is never decoded.
type Image struct {
	URL       string
	MediaType string
	Data      string
	// Detail is OpenAI's hint for how closely to look: "low", "high" or "auto". Empty when unset.
	Detail string
}

// Inline reports whether the image travels inside the request instead of at a URL.
func (i *Image) Inline() bool { return i.URL == "" }

// MaxInlineImageBytes is the largest inline image, decoded, that a translated request may
// carry. As base64 it stays under the per-image limit of every provider (Anthropic's 10 MB is
// the lowest), so an image that passes can go to any target that takes images.
const MaxInlineImageBytes = 5 << 20

// Codes of an ImageError.
const (
	CodeInvalidImage  = "invalid_image"
	CodeImageTooLarge = "image_too_large"
)

// ImageError is a problem with an image the caller sent. Each dialect reports it as its own 400.
type ImageError struct {
	Code    string
	Message string
}

func (e *ImageError) Error() string { return e.Message }

// NewInlineImage checks an inline image and returns it. mediaType must be an image type and
// data padded standard base64 of at most MaxInlineImageBytes once decoded.
func NewInlineImage(mediaType, data string) (*Image, error) {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if !strings.HasPrefix(mediaType, "image/") || len(mediaType) == len("image/") {
		return nil, &ImageError{Code: CodeInvalidImage, Message: "an inline image needs an image media type, such as image/png"}
	}
	size, ok := base64Size(data)
	if !ok {
		return nil, &ImageError{Code: CodeInvalidImage, Message: "an inline image must be base64 with padding"}
	}
	if size > MaxInlineImageBytes {
		return nil, &ImageError{Code: CodeImageTooLarge, Message: fmt.Sprintf(
			"an inline image is %.1f MiB, over the %d MiB limit; send a smaller image or its URL",
			float64(size)/(1<<20), MaxInlineImageBytes>>20)}
	}
	return &Image{MediaType: mediaType, Data: data}, nil
}

// base64Size returns the decoded size of padded standard base64, without decoding it.
func base64Size(s string) (int, bool) {
	n := len(s)
	if n == 0 || n%4 != 0 {
		return 0, false
	}
	pad := 0
	for pad < 2 && s[n-1-pad] == '=' {
		pad++
	}
	for i := 0; i < n-pad; i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '+' && c != '/' {
			return 0, false
		}
	}
	return n/4*3 - pad, true
}

// ContentError says a target cannot carry part of a request, such as an image at a URL going
// to a provider that only takes inline images. The gateway skips that target.
type ContentError struct {
	Reason string
}

func (e *ContentError) Error() string { return e.Reason }

// Text returns s as message content: one text part, or none when s is empty.
func Text(s string) []Part {
	if s == "" {
		return nil
	}
	return []Part{{Type: PartText, Text: s}}
}

// AppendText adds text to content, joining it to a text part at the end, so text between two
// images stays one part. Empty text adds nothing.
func AppendText(content []Part, s string) []Part {
	if s == "" {
		return content
	}
	if n := len(content); n > 0 && content[n-1].Type == PartText {
		content[n-1].Text += s
		return content
	}
	return append(content, Part{Type: PartText, Text: s})
}

// Message is one turn of a conversation. Content holds its parts in the order they were sent.
// An assistant turn may carry tool calls; a tool turn carries the result of the call named by
// ToolCallID.
type Message struct {
	Role       string
	Content    []Part
	ToolCalls  []ToolCall
	ToolCallID string
}

// Text returns the text parts of the message joined in order, without its images.
func (m Message) Text() string {
	// One text part is the common case and needs no copy.
	if len(m.Content) == 1 && m.Content[0].Type == PartText {
		return m.Content[0].Text
	}
	var b strings.Builder
	for _, p := range m.Content {
		if p.Type == PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// HasImages reports whether any part of the message is an image.
func (m Message) HasImages() bool {
	for _, p := range m.Content {
		if p.Type == PartImage {
			return true
		}
	}
	return false
}

// ToolCall is a request from the model to run a tool. Arguments is JSON text.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Tool is a function the model may call. Parameters is a JSON Schema.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Tool choice modes.
const (
	ToolChoiceAuto     = "auto"
	ToolChoiceNone     = "none"
	ToolChoiceRequired = "required"
	ToolChoiceTool     = "tool"
)

// ToolChoice says whether and which tool the model must call. Name is set for ToolChoiceTool.
type ToolChoice struct {
	Mode string
	Name string
}

// ChatRequest is a chat call in canonical form. System prompts are messages with RoleSystem.
type ChatRequest struct {
	Model       string
	Messages    []Message
	MaxTokens   *int
	Temperature *float64
	TopP        *float64
	Stop        []string
	Tools       []Tool
	ToolChoice  *ToolChoice
	Stream      bool
}

// HasImages reports whether any message of the request carries an image.
func (r *ChatRequest) HasImages() bool {
	for _, m := range r.Messages {
		if m.HasImages() {
			return true
		}
	}
	return false
}

// ChatResponse is a finished chat call.
type ChatResponse struct {
	ID           string
	Model        string
	Message      Message
	FinishReason string
	Usage        Usage
}

// Usage counts the tokens of one call. InputTokens excludes cache reads and writes.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	// CacheWrite1hTokens is the part of CacheWriteTokens cached for an hour, which Anthropic
	// prices higher than the five-minute default.
	CacheWrite1hTokens int64
	// ReasoningTokens are thinking tokens a provider reports apart from the output. They are
	// billed at the output price. Providers that count them inside the output leave this zero.
	ReasoningTokens int64
}

// EventType names a stream event.
type EventType string

// Stream event types. A stream sends start, then text and tool events, then finish and usage.
const (
	EventStart     EventType = "start"
	EventText      EventType = "text"
	EventToolStart EventType = "tool_start"
	EventToolDelta EventType = "tool_delta"
	EventFinish    EventType = "finish"
	EventUsage     EventType = "usage"
)

// Event is one item of a streamed response. Tool events carry ToolIndex, the position of the
// call in the message; ToolStart also carries ToolID and ToolName, ToolDelta a piece of ToolArgs.
type Event struct {
	Type         EventType
	Text         string
	ToolIndex    int
	ToolID       string
	ToolName     string
	ToolArgs     string
	FinishReason string
	Usage        *Usage
}

// Stream yields events until Recv returns io.EOF.
type Stream interface {
	Recv() (Event, error)
	Close() error
}

// EmbedRequest asks for one vector per input.
type EmbedRequest struct {
	Model string
	Input []string
}

// EmbedResponse holds the vectors in input order.
type EmbedResponse struct {
	Model   string
	Vectors [][]float32
	Usage   Usage
}
