package server_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/pricing"
	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/server"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/sqlite"
)

type liveConfig struct {
	router  *router.Router
	limiter *limits.Limiter
}

// A call keeps the router and limiter it started with when a reload swaps them, and the next
// call uses the new ones.
func TestLiveConfigSwap(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	up := upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	var cur atomic.Pointer[liveConfig]
	cur.Store(&liveConfig{router: router.New([]router.Route{openaiRoute("gpt-*", "gpt-5", up)})})

	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	plain, hash, last4, err := auth.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateKey(ctx, store.APIKey{OrgID: store.DefaultOrg, Name: "alice", Hash: hash, Last4: last4, Role: string(auth.RoleMember), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(server.Deps{
		Auth:   auth.NewAuthenticator(db, time.Now),
		Prices: pricing.Default(),
		Usage:  db,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Now:    time.Now,
		Live: func() (*router.Router, *limits.Limiter) {
			c := cur.Load()
			return c.router, c.limiter
		},
	}))
	t.Cleanup(srv.Close)
	e := &env{srv: srv, db: db, key: plain}

	first := make(chan result, 1)
	go func() {
		res, err := e.do(http.MethodPost, map[string]string{"Authorization": "Bearer " + e.key}, `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`)
		if err != nil {
			t.Error(err)
		}
		first <- res
	}()
	<-entered

	// The reload drops the gpt-* route and adds a budget.
	budgeted := limits.New(limits.Config{Budgets: []limits.Budget{{Subject: "user:*", Window: limits.Month, LimitNUSD: 1}}}, limits.NewMemoryStore(), time.Now)
	cur.Store(&liveConfig{
		router:  router.New([]router.Route{{Match: "echo-1", Targets: []router.Target{{Provider: echo.New("echo"), Type: "echo", Free: true}}}}),
		limiter: budgeted,
	})
	if res := e.post(t, `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`); res.status != http.StatusNotFound {
		t.Fatalf("a new call on the old route: %d %s", res.status, res.body)
	}
	if res := e.post(t, `{"model":"echo-1","messages":[{"role":"user","content":"hi"}]}`); res.status != http.StatusOK {
		t.Fatalf("a new call on the new route: %d %s", res.status, res.body)
	}
	close(release)
	if res := <-first; res.status != http.StatusOK || res.header.Get("X-Transponder-Model") != "gpt-5" {
		t.Fatalf("the call in flight: %d %v %s", res.status, res.header, res.body)
	}

	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	res, err := e.doPath(http.MethodGet, "/v1/models", map[string]string{"Authorization": "Bearer " + e.key}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(res.body, &models); err != nil {
		t.Fatal(err)
	}
	if len(models.Data) != 1 || models.Data[0].ID != "echo-1" {
		t.Fatalf("models after the reload: %s", res.body)
	}
	res, err = e.doPath(http.MethodGet, "/v1/usage", map[string]string{"Authorization": "Bearer " + e.key}, "")
	if err != nil || res.status != http.StatusOK || !strings.Contains(string(res.body), `"budget"`) {
		t.Fatalf("usage after the reload shows no budget: %d %v %s", res.status, err, res.body)
	}
}
