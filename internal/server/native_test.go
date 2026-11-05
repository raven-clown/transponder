package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/router"
)

func TestChatNativePassthrough(t *testing.T) {
	var got map[string]any
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("upstream got %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","system_fingerprint":"fp_1","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`)
	})
	e := setupRoutes(t, []router.Route{openaiRoute("fast", "gpt-4o-mini", up)})
	res := e.post(t, `{"model":"fast","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"logprobs":true,"tools":[{"type":"function","function":{"name":"f"}}]}`)

	if res.status != http.StatusOK {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	if got["model"] != "gpt-4o-mini" || got["logprobs"] != true || got["tools"] == nil {
		t.Fatalf("upstream body %v", got)
	}
	if !strings.Contains(string(res.body), `"system_fingerprint":"fp_1"`) {
		t.Fatalf("caller body %s", res.body)
	}
	if res.header.Get("X-Usage-Cost-USD") != "0.000450" || res.header.Get("X-Usage-Input-Tokens") != "1000" || res.header.Get("X-Usage-Output-Tokens") != "500" {
		t.Fatalf("usage headers %v", res.header)
	}
	recs := e.usage(t)
	if len(recs) != 1 {
		t.Fatalf("records %d", len(recs))
	}
	r := recs[0]
	if r.Route != "fast" || r.Provider != "oa" || r.Model != "gpt-4o-mini" || r.CostNUSD != 450_000 || r.Estimated || r.Status != 200 {
		t.Fatalf("record %+v", r)
	}
}

func TestChatNativeNoUsage(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"12345678"},"finish_reason":"stop"}]}`)
	})
	e := setupRoutes(t, []router.Route{openaiRoute("*", "", up)})
	res := e.post(t, ping)
	if res.status != http.StatusOK {
		t.Fatalf("status %d", res.status)
	}
	r := e.usage(t)[0]
	if !r.Estimated || r.OutputTokens != 2 || r.InputTokens != 1 {
		t.Fatalf("record %+v", r)
	}
}

func TestChatUpstreamErrors(t *testing.T) {
	closed := upstream(t, func(http.ResponseWriter, *http.Request) {})
	closed.Close()
	tests := []struct {
		name       string
		up         *httptest.Server
		wantStatus int
		wantBody   string
		wantCode   string
	}{
		{name: "400 passes through", wantStatus: 400, wantBody: `{"error":{"message":"bad","code":"invalid_value"}}`, up: statusUpstream(t, 400, `{"error":{"message":"bad","code":"invalid_value"}}`)},
		{name: "500", wantStatus: 502, wantCode: "upstream_error", up: statusUpstream(t, 500, `oops`)},
		{name: "401 from upstream", wantStatus: 502, wantCode: "upstream_error", up: statusUpstream(t, 401, `{}`)},
		{name: "connection refused", wantStatus: 502, wantCode: "upstream_error", up: closed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := setupRoutes(t, []router.Route{openaiRoute("*", "", tt.up)})
			res := e.post(t, ping)
			if res.status != tt.wantStatus {
				t.Fatalf("status %d: %s", res.status, res.body)
			}
			if tt.wantBody != "" && string(res.body) != tt.wantBody {
				t.Fatalf("body %s", res.body)
			}
			if tt.wantCode != "" && errorCode(t, res) != tt.wantCode {
				t.Fatalf("body %s", res.body)
			}
			if res.header.Get("X-Usage-Cost-USD") != "0.000000" {
				t.Fatalf("headers %v", res.header)
			}
			recs := e.usage(t)
			if len(recs) != 1 || recs[0].Status != tt.wantStatus {
				t.Fatalf("records %+v", recs)
			}
		})
	}
}

func statusUpstream(t *testing.T, status int, body string) *httptest.Server {
	return upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

func TestChatModelNotFound(t *testing.T) {
	e := setupRoutes(t, []router.Route{{Match: "only", Targets: []router.Target{{Provider: echo.New("echo"), Type: "echo"}}}})
	res := e.post(t, ping)
	if res.status != http.StatusNotFound || errorCode(t, res) != "model_not_found" {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	if res.header.Get("X-Usage-Cost-USD") != "0.000000" {
		t.Fatalf("headers %v", res.header)
	}
}
