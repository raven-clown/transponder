//go:build pprof

package main

import (
	"net/http"
	_ "net/http/pprof" //nolint:gosec // only in a build made with -tags pprof, on an address set by hand
	"os"
	"runtime"
	"time"
)

// A build with -tags pprof serves the Go profiles on TRANSPONDER_PPROF_ADDR, for bench/.
func init() {
	addr := os.Getenv("TRANSPONDER_PPROF_ADDR")
	if addr == "" {
		return
	}
	runtime.SetBlockProfileRate(int(10 * time.Microsecond))
	runtime.SetMutexProfileFraction(10)
	srv := &http.Server{Addr: addr, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.ListenAndServe() }()
}
