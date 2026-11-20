package openai

import (
	"encoding/json"
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
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return ResponsesPeek{}, invalidJSON("the request body is not a JSON object")
	}
	var p ResponsesPeek
	if err := field(m, "model", &p.Model); err != nil {
		return ResponsesPeek{}, err
	}
	if err := field(m, "stream", &p.Stream); err != nil {
		return ResponsesPeek{}, err
	}
	if err := field(m, "background", &p.Background); err != nil {
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

// UnknownResponse reports whether an error body says the provider does not hold the response
// that previous_response_id names, or does not take that field. The message is the provider's.
func UnknownResponse(body []byte) (message string, ok bool) {
	var w wireError
	if json.Unmarshal(body, &w) != nil {
		return "", false
	}
	code, param := "", ""
	if w.Error.Code != nil {
		code = *w.Error.Code
	}
	if w.Error.Param != nil {
		param = *w.Error.Param
	}
	if code == "previous_response_not_found" || param == "previous_response_id" {
		return w.Error.Message, true
	}
	return "", false
}
