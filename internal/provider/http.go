package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxErrorBody = 1 << 20

// TraceHeaders are copied from the caller's request to every upstream call. They are in Go's
// canonical form, so Get and Set do not copy the key on every call.
var TraceHeaders = []string{"X-Request-Id", "Traceparent", "Tracestate"}

// Send posts body to url with the given headers plus the trace headers from from, and returns
// the response when it is 2xx. Any other answer, and a failed connection, is an *Error.
func Send(ctx context.Context, client *http.Client, name, url string, body []byte, headers, from http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Provider: name, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	for _, h := range TraceHeaders {
		if v := from.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &Error{Provider: name, Retryable: true, Err: err}
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	// Read the deadline the headers asked for before the body: an HTTP date is an instant,
	// not a delay, so a slow error body must not push it further into the future.
	retryAt := retryAfter(resp.Header, time.Now())
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return nil, &Error{
		Provider:  name,
		Status:    resp.StatusCode,
		Retryable: RetryableStatus(resp.StatusCode),
		RetryAt:   retryAt,
		Body:      errBody,
		Err:       fmt.Errorf("upstream answered %s", resp.Status),
	}
}

// retryAfter reads a Retry-After header, given in seconds or as an HTTP date, as the instant
// it asks the caller to wait until, bounded to MaxRetryAfter. A header that is absent, that
// does not parse, or that gives a negative number of seconds holds no deadline, and gives the
// zero time. Zero seconds gives now, and a date already past gives that instant back: both are
// a deadline that has already arrived, which is not the same as having asked for none.
func retryAfter(h http.Header, now time.Time) time.Time {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return time.Time{}
	}
	// A number too large for an int64 comes back as an error with the largest value, which
	// is a wait longer than any caller should be given, so it reads as one.
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		// Bound before multiplying, since a huge number of seconds overflows the product.
		if secs < 0 {
			return time.Time{}
		}
		if secs == 0 {
			return now
		}
		if secs >= int64(MaxRetryAfter/time.Second) {
			return now.Add(MaxRetryAfter)
		}
		return now.Add(time.Duration(secs) * time.Second)
	}
	at, err := http.ParseTime(v)
	if err != nil {
		return time.Time{}
	}
	if wait := at.Sub(now); wait > MaxRetryAfter {
		return now.Add(MaxRetryAfter)
	}
	return at
}
