package server_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/tokens"
)

// TestEstimateWithoutProviderUsage checks the tokens counted when a provider reports none: an
// inline image counts as tokens.Image whatever its size, never as the text of its base64 data,
// and the answer is counted too.
func TestEstimateWithoutProviderUsage(t *testing.T) {
	big := strings.Repeat("A", 400_000)
	anImage := `{"model":"m","max_tokens":16,%s"messages":[{"role":"user","content":[{"type":"text","text":"what is this"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + big + `"}}]}]}`
	oaImage := `{"model":"m",%s"messages":[{"role":"user","content":[{"type":"text","text":"what is this"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + big + `"}}]}]}`
	const (
		oaStream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a cat\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		oaPlain  = `{"choices":[{"index":0,"message":{"role":"assistant","content":"a cat"},"finish_reason":"stop"}]}`
		anPlain  = `{"type":"message","role":"assistant","content":[{"type":"text","text":"a cat"}],"stop_reason":"end_turn"}`
		anStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"a cat\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	)
	cases := []struct {
		name   string
		path   string
		body   string
		answer string
		route  func(string, *counted) router.Route
	}{
		{"messages to anthropic", "/v1/messages", strings.Replace(anImage, "%s", "", 1), anPlain, anRoute},
		{"messages stream to anthropic", "/v1/messages", strings.Replace(anImage, "%s", `"stream":true,`, 1), anStream, anRoute},
		{"messages to openai", "/v1/messages", strings.Replace(anImage, "%s", "", 1), oaPlain, oaRoute},
		{"messages stream to openai", "/v1/messages", strings.Replace(anImage, "%s", `"stream":true,`, 1), oaStream, oaRoute},
		{"chat to openai", "/v1/chat/completions", strings.Replace(oaImage, "%s", "", 1), oaPlain, oaRoute},
		{"chat stream to openai", "/v1/chat/completions", strings.Replace(oaImage, "%s", `"stream":true,`, 1), oaStream, oaRoute},
		{"chat stream to anthropic", "/v1/chat/completions", strings.Replace(oaImage, "%s", `"stream":true,`, 1), anStream, anRoute},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			up := countedUpstream(t, status(http.StatusOK, tt.answer))
			e := setupRoutes(t, []router.Route{tt.route("*", up)})
			res, err := e.doPath(http.MethodPost, tt.path, map[string]string{"Authorization": "Bearer " + e.key}, tt.body)
			if err != nil {
				t.Fatal(err)
			}
			if res.status != http.StatusOK {
				t.Fatalf("status %d: %s", res.status, res.body)
			}
			recs := e.usage(t)
			if len(recs) != 1 || !recs[0].Estimated {
				t.Fatalf("records %+v, want one estimated", recs)
			}
			r := recs[0]
			if r.InputTokens < tokens.Image || r.InputTokens > tokens.Image+50 {
				t.Errorf("input tokens %d, want the image at %d plus a few for the text", r.InputTokens, tokens.Image)
			}
			if r.OutputTokens == 0 {
				t.Errorf("the answer was not counted")
			}
			got := res.header.Get("X-Usage-Input-Tokens") + res.trailer.Get("X-Usage-Input-Tokens")
			if got != strconv.FormatInt(r.InputTokens, 10) {
				t.Errorf("usage header %q, record %d", got, r.InputTokens)
			}
		})
	}
}
