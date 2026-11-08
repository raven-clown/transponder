package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

const maxErrorBody = 1 << 20

// TraceHeaders are copied from the caller's request to every upstream call.
var TraceHeaders = []string{"X-Request-ID", "Traceparent", "Tracestate"}

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
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return nil, &Error{
		Provider:  name,
		Status:    resp.StatusCode,
		Retryable: RetryableStatus(resp.StatusCode),
		Body:      errBody,
		Err:       fmt.Errorf("upstream answered %s", resp.Status),
	}
}
