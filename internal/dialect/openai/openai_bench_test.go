package openai_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/schema"
)

// benchRequest is a chat call of the size a team sends most: a system prompt, a short
// conversation and one tool, about 2 KB.
var benchRequest = []byte(`{"model":"gpt-4o-mini","stream":true,"max_tokens":512,"temperature":0.2,` +
	`"messages":[` +
	`{"role":"system","content":"You are a careful assistant for a team that runs a gateway in front of several language model providers. Answer in short plain sentences, name the file or setting you mean, and say when you are not sure."},` +
	`{"role":"user","content":"Our gateway sends chat calls to two providers and falls back from the first to the second when the first answers 429 or 5xx. What should we look at first?"},` +
	`{"role":"assistant","content":"Start with the fallback rate per route, then the p99 latency each provider adds, then the store write time."},` +
	`{"role":"user","content":"Usage is written to SQLite after each call and each team has a monthly budget. Over the last hour the p99 latency the gateway adds went from 2 ms to 9 ms while traffic stayed flat at about 400 calls a second. How would you tell a slow store from a slow upstream?"}],` +
	`"tools":[{"type":"function","function":{"name":"query_metrics","description":"Read a metric over a time range","parameters":{"type":"object","properties":{"name":{"type":"string"},"since":{"type":"string"}},"required":["name"]}}}]}`)

// benchAnswer is about 1 KB of answer text.
var benchAnswer = strings.Repeat("Measure the store write time apart from the upstream time. ", 17)

func benchResponse() []byte {
	return []byte(`{"id":"chatcmpl-bench","object":"chat.completion","created":1700000000,"model":"gpt-4o-mini",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"` + benchAnswer + `"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":420,"completion_tokens":256,"total_tokens":676}}`)
}

// benchChunks is the stream of benchAnswer in 32 chunks, then the finish and usage chunks.
func benchChunks() [][]byte {
	const head = `{"id":"chatcmpl-bench","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[`
	choice := func(delta, finish string) []byte {
		return []byte(head + `{"index":0,"delta":` + delta + `,"finish_reason":` + finish + `}]}`)
	}
	out := [][]byte{choice(`{"role":"assistant","content":""}`, "null")}
	step := len(benchAnswer) / 32
	for i := 0; i < len(benchAnswer); i += step {
		out = append(out, choice(fmt.Sprintf(`{"content":%q}`, benchAnswer[i:min(i+step, len(benchAnswer))]), "null"))
	}
	return append(out, choice(`{}`, `"stop"`), []byte(head+`],"usage":{"prompt_tokens":420,"completion_tokens":256,"total_tokens":676}}`))
}

func BenchmarkDecodeChatRequest(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(benchRequest)))
	for b.Loop() {
		if _, err := openai.DecodeChatRequest(bytes.NewReader(benchRequest)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeChatRequest(b *testing.B) {
	req, err := openai.DecodeChatRequest(bytes.NewReader(benchRequest))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := openai.EncodeChatRequest(req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPeekAndRewrite is what a call to an OpenAI target does to the body on the native
// path: read the model and stream flag, then put the target's model in.
func BenchmarkPeekAndRewrite(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(benchRequest)))
	for b.Loop() {
		if _, err := openai.PeekRequest(benchRequest); err != nil {
			b.Fatal(err)
		}
		if _, err := openai.RewriteRequest(benchRequest, "gpt-4o", true); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseUsage(b *testing.B) {
	body := benchResponse()
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := openai.ParseUsage(body); !ok {
			b.Fatal("no usage")
		}
	}
}

func BenchmarkDecodeChatResponse(b *testing.B) {
	body := benchResponse()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		if _, err := openai.DecodeChatResponse(body); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeChatResponse(b *testing.B) {
	resp, err := openai.DecodeChatResponse(benchResponse())
	if err != nil {
		b.Fatal(err)
	}
	created := time.Unix(1700000000, 0)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := openai.EncodeChatResponse(resp, created); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStreamDecode turns one whole streamed answer into canonical events.
func BenchmarkStreamDecode(b *testing.B) {
	chunks := benchChunks()
	b.ReportAllocs()
	for b.Loop() {
		var d openai.ChunkDecoder
		for _, c := range chunks {
			if _, err := d.Events(c); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkStreamEncode writes one whole answer as OpenAI chunks, as a translated stream does.
func BenchmarkStreamEncode(b *testing.B) {
	var d openai.ChunkDecoder
	var events []schema.Event
	for _, c := range benchChunks() {
		evs, err := d.Events(c)
		if err != nil {
			b.Fatal(err)
		}
		events = append(events, evs...)
	}
	created := time.Unix(1700000000, 0)
	b.ReportAllocs()
	for b.Loop() {
		for _, ev := range events {
			if _, err := openai.EncodeChunk("chatcmpl-bench", "gpt-4o-mini", created, ev); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkChunkUsage is the check a native stream relay runs on every chunk for usage.
func BenchmarkChunkUsage(b *testing.B) {
	chunks := benchChunks()
	b.ReportAllocs()
	for b.Loop() {
		for _, c := range chunks {
			openai.ChunkUsage(c)
		}
	}
}
