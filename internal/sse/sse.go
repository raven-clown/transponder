// Package sse reads and writes server-sent event streams.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// ErrEventTooLarge means one line or event is over MaxEventBytes.
var ErrEventTooLarge = errors.New("sse: event is too large")

// MaxEventBytes caps one line and one event, so a broken upstream cannot grow memory without end.
var MaxEventBytes = 16 << 20

// Event is one dispatched event. Name is empty when the stream sent no event: line.
type Event struct {
	Name string
	Data []byte
}

// Reader reads events from a stream.
type Reader struct {
	r     *bufio.Reader
	begun bool
}

// NewReader reads events from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReader(r)}
}

// Next returns the next event. It returns io.EOF on a clean end, and the partial event with
// io.ErrUnexpectedEOF when the stream stops inside one.
func (s *Reader) Next() (Event, error) {
	var ev Event
	started := false
	for {
		line, err := s.readLine()
		eof := errors.Is(err, io.EOF)
		if err != nil && !eof {
			return Event{}, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if eof && len(line) == 0 {
			if started {
				return ev, io.ErrUnexpectedEOF
			}
			return Event{}, io.EOF
		}
		switch {
		case len(line) == 0:
			if ev.Data != nil {
				return ev, nil
			}
			ev, started = Event{}, false
		case line[0] == ':':
		default:
			field, value, _ := bytes.Cut(line, []byte(":"))
			value = bytes.TrimPrefix(value, []byte(" "))
			switch string(field) {
			case "event":
				ev.Name, started = string(value), true
			case "data":
				if len(ev.Data)+len(value)+1 > MaxEventBytes {
					return Event{}, ErrEventTooLarge
				}
				if ev.Data == nil {
					ev.Data = []byte{}
				} else {
					ev.Data = append(ev.Data, '\n')
				}
				ev.Data = append(ev.Data, value...)
				started = true
			}
		}
		if eof {
			if started {
				return ev, io.ErrUnexpectedEOF
			}
			return Event{}, io.EOF
		}
	}
}

func (s *Reader) readLine() ([]byte, error) {
	if !s.begun {
		// A stream may begin with a UTF-8 byte order mark, which is not part of its first line.
		s.begun = true
		if b, err := s.r.Peek(3); err == nil && bytes.Equal(b, []byte("\xEF\xBB\xBF")) {
			_, _ = s.r.Discard(3)
		}
	}
	// A line that fits the buffer is returned without a copy. It is only valid until the next
	// read, and Next copies what it keeps before then.
	chunk, err := s.r.ReadSlice('\n')
	if len(chunk) > MaxEventBytes {
		return nil, ErrEventTooLarge
	}
	if !errors.Is(err, bufio.ErrBufferFull) {
		return chunk, err
	}
	line := append([]byte(nil), chunk...)
	for {
		chunk, err := s.r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxEventBytes {
			return nil, ErrEventTooLarge
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

// Frame renders one event: an event: line when name is set and one data: line per line of data.
func Frame(name string, data []byte) []byte {
	size := len(data) + 1 + (bytes.Count(data, []byte{'\n'})+1)*len("data: \n")
	if name != "" {
		size += len("event: \n") + len(name)
	}
	out := make([]byte, 0, size)
	if name != "" {
		out = append(append(append(out, "event: "...), name...), '\n')
	}
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		out = append(append(append(out, "data: "...), line...), '\n')
	}
	return append(out, '\n')
}
