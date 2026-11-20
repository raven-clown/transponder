package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/telemetry"
)

// spanFlushTimeout is how long the last spans get to reach the collector at shutdown.
var spanFlushTimeout = 5 * time.Second

// startTracing returns the tracer for the otel settings, nil while otel.endpoint is empty,
// and a function that exports what is still queued. Spans the collector does not take are
// counted in m.
func startTracing(cfg config.OTel, m *telemetry.Metrics, logger *slog.Logger) (*telemetry.Tracer, func(), error) {
	tracer, err := telemetry.NewTracer(telemetry.TracerConfig{
		Endpoint:    cfg.Endpoint,
		Headers:     cfg.Headers,
		SampleRatio: cfg.SampleRatio,
		Version:     version,
		Client:      &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
		Logger:      logger,
		Dropped:     m.SpansDropped,
	})
	if err != nil || tracer == nil {
		return nil, func() {}, err
	}
	logger.Info("tracing", "endpoint", cfg.Endpoint)
	return tracer, func() {
		ctx, cancel := context.WithTimeout(context.Background(), spanFlushTimeout)
		defer cancel()
		if err := tracer.Shutdown(ctx); err != nil {
			logger.Warn("stopping the tracer", "err", err)
		}
	}, nil
}
