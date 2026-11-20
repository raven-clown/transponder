package server

import (
	"errors"
	"io"
	"slices"
	"testing"
)

func TestOpen(t *testing.T) {
	errBad := errors.New("bad")
	isOpening := func(s string) bool { return s == "open" }
	failure := func(s string) error {
		if s == "error" {
			return errBad
		}
		return nil
	}
	many := slices.Repeat([]string{"open"}, maxOpening+5)
	tests := map[string]struct {
		events  []string
		wantErr error
		want    []string
	}{
		"output first":           {[]string{"out", "open"}, nil, []string{"out", "open"}},
		"opening then output":    {[]string{"open", "open", "out"}, nil, []string{"open", "open", "out"}},
		"error in the opening":   {[]string{"open", "error", "out"}, errBad, nil},
		"ends in the opening":    {[]string{"open"}, io.EOF, nil},
		"error after the output": {[]string{"out", "error"}, nil, []string{"out", "error"}},
		"opening too long":       {many, nil, many},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			i := 0
			read := func() (string, error) {
				if i == len(tt.events) {
					return "", io.EOF
				}
				i++
				return tt.events[i-1], nil
			}
			next, err := open(read, isOpening, failure)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if name == "opening too long" && i != maxOpening {
				t.Fatalf("read %d events before the stream began, want at most %d", i, maxOpening)
			}
			var got []string
			for {
				ev, err := next()
				if err != nil {
					break
				}
				got = append(got, ev)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("events %q, want %q", got, tt.want)
			}
		})
	}
}
