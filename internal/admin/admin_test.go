package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/api"
	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/runtime"
	"github.com/raven-clown/transponder/internal/store"
)

func TestRoles(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name   string
		role   auth.Role
		method string
		path   string
		body   any
		want   int
		code   string
	}{
		{"no key", "", http.MethodGet, "/keys", nil, http.StatusUnauthorized, "unauthenticated"},
		{"member reads", auth.RoleMember, http.MethodGet, "/keys", nil, http.StatusForbidden, "forbidden"},
		{"viewer reads", auth.RoleViewer, http.MethodGet, "/keys", nil, http.StatusOK, ""},
		{"viewer writes", auth.RoleViewer, http.MethodPost, "/keys", map[string]any{"name": "x"}, http.StatusForbidden, "forbidden"},
		{"viewer deletes", auth.RoleViewer, http.MethodDelete, "/keys/the-member", nil, http.StatusForbidden, "forbidden"},
		{"admin writes", auth.RoleAdmin, http.MethodPost, "/keys", map[string]any{"name": "x"}, http.StatusCreated, ""},
		{"member status", auth.RoleMember, http.MethodGet, "/status", nil, http.StatusForbidden, "forbidden"},
		{"viewer audit", auth.RoleViewer, http.MethodGet, "/audit", nil, http.StatusOK, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := e.do(tt.role, tt.method, tt.path, tt.body)
			if tt.code != "" {
				wantError(t, rec, tt.want, tt.code, "")
				return
			}
			if rec.Code != tt.want {
				t.Fatalf("status %d, want %d; body %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

func TestUnknownEndpoint(t *testing.T) {
	e := newEnv(t)
	rec := e.raw(auth.RoleAdmin, http.MethodGet, "/nothing", nil)
	wantError(t, rec, http.StatusNotFound, "not_found", "")
}

func TestServesOpenAPI(t *testing.T) {
	e := newEnv(t)
	rec := e.do(auth.RoleViewer, http.MethodGet, "/openapi.yaml", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != string(api.OpenAPI) || rec.Header().Get("Content-Type") != "application/yaml" {
		t.Fatalf("status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestStatus(t *testing.T) {
	e := newEnv(t)
	body := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/status", nil))
	if body["version"] != "test" || body["uptime_seconds"] != float64(90) {
		t.Fatalf("status %v", body)
	}
	st := body["store"].(map[string]any)
	if st["kind"] != "sqlite" || st["healthy"] != true {
		t.Fatalf("store %v", st)
	}
	cfg := body["config"].(map[string]any)
	if cfg["version"] != float64(1) || cfg["reload_failed"] != false || cfg["failed_at"] != nil {
		t.Fatalf("config %v", cfg)
	}

	e.reload.status = runtime.Status{Version: 3, LoadedAt: testNow, Failed: true, FailedVersion: 4, Error: "route x uses provider y", FailedAt: testNow}
	cfg = decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/status", nil))["config"].(map[string]any)
	if cfg["reload_failed"] != true || cfg["failed_version"] != float64(4) || cfg["error"] != "route x uses provider y" || cfg["failed_at"] == nil {
		t.Fatalf("config %v", cfg)
	}
}

func TestStatusStoreDown(t *testing.T) {
	base := newEnv(t)
	e := base.on(failing{Store: base.db, err: errStore})
	st := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/status", nil))["store"].(map[string]any)
	if st["healthy"] != false || st["error"] != "store down" {
		t.Fatalf("store %v", st)
	}
}

func TestStoreErrors(t *testing.T) {
	base := newEnv(t)
	e := base.on(failing{Store: base.db, err: errStore})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/keys"},
		{http.MethodGet, "/keys/x"},
		{http.MethodPost, "/keys"},
		{http.MethodPatch, "/keys/x"},
		{http.MethodDelete, "/keys/x"},
		{http.MethodPost, "/keys/x/rotate"},
		{http.MethodGet, "/usage"},
		{http.MethodGet, "/requests"},
		{http.MethodGet, "/audit"},
	} {
		var body any
		switch tc.method {
		case http.MethodPost:
			body = map[string]any{"name": "x"}
		case http.MethodPatch:
			body = map[string]any{}
		}
		if strings.HasSuffix(tc.path, "/rotate") {
			body = nil
		}
		rec := e.raw(auth.RoleAdmin, tc.method, tc.path, body)
		wantError(t, rec, http.StatusInternalServerError, "internal", "")
	}
	if !strings.Contains(e.logs.String(), "store down") {
		t.Fatalf("the error was not logged: %s", e.logs)
	}
}

func TestDenyErrors(t *testing.T) {
	e := newEnv(t)
	h := &handler{d: Deps{Logger: e.handlerLogger()}}
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{auth.ErrUnavailable, http.StatusServiceUnavailable, "auth_unavailable"},
		{errors.New("lookup failed"), http.StatusInternalServerError, "internal"},
	}
	for _, tt := range tests {
		rec := e.recorder()
		h.deny(rec, e.request(), tt.err)
		wantError(t, rec, tt.status, tt.code, "")
	}
}

func TestIfMatch(t *testing.T) {
	tests := []struct {
		header string
		want   int64
		ok     bool
	}{
		{"", 0, true},
		{`"3"`, 3, true},
		{`W/"7"`, 7, true},
		{"12", 12, true},
		{`"0"`, 0, false},
		{`"abc"`, 0, false},
		{"*", 0, false},
	}
	e := newEnv(t)
	for _, tt := range tests {
		r := e.request()
		r.Header.Set("If-Match", tt.header)
		got, err := ifMatch(r)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("If-Match %q: %d, %v", tt.header, got, err)
		}
	}
}

func TestReadPage(t *testing.T) {
	e := newEnv(t)
	for _, q := range []string{"limit=0", "limit=501", "limit=x"} {
		wantError(t, e.do(auth.RoleViewer, http.MethodGet, "/keys?"+q, nil), http.StatusBadRequest, "invalid", "limit")
	}
	for _, q := range []string{"cursor=***", "cursor="} {
		rec := e.do(auth.RoleViewer, http.MethodGet, "/audit?"+q, nil)
		if q == "cursor=" {
			if rec.Code != http.StatusOK {
				t.Fatalf("empty cursor: %d", rec.Code)
			}
			continue
		}
		wantError(t, rec, http.StatusBadRequest, "invalid", "cursor")
	}
}

func TestWriteLogsFailedReload(t *testing.T) {
	e := newEnv(t)
	e.reload.err = errors.New("bad config")
	rec := e.do(auth.RoleAdmin, http.MethodPost, "/keys", map[string]any{"name": "still-made"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d; body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(e.logs.String(), "bad config") {
		t.Fatalf("the failed reload was not logged: %s", e.logs)
	}
	if _, err := e.db.KeyByName(context.Background(), store.DefaultOrg, "still-made"); err != nil {
		t.Fatal(err)
	}
}

func TestNoReloader(t *testing.T) {
	e := newEnv(t)
	h := New(Deps{Auth: auth.NewAuthenticator(e.db, e.now), Store: e.db, Logger: e.handlerLogger(), Now: e.now})
	r := e.request()
	r.Method = http.MethodGet
	r.URL.Path = Prefix + "/status"
	r.Header.Set("Authorization", "Bearer "+e.keys[auth.RoleAdmin])
	rec := e.recorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
}
