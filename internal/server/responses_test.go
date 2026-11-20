package server_test

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/server"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/tokens"
)

const (
	responsesPing   = `{"model":"m","input":"ping"}`
	responsesStream = `{"model":"m","input":"ping","stream":true}`
)

//go:embed testdata/responses
var responsesData embed.FS

// fixture reads a Responses API body kept under testdata/responses.
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := responsesData.ReadFile("testdata/responses/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// replay answers every call with a recorded body.
func replay(code int, contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func (e *env) responses(t *testing.T, body string) result {
	t.Helper()
	res, err := e.doPath(http.MethodPost, "/v1/responses", e.auth(), body)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestResponsesPassthrough(t *testing.T) {
	plain := fixture(t, "plain.json")
	var got map[string]any
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("upstream got %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		replay(http.StatusOK, "application/json", plain)(w, r)
	})
	e := setupRoutes(t, []router.Route{openaiRoute("fast", "gpt-4o-mini", up)})
	res := e.responses(t, `{"model":"fast","instructions":"Be brief.","input":[{"role":"user","content":[{"type":"input_text","text":"Tell me a story."}]}],"previous_response_id":null,"reasoning":{"effort":"low"},"tools":[{"type":"web_search"}],"include":["reasoning.encrypted_content"]}`)

	if res.status != http.StatusOK {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	if string(res.body) != plain {
		t.Fatalf("the caller's body is not the provider's: %s", res.body)
	}
	// Only the model changes, and store is set to false, which the caller left out.
	if got["model"] != "gpt-4o-mini" || got["store"] != false || got["instructions"] != "Be brief." || got["input"] == nil ||
		got["reasoning"] == nil || got["tools"] == nil || got["include"] == nil || len(got) != 8 {
		t.Fatalf("upstream body %v", got)
	}
	h := res.header
	if h.Get("X-Usage-Input-Tokens") != "36" || h.Get("X-Usage-Output-Tokens") != "87" || h.Get("X-Usage-Reasoning-Tokens") != "0" || h.Get("X-Usage-Cost-USD") != "0.000057" {
		t.Fatalf("usage headers %v", h)
	}
	if h.Get("X-Transponder-Provider") != "oa" || h.Get("X-Transponder-Model") != "gpt-4o-mini" || h.Get("X-Transponder-Fallbacks") != "0" {
		t.Fatalf("routing headers %v", h)
	}
	recs := e.usage(t)
	if len(recs) != 1 {
		t.Fatalf("records %d", len(recs))
	}
	r := recs[0]
	if r.Endpoint != "responses" || r.Route != "fast" || r.Provider != "oa" || r.Model != "gpt-4o-mini" || r.User != "alice" ||
		r.InputTokens != 36 || r.OutputTokens != 87 || r.CostNUSD != 57_600 || r.Estimated || r.Status != 200 {
		t.Fatalf("record %+v", r)
	}
}

func TestResponsesUsage(t *testing.T) {
	tests := []struct {
		name, request, contentType, body   string
		in, out, cacheRead, reasoning, nus int64
		estimated                          bool
	}{
		{name: "cached and reasoning tokens", request: responsesPing, contentType: "application/json", body: fixture(t, "reasoning.json"),
			in: 176, cacheRead: 1024, out: 152, reasoning: 448, nus: 6_348_000},
		{name: "cached and reasoning tokens in a stream", request: responsesStream, contentType: "text/event-stream", body: fixture(t, "stream_reasoning.sse"),
			in: 252, cacheRead: 2048, out: 20, reasoning: 320, nus: 3_971_000},
		{name: "no usage is an estimate", request: responsesPing, contentType: "application/json",
			body: `{"object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"12345678"}]}],"usage":null}`,
			in:   7, out: 2, nus: 28_750, estimated: true},
		{name: "a stream that ends without usage is an estimate", request: responsesStream, contentType: "text/event-stream",
			body: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"12345678\"}\n\nevent: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"usage\":null}}\n\n",
			in:   8, out: 2, nus: 30_000, estimated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := upstream(t, replay(http.StatusOK, tt.contentType, tt.body))
			e := setupRoutes(t, []router.Route{openaiRoute("*", "gpt-5", up)})
			res := e.responses(t, tt.request)
			if res.status != http.StatusOK {
				t.Fatalf("status %d: %s", res.status, res.body)
			}
			r := e.usage(t)[0]
			if r.InputTokens != tt.in || r.OutputTokens != tt.out || r.CacheReadTokens != tt.cacheRead || r.ReasoningTokens != tt.reasoning || r.CostNUSD != tt.nus || r.Estimated != tt.estimated {
				t.Fatalf("record %+v", r)
			}
			h := res.header
			if strings.Contains(tt.request, "stream") {
				h = res.trailer
			}
			if h.Get("X-Usage-Reasoning-Tokens") != jsonNumber(tt.reasoning) || h.Get("X-Usage-Input-Tokens") != jsonNumber(tt.in) {
				t.Fatalf("usage headers %v", h)
			}
		})
	}
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestResponsesStream(t *testing.T) {
	stream := fixture(t, "stream.sse")
	var got map[string]any
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		replay(http.StatusOK, "text/event-stream", stream)(w, r)
	})
	e := setupRoutes(t, []router.Route{openaiRoute("*", "gpt-4o-mini", up)})
	res := e.responses(t, `{"model":"m","input":"hello","stream":true,"store":true}`)

	if res.status != http.StatusOK || res.header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, headers %v: %s", res.status, res.header, res.body)
	}
	// Every event reaches the caller as sent, names included, and nothing is added after the last.
	if string(res.body) != stream {
		t.Fatalf("the caller's stream is not the provider's:\n%s", res.body)
	}
	// The caller asked for store, and the provider is told not to.
	if got["model"] != "gpt-4o-mini" || got["stream"] != true || got["store"] != false || len(got) != 4 {
		t.Fatalf("upstream body %v", got)
	}
	checkTrailers(t, res, "37", "11", "0.000012")
	r := e.usage(t)[0]
	if r.Endpoint != "responses" || r.InputTokens != 37 || r.OutputTokens != 11 || r.CostNUSD != 12_150 || r.Estimated || r.Status != 200 {
		t.Fatalf("record %+v", r)
	}
}

func TestResponsesStreamCut(t *testing.T) {
	events := strings.SplitAfter(fixture(t, "stream.sse"), "\n\n")
	firstDelta := strings.Join(events[:5], "")
	tests := map[string]struct {
		upstream string
		wantLast string
	}{
		"the stream stops before the response ends": {firstDelta, `"code":"upstream_error"`},
		"the provider sends an error event": {
			firstDelta + "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"The server had an error.\",\"param\":null,\"sequence_number\":5}\n\n",
			`"code":"server_error"`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a := countedUpstream(t, replay(http.StatusOK, "text/event-stream", tt.upstream))
			b := countedUpstream(t, replay(http.StatusOK, "text/event-stream", fixture(t, "stream.sse")))
			e := setupRoutes(t, route("*", oaTarget("a", "gpt-4o-mini", a), oaTarget("b", "gpt-4o-mini", b)))
			res := e.responses(t, responsesStream)
			frames := strings.Split(strings.TrimSpace(string(res.body)), "\n\n")
			last := frames[len(frames)-1]
			if res.status != http.StatusOK || len(frames) != 6 || !strings.HasPrefix(last, "event: error\ndata: ") || !strings.Contains(last, tt.wantLast) || !strings.Contains(last, `"sequence_number":5`) {
				t.Fatalf("status %d, frames %q", res.status, frames)
			}
			// Bytes had reached the caller, so the next target is not tried.
			if b.calls.Load() != 0 {
				t.Fatalf("fell back after the stream had started")
			}
			r := e.usage(t)[0]
			if !r.Estimated || r.OutputTokens == 0 || r.InputTokens == 0 || r.Provider != "a" {
				t.Fatalf("record %+v", r)
			}
		})
	}
}

func TestResponsesUpstreamErrors(t *testing.T) {
	bad := fixture(t, "error_400.json")
	tests := []struct {
		name       string
		request    string
		first      http.HandlerFunc
		wantStatus int
		wantBody   string
		wantCode   string
	}{
		{name: "a 400 passes through", request: responsesPing, first: replay(400, "application/json", bad), wantStatus: 400, wantBody: bad},
		{name: "a 400 on a stream passes through", request: responsesStream, first: replay(400, "application/json", bad), wantStatus: 400, wantBody: bad},
		{name: "a 500 is a 502", request: responsesPing, first: replay(500, "text/plain", "oops"), wantStatus: 502, wantCode: "upstream_error"},
		{name: "a refused key is a 502", request: responsesPing, first: replay(401, "application/json", `{}`), wantStatus: 502, wantCode: "upstream_error"},
		{name: "a stream that begins with an error is a 502", request: responsesStream, wantStatus: 502, wantCode: "upstream_error",
			first: replay(200, "text/event-stream", "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"x\",\"param\":null,\"sequence_number\":0}\n\n")},
		{name: "an empty stream is a 502", request: responsesStream, first: replay(200, "text/event-stream", ""), wantStatus: 502, wantCode: "upstream_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := countedUpstream(t, tt.first)
			e := setupRoutes(t, route("*", oaTarget("oa", "gpt-4o-mini", up)))
			res := e.responses(t, tt.request)
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
			if len(recs) != 1 || recs[0].Status != tt.wantStatus || recs[0].Endpoint != "responses" {
				t.Fatalf("records %+v", recs)
			}
		})
	}
}

func TestResponsesFallback(t *testing.T) {
	plain, stream := fixture(t, "plain.json"), fixture(t, "stream.sse")
	tests := map[string]struct {
		request string
		first   http.HandlerFunc
		second  http.HandlerFunc
	}{
		"server error":          {responsesPing, status(500, "oops"), replay(200, "application/json", plain)},
		"rate limited":          {responsesPing, status(429, "{}"), replay(200, "application/json", plain)},
		"stream, error status":  {responsesStream, status(503, "x"), replay(200, "text/event-stream", stream)},
		"stream, empty":         {responsesStream, replay(200, "text/event-stream", ""), replay(200, "text/event-stream", stream)},
		"stream, error event":   {responsesStream, replay(200, "text/event-stream", "event: error\ndata: {\"type\":\"error\",\"message\":\"x\"}\n\n"), replay(200, "text/event-stream", stream)},
		"no Responses API, 404": {responsesPing, status(404, "404 page not found"), replay(200, "application/json", plain)},
		"no Responses API, 405": {responsesStream, status(405, ""), replay(200, "text/event-stream", stream)},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a := countedUpstream(t, tt.first)
			b := countedUpstream(t, tt.second)
			e := setupRoutes(t, route("*", oaTarget("a", "gpt-5", a), oaTarget("b", "gpt-4o-mini", b)))
			res := e.responses(t, tt.request)
			if res.status != http.StatusOK || a.calls.Load() != 1 || b.calls.Load() != 1 {
				t.Fatalf("status %d, a called %d times, b %d: %s", res.status, a.calls.Load(), b.calls.Load(), res.body)
			}
			if res.header.Get("X-Transponder-Provider") != "b" || res.header.Get("X-Transponder-Fallbacks") != "1" {
				t.Fatalf("headers %v", res.header)
			}
			recs := e.usage(t)
			if len(recs) != 1 || recs[0].Provider != "b" || recs[0].Model != "gpt-4o-mini" || recs[0].Fallbacks != 1 || recs[0].CostNUSD == 0 {
				t.Fatalf("records %+v", recs)
			}
		})
	}
}

func TestResponsesAttemptTimeout(t *testing.T) {
	stream := fixture(t, "stream.sse")
	release := make(chan struct{})
	defer close(release)
	slow := countedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	b := countedUpstream(t, replay(200, "text/event-stream", stream))
	e := setupWith(t, route("*", oaTarget("a", "gpt-5", slow), oaTarget("b", "gpt-4o-mini", b)), time.Minute, 100*time.Millisecond)
	res := e.responses(t, responsesStream)
	if res.status != http.StatusOK || string(res.body) != stream || res.header.Get("X-Transponder-Provider") != "b" {
		t.Fatalf("status %d, headers %v: %s", res.status, res.header, res.body)
	}
}

func TestResponsesBodyTooLarge(t *testing.T) {
	defer server.SetMaxUpstreamBytes(64)()
	up := countedUpstream(t, replay(200, "application/json", fixture(t, "plain.json")))
	e := setupRoutes(t, route("*", oaTarget("a", "", up)))
	res := e.responses(t, responsesPing)
	if res.status != http.StatusBadGateway || errorCode(t, res) != "upstream_too_large" {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
}

func TestResponsesSkipsTargetsOfAnotherDialect(t *testing.T) {
	plain := fixture(t, "plain.json")
	an := countedUpstream(t, status(200, "{}"))
	oa := countedUpstream(t, replay(200, "application/json", plain))

	e := setupRoutes(t, route("*", anTarget("claude-test", an), oaTarget("b", "gpt-4o-mini", oa)))
	res := e.responses(t, responsesPing)
	if res.status != http.StatusOK || string(res.body) != plain || an.calls.Load() != 0 || res.header.Get("X-Transponder-Provider") != "b" {
		t.Fatalf("status %d, anthropic called %d times: %s", res.status, an.calls.Load(), res.body)
	}

	// A route with no OpenAI-dialect target cannot serve the API at all.
	for name, routes := range map[string][]router.Route{
		"anthropic only": route("*", anTarget("claude-test", an)),
		"echo only":      echoRoutes(echo.New("echo")),
	} {
		t.Run(name, func(t *testing.T) {
			only := setupRoutes(t, routes)
			for _, body := range []string{responsesPing, responsesStream} {
				res := only.responses(t, body)
				if res.status != http.StatusBadRequest || errorCode(t, res) != "unsupported" || !strings.Contains(string(res.body), "speak the OpenAI API") {
					t.Fatalf("status %d: %s", res.status, res.body)
				}
			}
			if an.calls.Load() != 0 {
				t.Fatalf("anthropic called %d times", an.calls.Load())
			}
		})
	}
}

func TestResponsesWithoutTheEndpoint(t *testing.T) {
	up := countedUpstream(t, status(404, `{"error":{"message":"Unknown request URL: POST /v1/responses"}}`))
	e := setupRoutes(t, route("*", oaTarget("a", "", up)))
	res := e.responses(t, responsesPing)
	if res.status != http.StatusBadRequest || errorCode(t, res) != "unsupported" || !strings.Contains(string(res.body), "answered 404: Unknown request URL") {
		t.Fatalf("status %d: %s", res.status, res.body)
	}

	// A missing endpoint is not an outage: the target is not cooled down, so chat still starts there.
	chatOnly := countedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			status(404, "404 page not found")(w, r)
			return
		}
		status(200, okChat)(w, r)
	})
	other := countedUpstream(t, replay(200, "application/json", fixture(t, "plain.json")))
	e = setupRoutes(t, route("*", oaTarget("a", "", chatOnly), oaTarget("b", "", other)))
	for i := range 5 {
		if res := e.responses(t, responsesPing); res.status != http.StatusOK || res.header.Get("X-Transponder-Provider") != "b" {
			t.Fatalf("call %d: status %d, headers %v", i, res.status, res.header)
		}
	}
	if res := e.post(t, ping); res.status != http.StatusOK || res.header.Get("X-Transponder-Provider") != "a" {
		t.Fatalf("chat after the skipped calls: status %d, headers %v", res.status, res.header)
	}
}

// addKey stores another member key and returns its secret.
func (e *env) addKey(t *testing.T, name string) string {
	t.Helper()
	plain, hash, last4, err := auth.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.CreateKey(context.Background(), store.APIKey{OrgID: store.DefaultOrg, Name: name, Hash: hash, Last4: last4, Role: string(auth.RoleMember), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return plain
}

type wireParamError struct {
	Error struct {
		Code    string `json:"code"`
		Param   string `json:"param"`
		Message string `json:"message"`
	} `json:"error"`
}

// Every caller of a route shares the provider account, so a response one caller made must not
// be continued by another. The gateway stores nothing and refuses stored state from anyone.
func TestResponsesStoredStateRefused(t *testing.T) {
	up := countedUpstream(t, replay(200, "application/json", fixture(t, "plain.json")))
	e := setupRoutes(t, route("*", oaTarget("a", "gpt-4o-mini", up)))
	bob := e.addKey(t, "bob")

	first := e.responses(t, `{"model":"m","input":"Tell me a story.","store":true}`)
	var made struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(first.body, &made); err != nil || first.status != http.StatusOK || made.ID == "" {
		t.Fatalf("first call %d: %s", first.status, first.body)
	}
	chained := `{"model":"m","input":"and then?","previous_response_id":"` + made.ID + `"}`

	tests := map[string]struct {
		key, body, wantParam string
	}{
		"another caller continues the response": {bob, chained, "previous_response_id"},
		"the caller who made it, too":           {e.key, chained, "previous_response_id"},
		"the field in another case":             {bob, `{"model":"m","input":"x","Previous_Response_Id":"` + made.ID + `"}`, "previous_response_id"},
		"a conversation by id":                  {bob, `{"model":"m","input":"x","conversation":"conv_689667905b048191b4740501625afd940c7533ace33a2dab"}`, "conversation"},
		"a conversation as an object":           {bob, `{"model":"m","input":"x","conversation":{"id":"conv_1"}}`, "conversation"},
		"an item the provider stored":           {bob, `{"model":"m","input":[{"type":"item_reference","id":"msg_67ccd2bf17f0819081ff3bb2cf6508e60bb6a6b452d3795b"}]}`, "input"},
		"stored state on a stream":              {bob, `{"model":"m","input":"x","stream":true,"previous_response_id":"` + made.ID + `"}`, "previous_response_id"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			res, err := e.doPath(http.MethodPost, "/v1/responses", map[string]string{"Authorization": "Bearer " + tt.key}, tt.body)
			if err != nil {
				t.Fatal(err)
			}
			var we wireParamError
			_ = json.Unmarshal(res.body, &we)
			if res.status != http.StatusBadRequest || we.Error.Code != "unsupported" || we.Error.Param != tt.wantParam || !strings.Contains(we.Error.Message, "send the whole conversation in input") {
				t.Fatalf("status %d: %s", res.status, res.body)
			}
			if res.header.Get("X-Usage-Cost-USD") != "0.000000" {
				t.Fatalf("headers %v", res.header)
			}
		})
	}
	if up.calls.Load() != 1 || len(e.usage(t)) != 1 {
		t.Fatalf("upstream called %d times, %d records; only the first call should have gone through", up.calls.Load(), len(e.usage(t)))
	}
}

func TestResponsesBadRequests(t *testing.T) {
	up := countedUpstream(t, replay(200, "application/json", fixture(t, "plain.json")))
	e := setupRoutes(t, []router.Route{{Match: "known", Targets: []router.Target{oaTarget("a", "gpt-5", up)}}})
	tests := map[string]struct {
		body       string
		wantStatus int
		wantCode   string
	}{
		"not JSON":             {`{"model":`, 400, "invalid_json"},
		"no model":             {`{"input":"x"}`, 400, "missing_field"},
		"stream is not a bool": {`{"model":"known","input":"x","stream":"yes"}`, 400, "invalid_json"},
		"background":           {`{"model":"known","input":"x","background":true}`, 400, "unsupported"},
		"unknown model":        {`{"model":"other","input":"x"}`, 404, "model_not_found"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			res := e.responses(t, tt.body)
			if res.status != tt.wantStatus || errorCode(t, res) != tt.wantCode {
				t.Fatalf("status %d: %s", res.status, res.body)
			}
		})
	}
	if up.calls.Load() != 0 || len(e.usage(t)) != 0 {
		t.Fatalf("upstream called %d times, %d records", up.calls.Load(), len(e.usage(t)))
	}
}

// A provider written in Go reads Background as background and Store as store, so the gateway
// must not let either by.
func TestResponsesCaseVariants(t *testing.T) {
	var got map[string]json.RawMessage
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		replay(200, "application/json", fixture(t, "plain.json"))(w, r)
	})
	e := setupRoutes(t, []router.Route{openaiRoute("*", "gpt-5", up)})
	for _, body := range []string{
		`{"model":"m","input":"x","Background":true}`,
		`{"model":"m","input":"x","BACKGROUND":true,"background":false,"stream":false}`,
		`{"model":"m","input":"x","Store":true}`,
		`{"model":"m","input":"x","store":false,"STORE":true}`,
	} {
		got = nil
		if res := e.responses(t, body); res.status != http.StatusOK {
			t.Fatalf("status %d: %s", res.status, res.body)
		}
		for k := range got {
			for _, field := range []string{"background", "store"} {
				if k != field && strings.EqualFold(k, field) {
					t.Fatalf("%s reached the provider with %s", body, k)
				}
			}
		}
		if string(got["input"]) != `"x"` || string(got["model"]) != `"gpt-5"` || string(got["store"]) != "false" {
			t.Fatalf("upstream body %v", got)
		}
	}
}

func TestResponsesRouteAccess(t *testing.T) {
	e, p := setupOIDCRoutes(t, []router.Route{
		{Match: "fast", Access: allow(t, "team:platform"), Targets: []router.Target{oaTarget("a", "gpt-4o-mini", countedUpstream(t, replay(200, "application/json", fixture(t, "plain.json"))))}},
	})
	elsewhere := p.Claims()
	elsewhere["groups"] = []string{"sales"}
	tests := map[string]struct {
		credential string
		wantStatus int
		wantCode   string
	}{
		"a member of the team":       {p.Token(p.Claims()), http.StatusOK, ""},
		"a user of another team":     {p.Token(elsewhere), http.StatusForbidden, "route_forbidden"},
		"a key the route leaves out": {e.key, http.StatusForbidden, "route_forbidden"},
		"no credentials":             {"", http.StatusUnauthorized, "invalid_api_key"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			headers := map[string]string{}
			if tt.credential != "" {
				headers["Authorization"] = "Bearer " + tt.credential
			}
			before := len(e.usage(t))
			res, err := e.doPath(http.MethodPost, "/v1/responses", headers, `{"model":"fast","input":"ping"}`)
			if err != nil {
				t.Fatal(err)
			}
			if res.status != tt.wantStatus {
				t.Fatalf("status %d, want %d: %s", res.status, tt.wantStatus, res.body)
			}
			if tt.wantStatus == http.StatusOK {
				if r := e.usage(t)[0]; r.User == "" || r.Key != "" || !slices.Contains(r.Teams, "platform") {
					t.Fatalf("record %+v", r)
				}
				return
			}
			if errorCode(t, res) != tt.wantCode {
				t.Fatalf("body %s", res.body)
			}
			var we wireError
			_ = json.Unmarshal(res.body, &we)
			if tt.wantStatus == http.StatusForbidden && !strings.Contains(we.Error.Message, `route "fast"`) {
				t.Fatalf("the refusal does not name the route: %s", res.body)
			}
			if after := len(e.usage(t)); after != before {
				t.Fatalf("a refused call was recorded")
			}
		})
	}
}

func allow(t *testing.T, entries ...string) router.Access {
	t.Helper()
	a, err := router.ParseAccess(entries)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestResponsesLimits(t *testing.T) {
	big := `{"object":"response","status":"completed","output":[],"usage":{"input_tokens":1000000,"output_tokens":0,"total_tokens":1000000}}`

	t.Run("a spent budget refuses the next call", func(t *testing.T) {
		up := countedUpstream(t, replay(200, "application/json", big))
		cfg := limits.Config{Budgets: []limits.Budget{{Subject: "user:*", Window: limits.Month, LimitNUSD: 100_000_000}}}
		e := setupLimited(t, route("*", oaTarget("a", "gpt-4o-mini", up)), cfg)
		if res := e.responses(t, responsesPing); res.status != http.StatusOK {
			t.Fatalf("first call %d: %s", res.status, res.body)
		}
		res := e.responses(t, responsesPing)
		if res.status != http.StatusPaymentRequired || errorCode(t, res) != "budget_exceeded" {
			t.Fatalf("status %d: %s", res.status, res.body)
		}
		// The budget is one pot for every endpoint.
		if res := e.post(t, ping); res.status != http.StatusPaymentRequired {
			t.Fatalf("chat after the budget was spent: %d", res.status)
		}
		if up.calls.Load() != 1 || len(e.usage(t)) != 1 {
			t.Fatalf("upstream called %d times, %d records", up.calls.Load(), len(e.usage(t)))
		}
	})

	t.Run("a streamed call counts against the budget", func(t *testing.T) {
		stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + big + "}\n\n"
		up := countedUpstream(t, replay(200, "text/event-stream", stream))
		cfg := limits.Config{Budgets: []limits.Budget{{Subject: "user:*", Window: limits.Month, LimitNUSD: 100_000_000}}}
		e := setupLimited(t, route("*", oaTarget("a", "gpt-4o-mini", up)), cfg)
		if res := e.responses(t, responsesStream); res.status != http.StatusOK {
			t.Fatalf("first call %d: %s", res.status, res.body)
		}
		if res := e.responses(t, responsesStream); res.status != http.StatusPaymentRequired {
			t.Fatalf("status %d: %s", res.status, res.body)
		}
	})

	t.Run("the request rate", func(t *testing.T) {
		up := countedUpstream(t, replay(200, "application/json", fixture(t, "plain.json")))
		r := limits.Rate{N: 1, Per: time.Minute}
		e := setupLimited(t, route("*", oaTarget("a", "", up)), oneProfile(func(p *limits.Profile) { p.RequestRate = &r }))
		if res := e.responses(t, responsesPing); res.status != http.StatusOK || res.header.Get("RateLimit-Policy") == "" {
			t.Fatalf("first call %d, headers %v", res.status, res.header)
		}
		if res := e.responses(t, responsesPing); res.status != http.StatusTooManyRequests || errorCode(t, res) != "rate_limited" || res.header.Get("Retry-After") != "60" {
			t.Fatalf("status %d, headers %v: %s", res.status, res.header, res.body)
		}
	})

	t.Run("the output cap", func(t *testing.T) {
		var got map[string]json.RawMessage
		up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got = nil
			_ = json.Unmarshal(b, &got)
			replay(200, "application/json", fixture(t, "plain.json"))(w, r)
		})
		e := setupLimited(t, []router.Route{openaiRoute("*", "", up)}, oneProfile(func(p *limits.Profile) { p.MaxOutputTokens = 100 }))
		for body, want := range map[string]string{
			`{"model":"m","input":"x","max_output_tokens":5000}`: "100",
			`{"model":"m","input":"x","max_output_tokens":20}`:   "20",
			`{"model":"m","input":"x","Max_Output_Tokens":9000}`: "100",
			responsesPing: "100",
		} {
			e.responses(t, body)
			if string(got["max_output_tokens"]) != want || string(got["store"]) != "false" || len(got) != 4 {
				t.Fatalf("%s reached the provider as %v", body, got)
			}
		}
	})

	t.Run("the input limit counts images", func(t *testing.T) {
		up := countedUpstream(t, replay(200, "application/json", fixture(t, "plain.json")))
		e := setupLimited(t, route("*", oaTarget("a", "", up)), oneProfile(func(p *limits.Profile) { p.MaxInputTokens = 2*tokens.Image + 100 }))
		image := `{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("QUJD", 4000) + `"}`
		request := func(parts ...string) string {
			return `{"model":"m","input":[{"role":"user","content":[` + strings.Join(parts, ",") + `]}]}`
		}
		if res := e.responses(t, request(image, image)); res.status != http.StatusOK {
			t.Fatalf("two images: %d: %s", res.status, res.body)
		}
		if res := e.responses(t, request(image, image, image)); res.status != http.StatusBadRequest || errorCode(t, res) != "input_too_large" {
			t.Fatalf("three images: %d: %s", res.status, res.body)
		}
	})

	t.Run("the input limit refuses what it cannot count", func(t *testing.T) {
		up := countedUpstream(t, replay(200, "application/json", fixture(t, "plain.json")))
		e := setupLimited(t, route("*", oaTarget("a", "", up)), oneProfile(func(p *limits.Profile) { p.MaxInputTokens = 100_000 }))
		message := func(part string) string {
			return `{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"Summarise this."},` + part + `]}]}`
		}
		tests := map[string]struct{ body, wantParam string }{
			"a stored prompt":       {`{"model":"m","prompt":{"id":"pmpt_68af1f8b2c5c8190a2e4b1d0f3c9e7a60d1b2c3e4f5a6b7c","version":"3"},"input":"hi"}`, "prompt"},
			"a file by id":          {message(`{"type":"input_file","file_id":"file-6F2ksmvXxt4VdoqmHRw6kL"}`), "input"},
			"a file at a URL":       {message(`{"type":"input_file","file_url":"https://example.com/report.pdf"}`), "input"},
			"an inline file":        {message(`{"type":"input_file","filename":"report.pdf","file_data":"data:application/pdf;base64,` + strings.Repeat("QUJD", 1000) + `"}`), "input"},
			"a file in tool output": {`{"model":"m","input":[{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_file","file_id":"file-1"}]}]}`, "input"},
			"a part typed twice":    {message(`{"type":"input_text","TYPE":"input_file","text":"x","file_id":"file-1"}`), "input"},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				res := e.responses(t, tt.body)
				var we wireParamError
				_ = json.Unmarshal(res.body, &we)
				if res.status != http.StatusBadRequest || we.Error.Code != "input_not_countable" || we.Error.Param != tt.wantParam || !strings.Contains(we.Error.Message, "100000") {
					t.Fatalf("status %d: %s", res.status, res.body)
				}
			})
		}
		if up.calls.Load() != 0 || len(e.usage(t)) != 0 {
			t.Fatalf("upstream called %d times, %d records", up.calls.Load(), len(e.usage(t)))
		}
	})

	t.Run("without an input limit, a file is reserved like an image", func(t *testing.T) {
		up := countedUpstream(t, replay(200, "application/json", fixture(t, "plain.json")))
		e := setupLimited(t, route("*", oaTarget("a", "", up)), oneProfile(func(p *limits.Profile) { p.TokensPerDay = 2*tokens.Image + 100 }))
		file := `{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,QUJD"}`
		request := func(parts ...string) string {
			return `{"model":"m","max_output_tokens":1,"input":[{"role":"user","content":[` + strings.Join(parts, ",") + `]}]}`
		}
		if res := e.responses(t, request(file, file, file)); res.status != http.StatusPaymentRequired || errorCode(t, res) != "quota_exceeded" {
			t.Fatalf("three files: %d: %s", res.status, res.body)
		}
		if res := e.responses(t, request(file, file)); res.status != http.StatusOK {
			t.Fatalf("two files: %d: %s", res.status, res.body)
		}
		if res := e.responses(t, `{"model":"m","max_output_tokens":1,"prompt":{"id":"pmpt_1"}}`); res.status != http.StatusOK {
			t.Fatalf("a stored prompt: %d: %s", res.status, res.body)
		}
	})
}
