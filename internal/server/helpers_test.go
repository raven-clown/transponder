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
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/pricing"
	"github.com/raven-clown/transponder/internal/provider"
	oaprovider "github.com/raven-clown/transponder/internal/provider/openai"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/server"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/sqlite"
)

type env struct {
	srv *httptest.Server
	db  *sqlite.DB
	key string
}

type result struct {
	status  int
	header  http.Header
	trailer http.Header
	body    []byte
}

func echoRoutes(p provider.Provider) []router.Route {
	return []router.Route{{Match: "*", Targets: []router.Target{{Provider: p, Type: "echo"}}}}
}

func setup(t *testing.T, p provider.Provider) *env {
	t.Helper()
	return setupRoutes(t, echoRoutes(p))
}

func setupRoutes(t *testing.T, routes []router.Route) *env {
	t.Helper()
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
	k := store.APIKey{OrgID: store.DefaultOrg, Name: "alice", Hash: hash, Last4: last4, Role: string(auth.RoleMember), CreatedAt: time.Now()}
	if err := db.CreateKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(server.Deps{
		Auth:   auth.NewAuthenticator(db, time.Now),
		Router: router.New(routes),
		Prices: pricing.Default(),
		Usage:  db,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Now:    time.Now,
	}))
	t.Cleanup(srv.Close)
	return &env{srv: srv, db: db, key: plain}
}

func (e *env) do(method string, headers map[string]string, body string) (result, error) {
	return e.doPath(method, "/v1/chat/completions", headers, body)
}

func (e *env) doPath(method, path string, headers map[string]string, body string) (result, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		return result{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		return result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, header: resp.Header, trailer: resp.Trailer, body: b}, err
}

func (e *env) post(t *testing.T, body string) result {
	t.Helper()
	res, err := e.do(http.MethodPost, map[string]string{"Authorization": "Bearer " + e.key}, body)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e *env) usage(t *testing.T) []store.UsageRecord {
	t.Helper()
	recs, err := e.db.RecentUsage(context.Background(), store.DefaultOrg, 100)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func errorCode(t *testing.T, res result) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.body, &body); err != nil {
		t.Fatalf("decoding %q: %v", res.body, err)
	}
	return body.Error.Code
}

func upstream(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func openaiRoute(match, model string, up *httptest.Server) router.Route {
	a := oaprovider.New(oaprovider.Options{Name: "oa", BaseURL: up.URL, APIKey: "k", Client: up.Client()})
	return router.Route{Match: match, Targets: []router.Target{{Provider: a, Type: "openai", Model: model}}}
}
