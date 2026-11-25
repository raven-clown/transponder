package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/runtime"
	"github.com/raven-clown/transponder/internal/secrets"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/sqlite"
)

var testNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

type fakeReloader struct {
	mu     sync.Mutex
	checks int
	err    error
	status runtime.Status
	// cancellable counts checks made with a context the caller could cancel.
	cancellable int
}

func (f *fakeReloader) Check(ctx context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if ctx.Done() != nil {
		f.cancellable++
	}
	return f.err == nil, f.err
}

func (f *fakeReloader) Status() runtime.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeReloader) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks
}

// env is an admin API on a fresh SQLite store, with one key per role.
type env struct {
	t      *testing.T
	db     *sqlite.DB
	store  Store
	h      http.Handler
	spec   *spec
	reload *fakeReloader
	// router, when set, is what /status reports cooldowns from.
	router *router.Router
	logs   *bytes.Buffer
	keys   map[auth.Role]string
	file   *config.Bootstrap
	box    *secrets.Box
	// oidc, when set, is the identity provider /status reports.
	oidc *auth.OIDC
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &env{t: t, db: db, keys: map[auth.Role]string{}}
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleViewer, auth.RoleMember} {
		plain, hash, last4, err := auth.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		k := store.APIKey{OrgID: store.DefaultOrg, Name: "the-" + string(role), Hash: hash, Last4: last4, Role: string(role), CreatedAt: testNow}
		if err := db.CreateKey(context.Background(), k); err != nil {
			t.Fatal(err)
		}
		e.keys[role] = plain
	}
	if e.box, err = secrets.New(bytes.Repeat([]byte{1}, 32)); err != nil {
		t.Fatal(err)
	}
	return e.on(db)
}

// on returns an env with the same keys whose admin API uses st.
func (e *env) on(st Store) *env {
	return e.with(st, e.file, e.box)
}

// with returns an env with the same keys on st, with file as the config file and box sealing
// provider keys.
func (e *env) with(st Store, file *config.Bootstrap, box *secrets.Box) *env {
	e = &env{t: e.t, db: e.db, store: st, keys: e.keys, file: file, box: box, oidc: e.oidc, spec: loadSpec(e.t), logs: &bytes.Buffer{},
		reload: &fakeReloader{status: runtime.Status{Version: 1, LoadedAt: testNow.Add(-time.Hour)}}}
	e.h = New(Deps{
		File:      e.file,
		Secrets:   e.box,
		Auth:      auth.NewAuthenticator(e.db, func() time.Time { return testNow }),
		Store:     st,
		Reload:    e.reload,
		Logger:    slog.New(slog.NewJSONHandler(e.logs, nil)),
		Now:       func() time.Time { return testNow },
		Version:   "test",
		StoreKind: "sqlite",
		Started:   testNow.Add(-90 * time.Second),
		Router:    func() *router.Router { return e.router },
		OIDC:      e.oidc,
	})
	return e
}

// withOIDC returns the env with o as its identity provider.
func (e *env) withOIDC(o *auth.OIDC) *env {
	e.oidc = o
	return e.with(e.store, e.file, e.box)
}

// do sends a request as role, checks the response against openapi.yaml and returns it.
// headers are name, value pairs.
func (e *env) do(role auth.Role, method, path string, body any, headers ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := e.raw(role, method, path, body, headers...)
	if err := e.spec.checkResponse(method, strings.SplitN(path, "?", 2)[0], rec); err != nil {
		e.t.Error(err)
	}
	return rec
}

func (e *env) raw(role auth.Role, method, path string, body any, headers ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		r = bytes.NewReader(raw)
	}
	req := httptest.NewRequestWithContext(e.t.Context(), method, Prefix+path, r)
	req.Header.Set("X-Request-ID", "req-test")
	if key, ok := e.keys[role]; ok {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) handlerLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(e.logs, nil))
}

func (e *env) recorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func (e *env) now() time.Time { return testNow }

func (e *env) request() *http.Request {
	return httptest.NewRequestWithContext(e.t.Context(), http.MethodPost, Prefix+"/keys", nil)
}

// decodeBody decodes a response body into a map.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	return m
}

// wantError checks status, error code and field.
func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code, field string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d; body %s", rec.Code, status, rec.Body)
	}
	e := decodeBody(t, rec)["error"].(map[string]any)
	if e["code"] != code {
		t.Fatalf("code %v, want %s; body %s", e["code"], code, rec.Body)
	}
	if got, _ := e["field"].(string); got != field {
		t.Fatalf("field %q, want %q; body %s", got, field, rec.Body)
	}
}

// failing is a store whose calls fail after the keys are read, for the 500 paths.
type failing struct {
	Store
	err error
}

var errStore = errors.New("store down")

func (f failing) ListKeys(context.Context, string) ([]store.APIKey, error) { return nil, f.err }
func (f failing) KeyByName(context.Context, string, string) (store.APIKey, error) {
	return store.APIKey{}, f.err
}
func (f failing) ListUsage(context.Context, store.UsageQuery) ([]store.UsageRecord, error) {
	return nil, f.err
}
func (f failing) ListAudit(context.Context, store.AuditQuery) ([]store.AuditRecord, error) {
	return nil, f.err
}
func (f failing) UpdateConfig(context.Context, func(context.Context) error) (int64, error) {
	return 0, f.err
}
func (f failing) Ping(context.Context) error { return f.err }
