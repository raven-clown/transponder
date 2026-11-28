package openai

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/sse"
)

// ResponsesPeek is what the gateway reads from a Responses API request before forwarding it.
type ResponsesPeek struct {
	Model  string
	Stream bool
	// Background asks the provider to run the response after the call returns.
	Background bool
}

// PeekResponses reads the routing fields of a Responses API request. Keys are matched exactly,
// as in PeekRequest.
func PeekResponses(body []byte) (ResponsesPeek, error) {
	if !json.Valid(body) {
		return ResponsesPeek{}, invalidJSON("the request body is not a JSON object")
	}
	var buf [32]member
	ms, ok := objectMembers(body, buf[:0])
	if !ok {
		return ResponsesPeek{}, invalidJSON("the request body is not a JSON object")
	}
	var p ResponsesPeek
	if err := field(ms, "model", &p.Model); err != nil {
		return ResponsesPeek{}, err
	}
	if err := field(ms, "stream", &p.Stream); err != nil {
		return ResponsesPeek{}, err
	}
	if err := field(ms, "background", &p.Background); err != nil {
		return ResponsesPeek{}, err
	}
	if p.Model == "" {
		return ResponsesPeek{}, &RequestError{Param: "model", Code: "missing_field", Message: "model is required"}
	}
	return p, nil
}

type wireResponseUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	TotalTokens        int64 `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// canonical splits cached input out of input tokens and reasoning out of output tokens, which
// include them on OpenAI. A server that reports reasoning apart, as xAI does for chat, has a
// total that only adds up with reasoning on top, and keeps its output tokens whole.
func (u *wireResponseUsage) canonical() schema.Usage {
	out := schema.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if u.InputTokensDetails != nil {
		out.CacheReadTokens = u.InputTokensDetails.CachedTokens
		out.InputTokens -= out.CacheReadTokens
	}
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
		apart := out.ReasoningTokens > 0 && u.TotalTokens == u.InputTokens+u.OutputTokens+out.ReasoningTokens
		if !apart {
			out.OutputTokens -= out.ReasoningTokens
		}
	}
	return out
}

type wireResponseObject struct {
	Usage  *wireResponseUsage `json:"usage"`
	Output []struct {
		Arguments string `json:"arguments"`
		Content   []struct {
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
	} `json:"output"`
}

// ParseResponseUsage reads the usage object of a response, as the body of a call that was not
// streamed carries it.
func ParseResponseUsage(body []byte) (schema.Usage, bool) {
	var w wireResponseObject
	if err := json.Unmarshal(body, &w); err != nil || w.Usage == nil {
		return schema.Usage{}, false
	}
	return w.Usage.canonical(), true
}

// ResponseText joins what the model wrote in a response: message text, refusals, reasoning
// summaries and tool call arguments. It is for estimating output tokens when the provider
// reports no usage.
func ResponseText(body []byte) string {
	var w wireResponseObject
	if json.Unmarshal(body, &w) != nil {
		return string(body)
	}
	var b strings.Builder
	for _, item := range w.Output {
		b.WriteString(item.Arguments)
		for _, c := range item.Content {
			b.WriteString(c.Text)
			b.WriteString(c.Refusal)
		}
		for _, s := range item.Summary {
			b.WriteString(s.Text)
		}
	}
	return b.String()
}

// Events of a Responses API stream that the gateway acts on.
const (
	responseCompleted  = "response.completed"
	responseIncomplete = "response.incomplete"
	responseFailed     = "response.failed"
	responseError      = "error"
)

// ResponseStream follows a Responses API stream as its events pass through.
type ResponseStream struct {
	usage     schema.Usage
	haveUsage bool
	done      bool
	failed    bool
	seq       int64
	text      strings.Builder
}

// Add reads one event of the stream.
func (s *ResponseStream) Add(data []byte) {
	var w struct {
		Type     string              `json:"type"`
		Seq      int64               `json:"sequence_number"`
		Delta    json.RawMessage     `json:"delta"`
		Response *wireResponseObject `json:"response"`
	}
	if json.Unmarshal(data, &w) != nil {
		return
	}
	s.seq = max(s.seq, w.Seq)
	switch w.Type {
	case responseCompleted, responseIncomplete, responseFailed:
		s.done = true
		if w.Response != nil && w.Response.Usage != nil {
			s.usage, s.haveUsage = w.Response.Usage.canonical(), true
		}
	case responseError:
		s.failed = true
	default:
		// Text, refusal, reasoning summary and tool argument deltas are all strings.
		var delta string
		if strings.HasSuffix(w.Type, ".delta") && json.Unmarshal(w.Delta, &delta) == nil {
			s.text.WriteString(delta)
		}
	}
}

// Usage returns the usage of the event that ended the response, if it carried one.
func (s *ResponseStream) Usage() (schema.Usage, bool) { return s.usage, s.haveUsage }

// Done reports whether the response reached its last event: completed, incomplete or failed.
func (s *ResponseStream) Done() bool { return s.done }

// Failed reports whether the provider sent an error event, which ends the stream.
func (s *ResponseStream) Failed() bool { return s.failed }

// Text returns the deltas seen so far, for estimating output tokens.
func (s *ResponseStream) Text() string { return s.text.String() }

// ErrorFrame renders the error event that ends a stream the gateway had to cut, numbered after
// the last event seen.
func (s *ResponseStream) ErrorFrame(code, message string) []byte {
	b, _ := json.Marshal(struct {
		Type    string  `json:"type"`
		Code    string  `json:"code"`
		Message string  `json:"message"`
		Param   *string `json:"param"`
		Seq     int64   `json:"sequence_number"`
	}{responseError, code, message, nil, s.seq + 1})
	return sse.Frame(responseError, b)
}

// OpeningResponseEvent reports whether a Responses API stream event comes before any output:
// response.queued, response.created or response.in_progress.
func OpeningResponseEvent(ev sse.Event) bool {
	// OpenAI names each event after its type, which saves decoding a whole response object.
	typ := ev.Name
	if typ == "" {
		var w struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(ev.Data, &w) != nil {
			return false
		}
		typ = w.Type
	}
	switch typ {
	case "response.queued", "response.created", "response.in_progress":
		return true
	}
	return false
}

// IsResponseStreamError reports whether a stream event is the provider's error event.
func IsResponseStreamError(ev sse.Event) bool {
	if ev.Name == responseError {
		return true
	}
	var w struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(ev.Data, &w) == nil && w.Type == responseError
}

// ResponsesRefs is what a Responses API request takes from outside its own body: state the
// provider stored earlier, and content the gateway cannot count before the call.
type ResponsesRefs struct {
	PreviousResponse bool // previous_response_id is set
	Conversation     bool // conversation is set
	ItemReference    bool // input names an item the provider stored, with or without type item_reference
	Prompt           bool // prompt names a prompt stored with the provider
	Files            int  // input_file parts in input, by id, by URL or inline
}

// Stateful reports the first field that continues stored state, or "" when there is none.
func (r ResponsesRefs) Stateful() string {
	switch {
	case r.PreviousResponse:
		return "previous_response_id"
	case r.Conversation:
		return "conversation"
	case r.ItemReference:
		return "input"
	}
	return ""
}

// ReadResponsesRefs lists the references of a Responses API request. Keys are matched without
// regard to case, as a server written in Go reads them, and a null value is no reference. A
// body that cannot be read has none; PeekResponses refuses it first.
func ReadResponsesRefs(body []byte) ResponsesRefs {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return ResponsesRefs{}
	}
	var r ResponsesRefs
	r.PreviousResponse = hasField(m, "previous_response_id")
	r.Conversation = hasField(m, "conversation")
	r.Prompt = hasField(m, "prompt")
	for k, raw := range m {
		if !strings.EqualFold(k, "input") {
			continue
		}
		var v any
		if json.Unmarshal(raw, &v) == nil {
			if items, ok := v.([]any); ok && slices.ContainsFunc(items, storedItem) {
				r.ItemReference = true
			}
			walkTypes(v, func(typ string) {
				switch typ {
				case "item_reference":
					r.ItemReference = true
				case "input_file":
					r.Files++
				}
			})
		}
	}
	return r
}

// itemNeeds is the content an input item of each type must carry to stand without the store.
// An item without a type is read as a message.
var itemNeeds = map[string][]string{
	"message":                 {"role", "content"},
	"reasoning":               {"encrypted_content"},
	"function_call":           {"call_id", "name", "arguments"},
	"function_call_output":    {"call_id", "output"},
	"custom_tool_call":        {"call_id", "name", "input"},
	"custom_tool_call_output": {"call_id", "output"},
	"computer_call_output":    {"call_id", "output"},
}

// storedItem reports whether an input item with an id may be read from the provider's store
// rather than from the request. It fails closed: an item of a known type must carry the content
// that type needs, any other item must carry something besides its id and type, and a null or
// empty value counts as missing. A key given twice in different cases, or a type that is not a
// string, also counts as stored, since a server may read either copy.
func storedItem(item any) bool {
	m, ok := item.(map[string]any)
	if !ok {
		return false
	}
	id, odd := false, false
	var types []string
	seen := map[string]bool{}
	has := map[string]bool{}
	for k, v := range m {
		key := strings.ToLower(k)
		odd = odd || seen[key]
		seen[key] = true
		switch {
		case key == "id":
			id = id || present(v)
		case key == "type":
			s, ok := v.(string)
			odd = odd || !ok
			types = append(types, strings.ToLower(s))
		case present(v):
			has[key] = true
		}
	}
	if !id {
		return false
	}
	if odd || len(has) == 0 {
		return true
	}
	if len(types) == 0 {
		types = []string{"message"}
	}
	for _, typ := range types {
		for _, need := range itemNeeds[typ] {
			if !has[need] {
				return true
			}
		}
	}
	return false
}

// present reports whether a JSON value carries anything: not null, an empty string, an empty
// array or an empty object.
func present(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// hasField reports whether any key that matches name without regard to case has a value other
// than null.
func hasField(m map[string]json.RawMessage, name string) bool {
	for k, raw := range m {
		if strings.EqualFold(k, name) && !isNull(raw) {
			return true
		}
	}
	return false
}

// walkTypes calls seen once for each object in v, with every distinct value of its type keys.
// A part may carry type in two cases, and a server may read either.
func walkTypes(v any, seen func(typ string)) {
	switch x := v.(type) {
	case map[string]any:
		found := map[string]bool{}
		for k, val := range x {
			if s, ok := val.(string); ok && strings.EqualFold(k, "type") && !found[s] {
				found[s] = true
				seen(s)
			}
			walkTypes(val, seen)
		}
	case []any:
		for _, val := range x {
			walkTypes(val, seen)
		}
	}
}
