package anthropic_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/dialect/anthropic"
	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/sse"
)

// benchMessages is the Anthropic form of the chat call the OpenAI benchmarks use.
var benchMessages = []byte(`{"model":"claude-sonnet-4-5","stream":true,"max_tokens":512,"temperature":0.2,` +
	`"system":"You are a careful assistant for a team that runs a gateway in front of several language model providers. Answer in short plain sentences, name the file or setting you mean, and say when you are not sure.",` +
	`"messages":[` +
	`{"role":"user","content":"Our gateway sends chat calls to two providers and falls back from the first to the second when the first answers 429 or 5xx. What should we look at first?"},` +
	`{"role":"assistant","content":"Start with the fallback rate per route, then the p99 latency each provider adds, then the store write time."},` +
	`{"role":"user","content":"Usage is written to SQLite after each call and each team has a monthly budget. Over the last hour the p99 latency the gateway adds went from 2 ms to 9 ms while traffic stayed flat at about 400 calls a second. How would you tell a slow store from a slow upstream?"}],` +
	`"tools":[{"name":"query_metrics","description":"Read a metric over a time range","input_schema":{"type":"object","properties":{"name":{"type":"string"},"since":{"type":"string"}},"required":["name"]}}]}`)

var benchChat = []byte(`{"model":"gpt-4o-mini","stream":true,"max_tokens":512,"temperature":0.2,` +
	`"messages":[` +
	`{"role":"system","content":"You are a careful assistant for a team that runs a gateway in front of several language model providers. Answer in short plain sentences, name the file or setting you mean, and say when you are not sure."},` +
	`{"role":"user","content":"Our gateway sends chat calls to two providers and falls back from the first to the second when the first answers 429 or 5xx. What should we look at first?"},` +
	`{"role":"assistant","content":"Start with the fallback rate per route, then the p99 latency each provider adds, then the store write time."},` +
	`{"role":"user","content":"Usage is written to SQLite after each call and each team has a monthly budget. Over the last hour the p99 latency the gateway adds went from 2 ms to 9 ms while traffic stayed flat at about 400 calls a second. How would you tell a slow store from a slow upstream?"}],` +
	`"tools":[{"type":"function","function":{"name":"query_metrics","description":"Read a metric over a time range","parameters":{"type":"object","properties":{"name":{"type":"string"},"since":{"type":"string"}},"required":["name"]}}}]}`)

var benchText = strings.Repeat("Measure the store write time apart from the upstream time. ", 17)

// benchEvents is benchText as an Anthropic stream in 32 deltas.
func benchEvents() []sse.Event {
	ev := func(name, data string) sse.Event { return sse.Event{Name: name, Data: []byte(data)} }
	out := []sse.Event{
		ev("message_start", `{"type":"message_start","message":{"id":"msg_bench","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":420,"output_tokens":1}}}`),
		ev("ping", `{"type":"ping"}`),
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
	}
	step := len(benchText) / 32
	for i := 0; i < len(benchText); i += step {
		out = append(out, ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, benchText[i:min(i+step, len(benchText))])))
	}
	return append(out,
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":256}}`),
		ev("message_stop", `{"type":"message_stop"}`),
	)
}

func BenchmarkDecodeRequest(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(benchMessages)))
	for b.Loop() {
		if _, err := anthropic.DecodeRequest(benchMessages); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTranslateRequest is the body work of a call that crosses dialects, both ways.
func BenchmarkTranslateRequest(b *testing.B) {
	b.Run("anthropic to openai", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			req, err := anthropic.DecodeRequest(benchMessages)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := openai.EncodeChatRequest(req); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("openai to anthropic", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			req, err := openai.DecodeChatRequest(bytes.NewReader(benchChat))
			if err != nil {
				b.Fatal(err)
			}
			if _, err := anthropic.EncodeRequest(req); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkTranslateStream turns one whole Anthropic stream into canonical events and writes
// them as OpenAI chunks, and the reverse, as a translated stream relay does.
func BenchmarkTranslateStream(b *testing.B) {
	events := benchEvents()
	b.Run("anthropic to openai", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var d anthropic.StreamDecoder
			for _, ev := range events {
				out, err := d.Events(ev)
				if err != nil {
					b.Fatal(err)
				}
				for _, e := range out {
					if _, err := openai.EncodeChunk("chatcmpl-bench", "claude-sonnet-4-5", time.Unix(1700000000, 0), e); err != nil {
						b.Fatal(err)
					}
				}
			}
		}
	})
	var d anthropic.StreamDecoder
	var canonical []schema.Event
	for _, ev := range events {
		out, err := d.Events(ev)
		if err != nil {
			b.Fatal(err)
		}
		canonical = append(canonical, out...)
	}
	b.Run("openai to anthropic", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			e := anthropic.NewStreamEncoder("msg_bench", "gpt-4o-mini")
			for _, ev := range canonical {
				e.Frames(ev)
			}
			e.End()
		}
	})
}
