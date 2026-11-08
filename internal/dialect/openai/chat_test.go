package openai

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/schema"
)

func TestDecodeChatRequest(t *testing.T) {
	req, err := DecodeChatRequest(strings.NewReader(`{"model":"gpt-x","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}],"max_tokens":5,"temperature":0.5,"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "gpt-x" || len(req.Messages) != 2 || !reflect.DeepEqual(req.Messages[1], schema.Message{Role: "user", Content: "hi"}) {
		t.Fatalf("req = %+v", req)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 5 || req.Temperature == nil || *req.Temperature != 0.5 || !req.Stream {
		t.Fatalf("options = %+v", req)
	}
}

func TestDecodeChatRequestErrors(t *testing.T) {
	tests := []struct {
		name, body, param, code string
	}{
		{name: "bad json", body: `{"model":`, code: "invalid_json"},
		{name: "missing model", body: `{"messages":[{"role":"user","content":"x"}]}`, param: "model", code: "missing_field"},
		{name: "empty messages", body: `{"model":"m","messages":[]}`, param: "messages", code: "missing_field"},
		{name: "content parts", body: `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, param: "messages", code: "unsupported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeChatRequest(strings.NewReader(tt.body))
			var re *RequestError
			if !errors.As(err, &re) {
				t.Fatalf("err = %v, want *RequestError", err)
			}
			if re.Param != tt.param || re.Code != tt.code {
				t.Fatalf("param %q code %q, want %q %q", re.Param, re.Code, tt.param, tt.code)
			}
		})
	}
}

func TestDecodeChatRequestWrongTypes(t *testing.T) {
	tests := []struct {
		name, body, param, message string
	}{
		{"model", `{"model":5,"messages":[{"role":"user","content":"x"}]}`, "model", "model must be a string"},
		{"messages", `{"model":"m","messages":"hi"}`, "messages", "messages must be a list"},
		{"message item", `{"model":"m","messages":[1]}`, "messages", "messages must be a list of objects"},
		{"role", `{"model":"m","messages":[{"role":1,"content":"x"}]}`, "messages.role", "messages.role must be a string"},
		{"role of a later message", `{"model":"m","messages":[{"role":"user","content":"x"},{"role":1}]}`, "messages.role", "messages.role must be a string"},
		{"max_tokens string", `{"model":"m","messages":[{"role":"user","content":"x"}],"max_tokens":"10"}`, "max_tokens", "max_tokens must be an integer"},
		{"max_tokens fraction", `{"model":"m","messages":[{"role":"user","content":"x"}],"max_tokens":1.5}`, "max_tokens", "max_tokens must be an integer"},
		{"temperature", `{"model":"m","messages":[{"role":"user","content":"x"}],"temperature":"hot"}`, "temperature", "temperature must be a number"},
		{"stream", `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":"yes"}`, "stream", "stream must be true or false"},
		{"body is a list", `[]`, "", "the request body must be a JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeChatRequest(strings.NewReader(tt.body))
			var re *RequestError
			if !errors.As(err, &re) {
				t.Fatalf("err = %v, want *RequestError", err)
			}
			if re.Param != tt.param || re.Code != "invalid_json" || re.Message != tt.message {
				t.Fatalf("param %q code %q message %q, want %q invalid_json %q", re.Param, re.Code, re.Message, tt.param, tt.message)
			}
			if strings.Contains(re.Message, "Go") || strings.Contains(re.Message, "wire") {
				t.Fatalf("message shows internals: %q", re.Message)
			}
		})
	}
}

func TestEncodeChatResponse(t *testing.T) {
	b, err := EncodeChatResponse(&schema.ChatResponse{
		ID:           "id-1",
		Model:        "m",
		Message:      schema.Message{Role: "assistant", Content: "echo: hi"},
		FinishReason: "stop",
		Usage:        schema.Usage{InputTokens: 3, OutputTokens: 4},
	}, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
			Total      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "id-1" || got.Object != "chat.completion" || got.Created != 1700000000 || got.Model != "m" {
		t.Fatalf("header fields = %+v", got)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "echo: hi" || got.Choices[0].Message.Role != "assistant" || got.Choices[0].FinishReason != "stop" {
		t.Fatalf("choices = %+v", got.Choices)
	}
	if got.Usage.Prompt != 3 || got.Usage.Completion != 4 || got.Usage.Total != 7 {
		t.Fatalf("usage = %+v", got.Usage)
	}
}
