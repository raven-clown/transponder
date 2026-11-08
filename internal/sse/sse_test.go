package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func readAll(t *testing.T, in string) ([]Event, error) {
	t.Helper()
	r := NewReader(strings.NewReader(in))
	var out []Event
	for {
		ev, err := r.Next()
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
}

func TestReader(t *testing.T) {
	in := ": ping\r\n\r\nevent: message_start\r\ndata: {\"a\":1}\r\n\r\ndata:{\"b\":2}\n\nevent: x\ndata: line one\ndata: line two\n\nid: 7\nretry: 10\n\n"
	got, err := readAll(t, in)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	want := []Event{
		{Name: "message_start", Data: []byte(`{"a":1}`)},
		{Data: []byte(`{"b":2}`)},
		{Name: "x", Data: []byte("line one\nline two")},
	}
	if len(got) != len(want) {
		t.Fatalf("events %+v", got)
	}
	for i := range want {
		if got[i].Name != want[i].Name || string(got[i].Data) != string(want[i].Data) {
			t.Fatalf("event %d = %q %q, want %q %q", i, got[i].Name, got[i].Data, want[i].Name, want[i].Data)
		}
	}
}

func TestReaderEnds(t *testing.T) {
	if _, err := readAll(t, "data: {\"a\":1}\n\ndata: {\"b\""); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("cut inside an event = %v", err)
	}
	if _, err := readAll(t, "data: {\"a\":1}\n"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("event without its blank line = %v", err)
	}
	r := NewReader(strings.NewReader("data: x\n\ndata: [DONE]"))
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	ev, err := r.Next()
	if !errors.Is(err, io.ErrUnexpectedEOF) || string(ev.Data) != "[DONE]" {
		t.Fatalf("cut event = %q, %v; want the partial data with io.ErrUnexpectedEOF", ev.Data, err)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestReaderEventTooLarge(t *testing.T) {
	old := MaxEventBytes
	MaxEventBytes = 32
	defer func() { MaxEventBytes = old }()
	for name, in := range map[string]string{
		"one long line":    "data: " + strings.Repeat("x", 100),
		"many short lines": strings.Repeat("data: xxxxxxxx\n", 10) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			src := &countingReader{r: strings.NewReader(in + strings.Repeat("y", 1<<20))}
			_, err := NewReader(src).Next()
			if !errors.Is(err, ErrEventTooLarge) {
				t.Fatalf("err = %v", err)
			}
			if src.n > 1<<16 {
				t.Fatalf("read %d bytes", src.n)
			}
		})
	}
}

func TestFrame(t *testing.T) {
	tests := []struct {
		name, event, data, want string
	}{
		{"data only", "", `{"a":1}`, "data: {\"a\":1}\n\n"},
		{"named", "message_stop", `{"type":"message_stop"}`, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
		{"multi-line", "", "one\ntwo", "data: one\ndata: two\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(Frame(tt.event, []byte(tt.data))); got != tt.want {
				t.Fatalf("Frame = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReaderEmptyData(t *testing.T) {
	got, err := readAll(t, "event: ping\ndata:\n\n")
	if !errors.Is(err, io.EOF) || len(got) != 1 || got[0].Name != "ping" || got[0].Data == nil || len(got[0].Data) != 0 {
		t.Fatalf("events %+v, %v", got, err)
	}
}
