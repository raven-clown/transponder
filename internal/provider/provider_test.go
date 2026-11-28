package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/raven-clown/transponder/internal/schema"
)

func TestReadBody(t *testing.T) {
	big := strings.Repeat("x", 3*presizeLimit+5000)
	const noLimit = 1 << 40
	for _, tt := range []struct {
		name string
		body string
		size int64
	}{
		{"declared exactly", "hello", 5},
		{"unknown length", "hello", -1},
		{"empty", "", 0},
		{"declared too small", "hello world", 3},
		{"declared too large", "hello", 100},
		{"over the presize limit", big, int64(len(big))},
		{"longer than declared, past the presize limit", big, 10},
		{"declared past the presize limit, shorter", "hello", int64(len(big))},
		{"declared exactly the presize limit", big[:presizeLimit], presizeLimit},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, r := range []io.Reader{strings.NewReader(tt.body), iotest.OneByteReader(strings.NewReader(tt.body)), iotest.DataErrReader(strings.NewReader(tt.body))} {
				got, err := ReadBody(r, tt.size, noLimit)
				if err != nil || string(got) != tt.body {
					t.Fatalf("got %d bytes, err %v; want %d bytes", len(got), err, len(tt.body))
				}
			}
		})
	}
	t.Run("nothing sent holds little", func(t *testing.T) {
		got, err := ReadBody(strings.NewReader("hi"), 1<<30, noLimit)
		if err != nil || string(got) != "hi" || cap(got) > presizeLimit+1024 {
			t.Fatalf("got %q, cap %d, err %v", got, cap(got), err)
		}
	})
	t.Run("a sender that stalls holds a few times what it sent", func(t *testing.T) {
		// A caller declares 20 MiB, sends 1 MiB and stops.
		sent := strings.Repeat("x", 1<<20)
		got, err := ReadBody(strings.NewReader(sent), 20<<20, 20<<20)
		if err != nil || len(got) != len(sent) || cap(got) > 4*len(sent)+1024 {
			t.Fatalf("len %d, cap %d, err %v", len(got), cap(got), err)
		}
	})
	t.Run("a declared size past the limit is not trusted", func(t *testing.T) {
		// A provider declares 64 TiB, sends 2 MiB and ends; the reader stops at 4 MiB.
		const limit = 4 << 20
		sent := strings.Repeat("x", 2<<20)
		got, err := ReadBody(io.LimitReader(strings.NewReader(sent), limit), 1<<46, limit)
		// The allocator rounds a large buffer up a little; it must not grow towards 64 TiB.
		if err != nil || len(got) != len(sent) || cap(got) > limit+limit/64 {
			t.Fatalf("len %d, cap %d, err %v", len(got), cap(got), err)
		}
	})
	t.Run("a read error comes back", func(t *testing.T) {
		boom := errors.New("boom")
		if _, err := ReadBody(io.MultiReader(strings.NewReader("ab"), iotest.ErrReader(boom)), 10, noLimit); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	})
	t.Run("a wrapped EOF is an error, as in io.ReadAll", func(t *testing.T) {
		wrapped := fmt.Errorf("cut: %w", io.EOF)
		r := io.MultiReader(strings.NewReader("ab"), iotest.ErrReader(wrapped))
		if _, err := ReadBody(r, 10, noLimit); !errors.Is(err, wrapped) {
			t.Fatalf("err = %v, want the wrapped EOF", err)
		}
	})
}

func BenchmarkReadBody(b *testing.B) {
	for _, size := range []int{2 << 10, 50 << 10, 1 << 20, 8 << 20} {
		body := strings.Repeat("x", size)
		b.Run(fmt.Sprintf("%dKiB/sized", size>>10), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = ReadBody(strings.NewReader(body), int64(size), 1<<30)
			}
		})
		b.Run(fmt.Sprintf("%dKiB/readall", size>>10), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = io.ReadAll(strings.NewReader(body))
			}
		})
	}
}

func TestRetryableStatus(t *testing.T) {
	for status, want := range map[int]bool{
		200: false, 400: false, 404: false, 422: false,
		401: true, 403: true, 408: true, 429: true, 500: true, 503: true, 529: true,
	} {
		if got := RetryableStatus(status); got != want {
			t.Errorf("RetryableStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

func TestErrorMessage(t *testing.T) {
	cause := errors.New("boom")
	err := &Error{Provider: "openai", Status: 503, Err: cause}
	if !strings.Contains(err.Error(), "openai") || !strings.Contains(err.Error(), "503") {
		t.Fatalf("message %q", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Fatal("errors.Is does not reach the cause")
	}
	conn := &Error{Provider: "local", Err: cause}
	if !strings.Contains(conn.Error(), "boom") {
		t.Fatalf("message %q", conn.Error())
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		header string
		want   time.Time
	}{
		{"seconds", "30", now.Add(30 * time.Second)},
		{"one second", "1", now.Add(time.Second)},
		// Zero seconds is a provider that is ready now: a deadline that has arrived, not
		// the absence of one, so it is kept and can outrank a later provider.
		{"zero", "0", now},
		{"negative", "-5", time.Time{}},
		{"absent", "", time.Time{}},
		{"not a number", "soon", time.Time{}},
		{"a date", now.Add(45 * time.Second).Format(http.TimeFormat), now.Add(45 * time.Second)},
		// A date already past is a deadline that has run out, not a provider that gave
		// none, so it is kept: the caller may retry now rather than wait on a later
		// provider's longer word.
		{"a date already past", now.Add(-time.Minute).Format(http.TimeFormat), now.Add(-time.Minute)},
		{"over an hour", "7200", now.Add(MaxRetryAfter)},
		{"a number that would overflow", "9223372036854775807", now.Add(MaxRetryAfter)},
		{"a number past int64", "92233720368547758080", now.Add(MaxRetryAfter)},
		{"a negative number past int64", "-92233720368547758080", time.Time{}},
		{"a date past the bound", now.Add(2 * MaxRetryAfter).Format(http.TimeFormat), now.Add(MaxRetryAfter)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.header != "" {
				h.Set("Retry-After", tc.header)
			}
			if got := retryAfter(h, now); !got.Equal(tc.want) {
				t.Fatalf("retryAfter(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestSendKeepsRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	before := time.Now()
	resp, err := Send(context.Background(), srv.Client(), "p", srv.URL, []byte("{}"), nil, http.Header{})
	// Send closes the body of a failed answer itself and returns no response, but bodyclose
	// reads the signature, so the response is closed here the way any caller must.
	if resp != nil {
		_ = resp.Body.Close()
	}
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v", err)
	}
	if d := pe.RetryAt.Sub(before); d > 31*time.Second || d < 29*time.Second {
		t.Fatalf("RetryAt is %v away, want about 30 seconds", d)
	}
}

func TestHeaderContext(t *testing.T) {
	if _, ok := HeaderFrom(context.Background()); ok {
		t.Fatal("bare context has headers")
	}
	h := http.Header{"X-Request-Id": {"r1"}}
	got, ok := HeaderFrom(WithHeader(context.Background(), h))
	if !ok || got.Get("X-Request-ID") != "r1" {
		t.Fatalf("headers %v", got)
	}
}

func TestCannotCarry(t *testing.T) {
	plain := errors.New("boom")
	if got := CannotCarry(plain); got != plain { //nolint:errorlint // the same value must come back, not a wrap of it
		t.Fatalf("a plain error came back as %v", got)
	}
	if CannotCarry(nil) != nil {
		t.Fatal("nil came back as an error")
	}
	err := CannotCarry(fmt.Errorf("encoding: %w", &schema.ContentError{Reason: "no images at a URL"}))
	var ce *schema.ContentError
	if !errors.Is(err, ErrUnsupported) || !errors.As(err, &ce) || ce.Reason != "no images at a URL" {
		t.Fatalf("err = %v", err)
	}
}
