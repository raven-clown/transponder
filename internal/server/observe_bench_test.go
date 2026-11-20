package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/pricing"
	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/server"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/sqlite"
	"github.com/raven-clown/transponder/internal/telemetry"
)

type discardSpans struct{}

func (discardSpans) Export(context.Context, []telemetry.SpanData) error { return nil }

// memoryStore holds one key and throws usage away, so a benchmark times the gateway and
// not the disk.
type memoryStore struct {
	store.UsageStore
	key store.APIKey
}

func (m *memoryStore) CreateKey(context.Context, store.APIKey) error { return nil }

func (m *memoryStore) KeyByHash(_ context.Context, hash [32]byte) (store.APIKey, error) {
	if hash != m.key.Hash {
		return store.APIKey{}, store.ErrNotFound
	}
	return m.key, nil
}

func (m *memoryStore) HasAdminKey(context.Context, string, time.Time) (bool, error) {
	return false, nil
}

func (m *memoryStore) RecordUsage(context.Context, store.UsageRecord) error { return nil }

// BenchmarkChat is one chat call through the gateway against the echo provider, with the
// key and the usage record in memory or in SQLite. "off" has no metrics and no tracer,
// which is the path every call took before there were any, so the other two show what
// they add.
func BenchmarkChat(b *testing.B) {
	tracer, err := telemetry.NewTracer(telemetry.TracerConfig{Exporter: discardSpans{}})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = tracer.Shutdown(context.Background()) }()
	plain, hash, last4, err := auth.NewKey()
	if err != nil {
		b.Fatal(err)
	}
	key := store.APIKey{OrgID: store.DefaultOrg, Name: "bench", Hash: hash, Last4: last4, Role: string(auth.RoleMember), CreatedAt: time.Now()}
	ctx := context.Background()
	for _, storage := range []string{"memory", "sqlite"} {
		for _, bb := range []struct {
			name    string
			metrics *telemetry.Metrics
			tracer  *telemetry.Tracer
		}{
			{"off", nil, nil},
			{"metrics", telemetry.NewMetrics("bench"), nil},
			{"metrics and tracing", telemetry.NewMetrics("bench"), tracer},
		} {
			b.Run(storage+"/"+bb.name, func(b *testing.B) {
				var keys store.KeyStore
				var usage store.UsageStore
				if storage == "sqlite" {
					db, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "b.db"))
					if err != nil {
						b.Fatal(err)
					}
					defer func() { _ = db.Close() }()
					if err := db.CreateKey(ctx, key); err != nil {
						b.Fatal(err)
					}
					keys, usage = db, db
				} else {
					mem := &memoryStore{key: key}
					keys, usage = mem, mem
				}
				h := server.New(server.Deps{
					Auth:    auth.NewAuthenticator(keys, time.Now),
					Router:  router.New(echoRoutes(echo.New("echo"))),
					Prices:  pricing.Default(),
					Usage:   usage,
					Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
					Now:     time.Now,
					Metrics: bb.metrics,
					Tracer:  bb.tracer,
				})
				b.ReportAllocs()
				for b.Loop() {
					req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(ping))
					req.Header.Set("Authorization", "Bearer "+plain)
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					if rec.Code != http.StatusOK {
						b.Fatalf("status %d: %s", rec.Code, rec.Body)
					}
				}
			})
		}
	}
}
