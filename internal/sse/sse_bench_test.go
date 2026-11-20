package sse_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/sse"
)

// BenchmarkReader reads one whole streamed answer: 32 chunks of about 32 bytes of text each,
// as an OpenAI-compatible provider sends them.
func BenchmarkReader(b *testing.B) {
	var stream bytes.Buffer
	text := strings.Repeat("Measure the store write time apart from the upstream time. ", 17)
	step := len(text) / 32
	for i := 0; i < len(text); i += step {
		fmt.Fprintf(&stream, "data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-4o-mini\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", text[i:min(i+step, len(text))])
	}
	stream.WriteString("data: [DONE]\n\n")
	body := stream.Bytes()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		r := sse.NewReader(bytes.NewReader(body))
		for {
			if _, err := r.Next(); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkFrame(b *testing.B) {
	data := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Measure the store write time apart."}}`)
	b.ReportAllocs()
	for b.Loop() {
		sse.Frame("content_block_delta", data)
	}
}
