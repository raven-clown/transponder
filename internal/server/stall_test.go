package server_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestStalledStream checks a stream that stops sending after its first output: the request
// timeout ends it with an error event, it is recorded, and a target that keeps stalling is
// cooled down like one whose streams break.
func TestStalledStream(t *testing.T) {
	stall := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"par\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
	a := countedUpstream(t, stall)
	b := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, upstreamChunks) })
	e := setupWith(t, route("*", oaTarget("a", "", a), oaTarget("b", "", b)), 300*time.Millisecond, 0)
	for range 3 {
		res := e.post(t, streamPing)
		p := payloads(t, res.body)
		if len(p) != 2 || !strings.Contains(p[0], "par") || !strings.Contains(p[1], "upstream_error") {
			t.Fatalf("payloads %q", p)
		}
	}
	recs := e.usage(t)
	if len(recs) != 3 || recs[0].Status != http.StatusOK || recs[0].Provider != "a" || !recs[0].Estimated {
		t.Fatalf("records %+v", recs)
	}
	res := e.post(t, streamPing)
	if res.header.Get("X-Transponder-Provider") != "b" || a.calls.Load() != 3 {
		t.Fatalf("a called %d times, headers %v: the stalling target was not cooled down", a.calls.Load(), res.header)
	}
}
