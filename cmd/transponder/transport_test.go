package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// TestProviderClientKeepsConnections sends two rounds of concurrent calls to one provider. The
// second round must reuse the first round's connections: a client that keeps only two idle
// ones dials again for every other call, and under load runs out of local ports.
func TestProviderClientKeepsConnections(t *testing.T) {
	const calls = 32
	var dialed atomic.Int64
	var arrived sync.WaitGroup
	release := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived.Done()
		<-release
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			dialed.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	client := providerClient()
	for round := range 2 {
		arrived.Add(calls)
		var done sync.WaitGroup
		for range calls {
			done.Go(func() {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
				if err != nil {
					t.Error(err)
					return
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			})
		}
		// Every call holds its connection until all of them are in, so they all run at once.
		arrived.Wait()
		close(release)
		done.Wait()
		release = make(chan struct{})
		if got := dialed.Load(); got != calls {
			t.Fatalf("after round %d the client had dialed %d connections, want %d", round+1, got, calls)
		}
	}
}
