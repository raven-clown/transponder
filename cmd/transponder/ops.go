package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// serveOps serves h, the probes and the metrics, on addr until the returned stop is called.
// With no addr there is nothing to start.
func serveOps(ctx context.Context, addr string, h http.Handler, logger *slog.Logger) (stop func(), err error) {
	if addr == "" {
		return func() {}, nil
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ops_listen_addr: %w", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("the ops listener stopped", "err", err)
		}
	}()
	logger.Info("ops listening", "addr", ln.Addr().String())
	return func() { _ = srv.Close() }, nil
}
