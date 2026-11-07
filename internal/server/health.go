package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// readyTimeout bounds each readiness check, so a stuck dependency answers 503
// instead of hanging the probe.
var readyTimeout = 2 * time.Second

// Pinger is a dependency that /readyz checks.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Healthz reports that the process is up. It checks nothing else.
func Healthz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowGet(w, r) {
			return
		}
		writeStatus(w, http.StatusOK, map[string]any{"status": "ok"})
	})
}

// Readyz pings every check at once and answers 503 with the names of the
// ones that failed or ran out of time.
func Readyz(checks map[string]Pinger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowGet(w, r) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		var (
			mu     sync.Mutex
			failed []string
			wg     sync.WaitGroup
		)
		for name, check := range checks {
			wg.Go(func() {
				if err := check.Ping(ctx); err != nil {
					mu.Lock()
					failed = append(failed, name)
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		if len(failed) > 0 {
			sort.Strings(failed)
			writeStatus(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "failed": failed})
			return
		}
		writeStatus(w, http.StatusOK, map[string]any{"status": "ok"})
	})
}

func allowGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	writeStatus(w, http.StatusMethodNotAllowed, map[string]any{"status": "method_not_allowed"})
	return false
}

func writeStatus(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
