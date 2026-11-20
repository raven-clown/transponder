package server_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/router"
)

func TestRequestTimeout(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	e := setupWith(t, []router.Route{openaiRoute("*", "", up)}, 200*time.Millisecond, 0)
	res := e.post(t, streamPing)
	p := payloads(t, res.body)
	if len(p) != 2 || !strings.Contains(p[1], "upstream_error") {
		t.Fatalf("payloads %q", p)
	}
}

func TestAttemptTimeoutBeforeFirstEvent(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	e := setupWith(t, []router.Route{openaiRoute("*", "", up)}, time.Minute, 100*time.Millisecond)
	start := time.Now()
	res := e.post(t, streamPing)
	if res.status != http.StatusBadGateway || errorCode(t, res) != "upstream_error" {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("gave up after %s, want about the attempt timeout", took)
	}
}

func TestAttemptTimeoutSparesSlowAnswers(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		<-time.After(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"late"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	e := setupWith(t, []router.Route{openaiRoute("*", "", up)}, time.Minute, 100*time.Millisecond)
	if res := e.post(t, ping); res.status != http.StatusOK {
		t.Fatalf("non-streaming status %d: %s", res.status, res.body)
	}
	res, err := e.doPath(http.MethodPost, "/v1/embeddings", e.auth(), `{"model":"m","input":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusOK {
		t.Fatalf("embeddings status %d: %s", res.status, res.body)
	}
}

func TestRequestTimeoutNonStreaming(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	up := upstream(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	e := setupWith(t, []router.Route{openaiRoute("*", "", up)}, 150*time.Millisecond, 0)
	res := e.post(t, ping)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	if r := e.usage(t)[0]; r.Status != http.StatusBadGateway {
		t.Fatalf("record %+v", r)
	}
}
