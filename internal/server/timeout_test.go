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
	e := setupWith(t, []router.Route{openaiRoute("*", "", up)}, 200*time.Millisecond)
	res := e.post(t, streamPing)
	p := payloads(t, res.body)
	if len(p) != 2 || !strings.Contains(p[1], "upstream_error") {
		t.Fatalf("payloads %q", p)
	}
}
