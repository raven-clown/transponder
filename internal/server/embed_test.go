package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/server"
)

func (e *env) embed(t *testing.T, headers map[string]string, body string) result {
	t.Helper()
	res, err := e.doPath(http.MethodPost, "/v1/embeddings", headers, body)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e *env) auth() map[string]string {
	return map[string]string{"Authorization": "Bearer " + e.key}
}

func TestEmbeddingsNative(t *testing.T) {
	var got map[string]any
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"model":"text-embedding-3-small","usage":{"prompt_tokens":1000000,"total_tokens":1000000}}`)
	})
	e := setupRoutes(t, []router.Route{openaiRoute("embed", "text-embedding-3-small", up)})
	res := e.embed(t, e.auth(), `{"model":"embed","input":"hello","dimensions":256}`)
	if res.status != http.StatusOK {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	if got["model"] != "text-embedding-3-small" || got["dimensions"] != float64(256) {
		t.Fatalf("upstream body %v", got)
	}
	if _, ok := got["stream_options"]; ok {
		t.Fatalf("stream_options sent to embeddings: %v", got)
	}
	if res.header.Get("X-Usage-Cost-USD") != "0.020000" || res.header.Get("X-Usage-Input-Tokens") != "1000000" {
		t.Fatalf("headers %v", res.header)
	}
	r := e.usage(t)[0]
	if r.Endpoint != "embeddings" || r.CostNUSD != 20_000_000 || r.Route != "embed" || r.Status != 200 {
		t.Fatalf("record %+v", r)
	}
}

func TestEmbeddingsEcho(t *testing.T) {
	e := setup(t, echo.New("echo"))
	res := e.embed(t, e.auth(), `{"model":"e","input":["a","bb"]}`)
	if res.status != http.StatusOK {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res.body, &body); err != nil || body.Object != "list" || len(body.Data) != 2 || body.Data[1].Index != 1 || len(body.Data[0].Embedding) != 8 {
		t.Fatalf("body %s: %v", res.body, err)
	}
	if res.header.Get("X-Usage-Input-Tokens") != "2" {
		t.Fatalf("headers %v", res.header)
	}
	if r := e.usage(t)[0]; r.Endpoint != "embeddings" || r.InputTokens != 2 {
		t.Fatalf("record %+v", r)
	}
}

func TestEmbeddingsErrors(t *testing.T) {
	only := []router.Route{{Match: "e", Targets: []router.Target{{Provider: echo.New("echo"), Type: "echo"}}}}
	tests := []struct {
		name    string
		routes  []router.Route
		noAuth  bool
		body    string
		status  int
		code    string
		records int
	}{
		{name: "unauthenticated", routes: only, noAuth: true, body: `{"model":"e","input":"x"}`, status: 401, code: "invalid_api_key"},
		{name: "missing input", routes: only, body: `{"model":"e"}`, status: 400, code: "missing_field"},
		{name: "bad json", routes: only, body: `{`, status: 400, code: "invalid_json"},
		{name: "no route", routes: only, body: `{"model":"other","input":"x"}`, status: 404, code: "model_not_found"},
		{name: "upstream 500", routes: []router.Route{openaiRoute("*", "", statusUpstream(t, 500, "oops"))}, body: `{"model":"e","input":"x"}`, status: 502, code: "upstream_error", records: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := setupRoutes(t, tt.routes)
			h := e.auth()
			if tt.noAuth {
				h = nil
			}
			res := e.embed(t, h, tt.body)
			if res.status != tt.status || errorCode(t, res) != tt.code {
				t.Fatalf("status %d: %s", res.status, res.body)
			}
			if n := len(e.usage(t)); n != tt.records {
				t.Fatalf("records %d", n)
			}
		})
	}
}

func TestUpstreamBodyTooLarge(t *testing.T) {
	defer server.SetMaxUpstreamBytes(64)()
	up := upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3,0.4,0.5,0.6,0.7]}],"usage":{"prompt_tokens":1}}`)
	})
	e := setupRoutes(t, []router.Route{openaiRoute("*", "", up)})
	for _, path := range []string{"/v1/embeddings", "/v1/chat/completions"} {
		res, err := e.doPath(http.MethodPost, path, e.auth(), `{"model":"e","input":"x","messages":[{"role":"user","content":"x"}]}`)
		if err != nil {
			t.Fatal(err)
		}
		if res.status != http.StatusBadGateway || errorCode(t, res) != "upstream_too_large" {
			t.Fatalf("%s: status %d: %s", path, res.status, res.body)
		}
	}
}
