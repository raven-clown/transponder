package sse

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func FuzzReader(f *testing.F) {
	f.Add([]byte("data: hello\n\n"))
	f.Add([]byte("event: message_start\ndata: {\"a\":1}\n\n"))
	f.Add([]byte(": comment\r\ndata: x\r\ndata: y\r\n\r\n"))
	f.Add([]byte("data: cut"))
	f.Fuzz(func(t *testing.T, in []byte) {
		r := NewReader(bytes.NewReader(in))
		for i := 0; ; i++ {
			if i > len(in)+1 {
				t.Fatalf("no end after %d events", i)
			}
			if _, err := r.Next(); err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
		}
	})
}

func FuzzFrameRoundTrip(f *testing.F) {
	f.Add("", []byte("hello"))
	f.Add("content_block_delta", []byte("{\"type\":\"text\"}"))
	f.Add("ping", []byte("two\nlines"))
	f.Add("", []byte(""))
	f.Fuzz(func(t *testing.T, name string, data []byte) {
		if strings.ContainsAny(name, "\r\n") || bytes.ContainsRune(data, '\r') {
			t.Skip("a carriage return or a newline in the name is not valid in a frame")
		}
		ev, err := NewReader(bytes.NewReader(Frame(name, data))).Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if ev.Name != name || !bytes.Equal(ev.Data, data) {
			t.Fatalf("got %q %q, want %q %q", ev.Name, ev.Data, name, data)
		}
	})
}
