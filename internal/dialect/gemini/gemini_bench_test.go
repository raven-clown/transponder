package gemini_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/dialect/gemini"
	"github.com/raven-clown/transponder/internal/dialect/openai"
)

var benchChat = []byte(`{"model":"gemini-2.5-flash","stream":true,"max_tokens":512,"temperature":0.2,` +
	`"messages":[` +
	`{"role":"system","content":"You are a careful assistant for a team that runs a gateway in front of several language model providers. Answer in short plain sentences, name the file or setting you mean, and say when you are not sure."},` +
	`{"role":"user","content":"Our gateway sends chat calls to two providers and falls back from the first to the second when the first answers 429 or 5xx. What should we look at first?"},` +
	`{"role":"assistant","content":"Start with the fallback rate per route, then the p99 latency each provider adds, then the store write time."},` +
	`{"role":"user","content":"Usage is written to SQLite after each call and each team has a monthly budget. Over the last hour the p99 latency the gateway adds went from 2 ms to 9 ms while traffic stayed flat at about 400 calls a second. How would you tell a slow store from a slow upstream?"}],` +
	`"tools":[{"type":"function","function":{"name":"query_metrics","description":"Read a metric over a time range","parameters":{"type":"object","properties":{"name":{"type":"string"},"since":{"type":"string"}},"required":["name"]}}}]}`)

// BenchmarkEncodeRequest writes an OpenAI chat call as a Gemini request.
func BenchmarkEncodeRequest(b *testing.B) {
	req, err := openai.DecodeChatRequest(bytes.NewReader(benchChat))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := gemini.EncodeRequest(req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStreamDecode reads one whole Gemini stream of 32 chunks into canonical events.
func BenchmarkStreamDecode(b *testing.B) {
	text := strings.Repeat("Measure the store write time apart from the upstream time. ", 17)
	step := len(text) / 32
	var chunks [][]byte
	for i := 0; i < len(text); i += step {
		chunks = append(chunks, fmt.Appendf(nil, `{"candidates":[{"content":{"role":"model","parts":[{"text":%q}]},"index":0}],"modelVersion":"gemini-2.5-flash"}`, text[i:min(i+step, len(text))]))
	}
	chunks = append(chunks, []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":420,"candidatesTokenCount":256,"totalTokenCount":676}}`))
	b.ReportAllocs()
	for b.Loop() {
		var d gemini.StreamDecoder
		for _, c := range chunks {
			if _, err := d.Events(c); err != nil {
				b.Fatal(err)
			}
		}
	}
}
