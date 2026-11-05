// Package schema holds the provider-neutral request and response types every adapter converts to and from.
package schema

// Message is one turn of a conversation.
type Message struct {
	Role    string
	Content string
}

// ChatRequest is a chat call in canonical form.
type ChatRequest struct {
	Model       string
	Messages    []Message
	MaxTokens   *int
	Temperature *float64
	Stream      bool
}

// ChatResponse is a finished chat call.
type ChatResponse struct {
	ID           string
	Model        string
	Message      Message
	FinishReason string
	Usage        Usage
}

// Usage counts the tokens of one call.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// EventType names a stream event.
type EventType string

// Stream event types, in the order a stream sends them.
const (
	EventStart  EventType = "start"
	EventText   EventType = "text"
	EventFinish EventType = "finish"
	EventUsage  EventType = "usage"
)

// Event is one item of a streamed response.
type Event struct {
	Type         EventType
	Text         string
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
