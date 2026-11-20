package openai

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/sse"
)

func TestPeekResponses(t *testing.T) {
	tests := []struct {
		name, body string
		want       ResponsesPeek
		wantCode   string
		wantParam  string
	}{
		{name: "model only", body: `{"model":"gpt-5","input":"hi"}`, want: ResponsesPeek{Model: "gpt-5"}},
		{name: "stream", body: `{"model":"gpt-5","input":[],"stream":true}`, want: ResponsesPeek{Model: "gpt-5", Stream: true}},
		{name: "background", body: `{"model":"gpt-5","background":true}`, want: ResponsesPeek{Model: "gpt-5", Background: true}},
		{name: "nulls are unset", body: `{"model":"gpt-5","stream":null,"background":null}`, want: ResponsesPeek{Model: "gpt-5"}},
		// Only the exact key counts, as on the provider.
		{name: "other case is another field", body: `{"model":"gpt-5","Stream":true}`, want: ResponsesPeek{Model: "gpt-5"}},
		{name: "not an object", body: `[1]`, wantCode: "invalid_json"},
		{name: "cut short", body: `{"model":`, wantCode: "invalid_json"},
		{name: "no model", body: `{"input":"hi"}`, wantCode: "missing_field", wantParam: "model"},
		{name: "model of the wrong type", body: `{"model":5}`, wantCode: "invalid_json", wantParam: "model"},
		{name: "stream of the wrong type", body: `{"model":"m","stream":"yes"}`, wantCode: "invalid_json", wantParam: "stream"},
		{name: "background of the wrong type", body: `{"model":"m","background":1}`, wantCode: "invalid_json", wantParam: "background"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PeekResponses([]byte(tt.body))
			if tt.wantCode == "" {
				if err != nil || got != tt.want {
					t.Fatalf("got %+v, %v; want %+v", got, err, tt.want)
				}
				return
			}
			var bad *RequestError
			if !errors.As(err, &bad) || bad.Code != tt.wantCode || bad.Param != tt.wantParam {
				t.Fatalf("err = %v, want code %s param %s", err, tt.wantCode, tt.wantParam)
			}
		})
	}
}

func TestParseResponseUsage(t *testing.T) {
	tests := []struct {
		name, body string
		want       schema.Usage
		wantOK     bool
	}{
		{name: "plain", body: `{"usage":{"input_tokens":36,"output_tokens":87,"total_tokens":123}}`,
			want: schema.Usage{InputTokens: 36, OutputTokens: 87}, wantOK: true},
		{name: "zero details", body: `{"usage":{"input_tokens":36,"input_tokens_details":{"cached_tokens":0},"output_tokens":87,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":123}}`,
			want: schema.Usage{InputTokens: 36, OutputTokens: 87}, wantOK: true},
		{name: "cached input is split out", body: `{"usage":{"input_tokens":1200,"input_tokens_details":{"cached_tokens":1024},"output_tokens":10,"total_tokens":1210}}`,
			want: schema.Usage{InputTokens: 176, CacheReadTokens: 1024, OutputTokens: 10}, wantOK: true},
		{name: "reasoning is split out of output", body: `{"usage":{"input_tokens":50,"output_tokens":600,"output_tokens_details":{"reasoning_tokens":448},"total_tokens":650}}`,
			want: schema.Usage{InputTokens: 50, OutputTokens: 152, ReasoningTokens: 448}, wantOK: true},
		{name: "reasoning reported on top of output stays apart", body: `{"usage":{"input_tokens":50,"output_tokens":100,"output_tokens_details":{"reasoning_tokens":300},"total_tokens":450}}`,
			want: schema.Usage{InputTokens: 50, OutputTokens: 100, ReasoningTokens: 300}, wantOK: true},
		{name: "null usage", body: `{"status":"failed","usage":null}`},
		{name: "no usage", body: `{"status":"queued"}`},
		{name: "not JSON", body: `oops`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseResponseUsage([]byte(tt.body))
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("got %+v, %v; want %+v, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestResponseText(t *testing.T) {
	body := `{"output":[
		{"type":"reasoning","summary":[{"type":"summary_text","text":"think "}]},
		{"type":"message","content":[{"type":"output_text","text":"hello "},{"type":"refusal","refusal":"no "}]},
		{"type":"function_call","name":"f","arguments":"{\"a\":1}"}]}`
	if got := ResponseText([]byte(body)); got != `think hello no {"a":1}` {
		t.Fatalf("got %q", got)
	}
	if got := ResponseText([]byte("oops")); got != "oops" {
		t.Fatalf("a body that is not JSON should count whole, got %q", got)
	}
}

func TestResponseStream(t *testing.T) {
	completed := `{"type":"response.completed","sequence_number":9,"response":{"status":"completed","usage":{"input_tokens":1200,"input_tokens_details":{"cached_tokens":1024},"output_tokens":600,"output_tokens_details":{"reasoning_tokens":448},"total_tokens":1800}}}`
	tests := []struct {
		name       string
		events     []string
		wantUsage  schema.Usage
		wantOK     bool
		wantDone   bool
		wantFailed bool
		wantText   string
		wantSeq    string
	}{
		{name: "completed", wantOK: true, wantDone: true, wantText: `Hi there{"a"`, wantSeq: `"sequence_number":10`,
			wantUsage: schema.Usage{InputTokens: 176, CacheReadTokens: 1024, OutputTokens: 152, ReasoningTokens: 448},
			events: []string{
				`{"type":"response.created","sequence_number":0,"response":{"status":"in_progress","usage":null}}`,
				`{"type":"response.output_text.delta","sequence_number":4,"delta":"Hi"}`,
				`{"type":"response.reasoning_summary_text.delta","sequence_number":5,"delta":" there"}`,
				`{"type":"response.function_call_arguments.delta","sequence_number":6,"delta":"{\"a\""}`,
				`{"type":"response.output_text.done","sequence_number":7,"text":"not counted twice"}`,
				completed,
			}},
		{name: "incomplete carries usage", wantOK: true, wantDone: true, wantSeq: `"sequence_number":4`,
			wantUsage: schema.Usage{InputTokens: 5, OutputTokens: 16},
			events:    []string{`{"type":"response.incomplete","sequence_number":3,"response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":5,"output_tokens":16,"total_tokens":21}}}`}},
		{name: "failed without usage", wantDone: true, wantSeq: `"sequence_number":3`,
			events: []string{`{"type":"response.failed","sequence_number":2,"response":{"status":"failed","error":{"code":"server_error","message":"x"},"usage":null}}`}},
		{name: "error event", wantFailed: true, wantText: "par", wantSeq: `"sequence_number":3`,
			events: []string{
				`{"type":"response.output_text.delta","sequence_number":1,"delta":"par"}`,
				`{"type":"error","sequence_number":2,"code":"server_error","message":"x","param":null}`,
			}},
		{name: "cut before the end", wantText: "par", wantSeq: `"sequence_number":2`,
			events: []string{`{"type":"response.output_text.delta","sequence_number":1,"delta":"par"}`}},
		{name: "what cannot be read is passed over", wantText: "ok", wantSeq: `"sequence_number":1`,
			events: []string{`[DONE]`, `{"type":"response.custom.delta","delta":{"not":"a string"}}`, `{"type":"response.output_text.delta","delta":"ok"}`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s ResponseStream
			for _, ev := range tt.events {
				s.Add([]byte(ev))
			}
			u, ok := s.Usage()
			if u != tt.wantUsage || ok != tt.wantOK || s.Done() != tt.wantDone || s.Failed() != tt.wantFailed || s.Text() != tt.wantText {
				t.Fatalf("usage %+v %v, done %v, failed %v, text %q", u, ok, s.Done(), s.Failed(), s.Text())
			}
			frame := string(s.ErrorFrame("upstream_error", "cut"))
			want := `data: {"type":"error","code":"upstream_error","message":"cut","param":null,` + tt.wantSeq + "}\n\n"
			if !strings.HasPrefix(frame, "event: error\n") || !strings.HasSuffix(frame, want) {
				t.Fatalf("error frame %q", frame)
			}
		})
	}
}

func FuzzResponseStream(f *testing.F) {
	f.Add([]byte(`{"type":"response.completed","sequence_number":9,"response":{"usage":{"input_tokens":1200,"input_tokens_details":{"cached_tokens":1024},"output_tokens":600,"output_tokens_details":{"reasoning_tokens":448},"total_tokens":1800}}}`))
	f.Add([]byte(`{"type":"response.output_text.delta","sequence_number":4,"delta":"Hi"}`))
	f.Add([]byte(`{"type":"response.failed","response":{"usage":null}}`))
	f.Add([]byte(`{"type":"error","code":"server_error","message":"x"}`))
	f.Add([]byte(`{"model":"m","previous_response_id":"resp_1","input":[{"type":"item_reference","id":"msg_1"},{"role":"user","content":[{"type":"input_file","file_id":"file-1"}]}]}`))
	f.Add([]byte(`[DONE]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var s ResponseStream
		s.Add(data)
		if u, ok := s.Usage(); ok != s.Done() && (ok || u != (schema.Usage{})) {
			t.Fatalf("usage %+v %v, done %v", u, ok, s.Done())
		}
		// The frame that ends a cut stream must itself be one readable event.
		ev, err := sse.NewReader(bytes.NewReader(s.ErrorFrame("upstream_error", "cut"))).Next()
		if err != nil || !IsResponseStreamError(ev) {
			t.Fatalf("error frame %q: %v", ev.Data, err)
		}
		ParseResponseUsage(data)
		ResponseText(data)
		// A body that names stored state must never be read as one that does not.
		r := ReadResponsesRefs(data)
		if (r.Stateful() == "") != (!r.PreviousResponse && !r.Conversation && !r.ItemReference) || r.Files < 0 {
			t.Fatalf("refs %+v", r)
		}
	})
}

func TestIsResponseStreamError(t *testing.T) {
	tests := []struct {
		name string
		ev   sse.Event
		want bool
	}{
		{"named error", sse.Event{Name: "error", Data: []byte(`{"message":"x"}`)}, true},
		{"typed error without a name", sse.Event{Data: []byte(`{"type":"error","message":"x"}`)}, true},
		{"created", sse.Event{Name: "response.created", Data: []byte(`{"type":"response.created"}`)}, false},
		{"failed is the end of a response, not of the stream", sse.Event{Name: "response.failed", Data: []byte(`{"type":"response.failed"}`)}, false},
		{"not JSON", sse.Event{Data: []byte(`[DONE]`)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsResponseStreamError(tt.ev); got != tt.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestReadResponsesRefs(t *testing.T) {
	message := func(parts string) string {
		return `{"model":"m","input":[{"role":"user","content":[` + parts + `]}]}`
	}
	tests := []struct {
		name, body string
		want       ResponsesRefs
		stateful   string
	}{
		{name: "text only", body: `{"model":"m","input":"hi","store":true}`},
		{name: "items without references", body: message(`{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"https://example.com/a.png"}`)},
		{name: "previous response", body: `{"model":"m","input":"and then?","previous_response_id":"resp_1"}`,
			want: ResponsesRefs{PreviousResponse: true}, stateful: "previous_response_id"},
		{name: "previous response in another case", body: `{"model":"m","Previous_Response_Id":"resp_1"}`,
			want: ResponsesRefs{PreviousResponse: true}, stateful: "previous_response_id"},
		{name: "null is no reference", body: `{"model":"m","previous_response_id":null,"conversation":null,"prompt":null}`},
		{name: "conversation by id", body: `{"model":"m","conversation":"conv_1"}`,
			want: ResponsesRefs{Conversation: true}, stateful: "conversation"},
		{name: "conversation as an object", body: `{"model":"m","conversation":{"id":"conv_1"}}`,
			want: ResponsesRefs{Conversation: true}, stateful: "conversation"},
		{name: "item reference", body: `{"model":"m","input":[{"type":"item_reference","id":"msg_1"},{"role":"user","content":"x"}]}`,
			want: ResponsesRefs{ItemReference: true}, stateful: "input"},
		{name: "item reference in another key case", body: `{"model":"m","Input":[{"TYPE":"item_reference","id":"msg_1"}]}`,
			want: ResponsesRefs{ItemReference: true}, stateful: "input"},
		{name: "an item by id alone", body: `{"model":"m","input":[{"id":"msg_1"}]}`,
			want: ResponsesRefs{ItemReference: true}, stateful: "input"},
		{name: "an item by id and type alone", body: `{"model":"m","input":[{"type":"message","ID":"msg_1"}]}`,
			want: ResponsesRefs{ItemReference: true}, stateful: "input"},
		{name: "reasoning without its encrypted content", body: `{"model":"m","input":[{"type":"reasoning","id":"rs_1","summary":[]}]}`,
			want: ResponsesRefs{ItemReference: true}, stateful: "input"},
		{name: "reasoning with null encrypted content", body: `{"model":"m","input":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":null}]}`,
			want: ResponsesRefs{ItemReference: true}, stateful: "input"},
		// What stateless clients send back: items carry their content, so their ids name nothing.
		{name: "items with their content", body: `{"model":"m","input":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAAAB"},{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hi"}]},{"type":"function_call","id":"fc_1","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"ok"}]}`},
		{name: "an id inside a part is not an item", body: message(`{"type":"input_text","text":"x","id":"p1"}`)},
		{name: "stored prompt", body: `{"model":"m","prompt":{"id":"pmpt_1","version":"2"}}`, want: ResponsesRefs{Prompt: true}},
		{name: "files by id, URL and inline", body: message(`{"type":"input_file","file_id":"file-1"},{"type":"input_file","file_url":"https://example.com/a.pdf"},{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,QUJD"}`),
			want: ResponsesRefs{Files: 3}},
		{name: "a file in a tool's output", body: `{"model":"m","input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_file","file_id":"file-1"}]}]}`,
			want: ResponsesRefs{Files: 1}},
		// A server keeps the last of two keys that differ by case, so either may be the one it reads.
		{name: "a part typed twice", body: message(`{"type":"input_text","TYPE":"input_file","text":"x","file_id":"file-1"}`), want: ResponsesRefs{Files: 1}},
		{name: "every reference", body: `{"model":"m","previous_response_id":"r","conversation":"c","prompt":{"id":"p"},"input":[{"type":"item_reference","id":"i"},{"type":"input_file","file_id":"f"}]}`,
			want: ResponsesRefs{PreviousResponse: true, Conversation: true, ItemReference: true, Prompt: true, Files: 1}, stateful: "previous_response_id"},
		{name: "not JSON", body: `{"model":`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReadResponsesRefs([]byte(tt.body))
			if got != tt.want || got.Stateful() != tt.stateful {
				t.Fatalf("got %+v (stateful %q), want %+v (%q)", got, got.Stateful(), tt.want, tt.stateful)
			}
		})
	}
}
