package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/provider/gemini"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/schema"
)

const (
	tinyPNG     = "iVBORw0KGgo="
	okMessage   = `{"type":"message","content":[{"type":"text","text":"a cat"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	okGenerate  = `{"candidates":[{"content":{"parts":[{"text":"a cat"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1}}`
	openaiImage = `{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is "},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + tinyPNG + `"}},` +
		`{"type":"text","text":"next to "},` +
		`{"type":"image_url","image_url":{"url":"https://example.com/b.png"}},` +
		`{"type":"text","text":"?"}]}]}`
	anthropicImage = `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is "},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + tinyPNG + `"}},` +
		`{"type":"text","text":"next to "},` +
		`{"type":"image","source":{"type":"url","url":"https://example.com/b.png"}},` +
		`{"type":"text","text":"?"}]}]}`
)

// recorded is a fake upstream that keeps the last body it was sent.
type recorded struct {
	*counted
	mu   sync.Mutex
	body []byte
}

func recordedUpstream(t *testing.T, answer string) *recorded {
	t.Helper()
	r := &recorded{}
	r.counted = countedUpstream(t, func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.body = b
		r.mu.Unlock()
		_, _ = io.WriteString(w, answer)
	})
	return r
}

// content returns the content of the first message the upstream got, found under path.
func (r *recorded) content(t *testing.T, list, field string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(r.body, &top); err != nil {
		t.Fatalf("upstream body %q: %v", r.body, err)
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(top[list], &msgs); err != nil || len(msgs) == 0 {
		t.Fatalf("upstream body has no %s: %s", list, r.body)
	}
	return string(msgs[0][field])
}

func gmTarget(up *counted) router.Target {
	return router.Target{Provider: gemini.New("gem", up.srv.URL, "k", up.srv.Client()), Type: "gemini", Model: "gemini-2.5-flash"}
}

func echoTarget() router.Target {
	return router.Target{Provider: echo.New("echo"), Type: "echo"}
}

func TestImagesReachEveryDialectInOrder(t *testing.T) {
	asBlocks := `[{"type":"text","text":"what is "},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + tinyPNG + `"}},` +
		`{"type":"text","text":"next to "},` +
		`{"type":"image","source":{"type":"url","url":"https://example.com/b.png"}},` +
		`{"type":"text","text":"?"}]`
	asParts := `[{"type":"text","text":"what is "},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + tinyPNG + `"}},` +
		`{"type":"text","text":"next to "},` +
		`{"type":"image_url","image_url":{"url":"https://example.com/b.png"}},` +
		`{"type":"text","text":"?"}]`

	t.Run("openai caller, anthropic target", func(t *testing.T) {
		up := recordedUpstream(t, okMessage)
		e := setupRoutes(t, route("*", anTarget("claude-x", up.counted)))
		res := e.post(t, openaiImage)
		if res.status != http.StatusOK || !strings.Contains(string(res.body), "a cat") {
			t.Fatalf("status %d: %s", res.status, res.body)
		}
		if got := up.content(t, "messages", "content"); got != asBlocks {
			t.Fatalf("anthropic got\n%s\nwant\n%s", got, asBlocks)
		}
	})

	t.Run("anthropic caller, openai target", func(t *testing.T) {
		up := recordedUpstream(t, okChat)
		e := setupRoutes(t, route("*", oaTarget("b", "gpt-x", up.counted)))
		res := e.messages(t, nil, anthropicImage)
		if res.status != http.StatusOK || !strings.Contains(string(res.body), "from b") {
			t.Fatalf("status %d: %s", res.status, res.body)
		}
		if got := up.content(t, "messages", "content"); got != asParts {
			t.Fatalf("openai got\n%s\nwant\n%s", got, asParts)
		}
	})

	t.Run("openai caller, gemini target", func(t *testing.T) {
		up := recordedUpstream(t, okGenerate)
		e := setupRoutes(t, route("*", gmTarget(up.counted)))
		inlineOnly := `{"model":"m","messages":[{"role":"user","content":[` +
			`{"type":"text","text":"what is "},` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + tinyPNG + `"}},` +
			`{"type":"text","text":"?"}]}]}`
		res := e.post(t, inlineOnly)
		if res.status != http.StatusOK || !strings.Contains(string(res.body), "a cat") {
			t.Fatalf("status %d: %s", res.status, res.body)
		}
		want := `[{"text":"what is "},{"inlineData":{"mimeType":"image/png","data":"` + tinyPNG + `"}},{"text":"?"}]`
		if got := up.content(t, "contents", "parts"); got != want {
			t.Fatalf("gemini got\n%s\nwant\n%s", got, want)
		}
	})
}

func TestEchoAnswersWithTheTextOfAnImageRequest(t *testing.T) {
	e := setupRoutes(t, route("*", echoTarget()))
	res := e.post(t, openaiImage)
	if res.status != http.StatusOK || !strings.Contains(string(res.body), `"content":"echo: what is next to ?"`) {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	res = e.messages(t, nil, anthropicImage)
	if res.status != http.StatusOK || !strings.Contains(string(res.body), `"text":"echo: what is next to ?"`) {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	stream := strings.Replace(openaiImage, `{"model":"m",`, `{"model":"m","stream":true,`, 1)
	res = e.post(t, stream)
	if res.status != http.StatusOK || !strings.Contains(string(res.body), `"content":"next "`) || !strings.Contains(string(res.body), "[DONE]") {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
}

func TestTargetThatCannotTakeAnImageIsSkipped(t *testing.T) {
	gm := countedUpstream(t, status(200, okGenerate))
	an := countedUpstream(t, status(200, okMessage))

	// Gemini cannot take the image at a URL; the Anthropic target after it can.
	e := setupRoutes(t, route("*", gmTarget(gm), anTarget("claude-x", an)))
	res := e.post(t, openaiImage)
	if res.status != http.StatusOK || !strings.Contains(string(res.body), "a cat") || gm.calls.Load() != 0 || an.calls.Load() != 1 {
		t.Fatalf("status %d, gemini called %d times, anthropic %d: %s", res.status, gm.calls.Load(), an.calls.Load(), res.body)
	}
	if got := res.header.Get("X-Transponder-Provider"); got != "claude" {
		t.Fatalf("served by %q, want claude", got)
	}
	// A skipped target did not fail, so it is not put in cooldown and the next call still goes
	// to it first.
	if r := e.usage(t)[0]; r.Provider != "claude" {
		t.Fatalf("record %+v", r)
	}
	text := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	if res = e.post(t, text); res.status != http.StatusOK || gm.calls.Load() != 1 {
		t.Fatalf("status %d, gemini called %d times: %s", res.status, gm.calls.Load(), res.body)
	}

	// The same over a stream.
	stream := strings.Replace(openaiImage, `{"model":"m",`, `{"model":"m","stream":true,`, 1)
	sse := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, upstreamChunks) })
	e = setupRoutes(t, route("*", gmTarget(gm), oaTarget("b", "gpt-x", sse)))
	before := gm.calls.Load()
	res = e.post(t, stream)
	if res.status != http.StatusOK || !strings.Contains(string(res.body), "[DONE]") || gm.calls.Load() != before || sse.calls.Load() != 1 {
		t.Fatalf("status %d, gemini called %d more times: %s", res.status, gm.calls.Load()-before, res.body)
	}
}

func TestNoTargetCanTakeTheImage(t *testing.T) {
	gm := countedUpstream(t, status(200, okGenerate))
	e := setupRoutes(t, route("*", gmTarget(gm)))
	const reason = "Gemini takes inline images only, not an image at a URL"

	res := e.post(t, openaiImage)
	if res.status != http.StatusBadRequest || errorCode(t, res) != "unsupported" || !strings.Contains(string(res.body), reason) {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	res = e.messages(t, nil, anthropicImage)
	if res.status != http.StatusBadRequest || !strings.Contains(string(res.body), `"invalid_request_error"`) || !strings.Contains(string(res.body), reason) {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	stream := strings.Replace(openaiImage, `{"model":"m",`, `{"model":"m","stream":true,`, 1)
	res = e.post(t, stream)
	if res.status != http.StatusBadRequest || !strings.Contains(string(res.body), reason) {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	if gm.calls.Load() != 0 {
		t.Fatalf("gemini was called %d times", gm.calls.Load())
	}

	// A GIF is inline, but not a type Gemini takes.
	gif := `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/gif;base64,` + tinyPNG + `"}}]}]}`
	res = e.post(t, gif)
	if res.status != http.StatusBadRequest || !strings.Contains(string(res.body), "not image/gif") {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
}

func TestInlineImageSizeCap(t *testing.T) {
	tooLarge := strings.Repeat("AAAA", schema.MaxInlineImageBytes/3+1)
	chat := `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + tooLarge + `"}}]}]}`
	block := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + tooLarge + `"}}]}]}`
	const limit = "over the 5 MiB limit"

	// Translated to another dialect, the image is checked and refused.
	an := countedUpstream(t, status(200, okMessage))
	e := setupRoutes(t, route("*", anTarget("claude-x", an)))
	res := e.post(t, chat)
	if res.status != http.StatusBadRequest || errorCode(t, res) != "image_too_large" || !strings.Contains(string(res.body), limit) || an.calls.Load() != 0 {
		t.Fatalf("status %d, upstream called %d times: %.300s", res.status, an.calls.Load(), res.body)
	}
	oa := countedUpstream(t, status(200, okChat))
	e = setupRoutes(t, route("*", oaTarget("b", "gpt-x", oa)))
	res = e.messages(t, nil, block)
	if res.status != http.StatusBadRequest || !strings.Contains(string(res.body), limit) || oa.calls.Load() != 0 {
		t.Fatalf("status %d, upstream called %d times: %.300s", res.status, oa.calls.Load(), res.body)
	}

	// Sent on in the caller's own dialect, the body is not opened: the provider's limit applies.
	res = e.post(t, chat)
	if res.status != http.StatusOK || oa.calls.Load() != 1 {
		t.Fatalf("status %d, upstream called %d times: %.300s", res.status, oa.calls.Load(), res.body)
	}
}

func TestMalformedImagePart(t *testing.T) {
	e := setupRoutes(t, route("*", echoTarget()))
	for name, part := range map[string]string{
		"no url":         `{"type":"image_url","image_url":{}}`,
		"bad base64":     `{"type":"image_url","image_url":{"url":"data:image/png;base64,###"}}`,
		"not an image":   `{"type":"image_url","image_url":{"url":"data:text/html;base64,QUJD"}}`,
		"unknown scheme": `{"type":"image_url","image_url":{"url":"ftp://example.com/a.png"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			res := e.post(t, `{"model":"m","messages":[{"role":"user","content":[`+part+`]}]}`)
			if res.status != http.StatusBadRequest || errorCode(t, res) != "invalid_image" {
				t.Fatalf("status %d: %s", res.status, res.body)
			}
		})
	}
}

func TestImagesCountWhenTheProviderReportsNoUsage(t *testing.T) {
	up := countedUpstream(t, status(200, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	e := setupRoutes(t, route("*", oaTarget("b", "gpt-x", up)))
	res := e.post(t, openaiImage)
	if res.status != http.StatusOK {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	// Two images at 1,000 tokens each, and 19 characters of text.
	if got := res.header.Get("X-Usage-Input-Tokens"); got != "2005" {
		t.Fatalf("input tokens %s, want 2005", got)
	}
	if r := e.usage(t)[0]; !r.Estimated || r.InputTokens != 2005 {
		t.Fatalf("record %+v", r)
	}
}
