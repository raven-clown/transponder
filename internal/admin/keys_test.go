package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/store"
)

func TestCreateKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	before, _ := e.db.ConfigVersion(ctx)
	rec := e.do(auth.RoleAdmin, http.MethodPost, "/keys", map[string]any{
		"name": "logbook", "role": "viewer", "teams": []string{"platform"}, "expires_at": "2027-01-01T00:00:00Z",
	})
	if rec.Code != http.StatusCreated || rec.Header().Get("ETag") != `"1"` {
		t.Fatalf("status %d, etag %q; body %s", rec.Code, rec.Header().Get("ETag"), rec.Body)
	}
	body := decodeBody(t, rec)
	secret, _ := body["secret"].(string)
	if !strings.HasPrefix(secret, "tp_") || body["last4"] != secret[len(secret)-4:] {
		t.Fatalf("secret %q, last4 %v", secret, body["last4"])
	}
	if body["role"] != "viewer" || body["expires_at"] != "2027-01-01T00:00:00Z" || body["managed_by_file"] != false || body["source"] != "api" {
		t.Fatalf("body %v", body)
	}
	k, err := e.db.KeyByHash(ctx, auth.HashKey(secret))
	if err != nil || k.Name != "logbook" || k.Role != "viewer" || len(k.Teams) != 1 {
		t.Fatalf("stored %+v, %v", k, err)
	}
	if after, _ := e.db.ConfigVersion(ctx); after != before+1 {
		t.Fatalf("config version %d, want %d", after, before+1)
	}
	if e.reload.count() != 1 {
		t.Fatalf("reloads %d, want 1", e.reload.count())
	}
	// The write has committed, so its reload must not stop when the caller goes away.
	if e.reload.cancellable != 0 {
		t.Fatalf("%d reloads ran on the caller's context", e.reload.cancellable)
	}
	recs, err := e.db.ListAudit(ctx, store.AuditQuery{OrgID: store.DefaultOrg, Limit: 10})
	if err != nil || len(recs) != 1 {
		t.Fatalf("audit %+v, %v", recs, err)
	}
	a := recs[0]
	if a.Actor != "key:the-admin" || a.Action != "create" || a.Kind != "keys" || a.Resource != "logbook" || a.RequestID != "req-test" || !a.Time.Equal(testNow) {
		t.Fatalf("audit %+v", a)
	}
	if strings.Contains(string(a.Diff), secret) || strings.Contains(string(a.Diff), "secret") {
		t.Fatalf("the audit diff carries the secret: %s", a.Diff)
	}
	var diff struct{ Before, After map[string]any }
	if err := json.Unmarshal(a.Diff, &diff); err != nil || diff.Before != nil || diff.After["name"] != "logbook" {
		t.Fatalf("diff %s, %v", a.Diff, err)
	}

	wantError(t, e.do(auth.RoleAdmin, http.MethodPost, "/keys", map[string]any{"name": "logbook"}), http.StatusConflict, "already_exists", "name")
	if recs, _ := e.db.ListAudit(ctx, store.AuditQuery{OrgID: store.DefaultOrg, Limit: 10}); len(recs) != 1 {
		t.Fatalf("a refused write left an audit record: %+v", recs)
	}
}

func TestCreateKeyDefaults(t *testing.T) {
	e := newEnv(t)
	body := decodeBody(t, e.do(auth.RoleAdmin, http.MethodPost, "/keys", map[string]any{"name": "plain", "expires_at": nil}))
	if body["role"] != "member" || body["expires_at"] != nil || len(body["teams"].([]any)) != 0 || body["disabled"] != false {
		t.Fatalf("body %v", body)
	}
}

func TestCreateKeyInvalid(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name  string
		body  any
		code  string
		field string
	}{
		{"no name", map[string]any{}, "invalid", "name"},
		{"bad name", map[string]any{"name": "has space"}, "invalid", "name"},
		{"long name", map[string]any{"name": strings.Repeat("a", 65)}, "invalid", "name"},
		{"bad role", map[string]any{"name": "a", "role": "owner"}, "invalid", "role"},
		{"bad expiry", map[string]any{"name": "a", "expires_at": "tomorrow"}, "invalid", "expires_at"},
		{"expiry not a string", map[string]any{"name": "a", "expires_at": 5}, "invalid", "expires_at"},
		{"past expiry", map[string]any{"name": "a", "expires_at": "2026-11-20T11:00:00Z"}, "invalid", "expires_at"},
		{"expiry at now", map[string]any{"name": "a", "expires_at": "2026-11-20T12:00:00Z"}, "invalid", "expires_at"},
		{"zero expiry", map[string]any{"name": "a", "expires_at": "0001-01-01T00:00:00Z"}, "invalid", "expires_at"},
		{"empty team", map[string]any{"name": "a", "teams": []string{""}}, "invalid", "teams"},
		{"unknown field", map[string]any{"name": "a", "owner": "me"}, "invalid", "owner"},
		{"wrong type", map[string]any{"name": 5}, "invalid", "name"},
		{"teams wrong type", map[string]any{"name": "a", "teams": "x"}, "invalid", "teams"},
		{"not json", "{", "invalid", ""},
		{"empty", "", "invalid", ""},
		{"two objects", `{"name":"a"} {}`, "invalid", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantError(t, e.do(auth.RoleAdmin, http.MethodPost, "/keys", tt.body), http.StatusBadRequest, tt.code, tt.field)
		})
	}
	rec := e.raw(auth.RoleAdmin, http.MethodPost, "/keys", `{"name":"`+strings.Repeat("a", 2<<20)+`"}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("huge body: status %d", rec.Code)
	}
}

func TestCreateKeyPastExpiry(t *testing.T) {
	e := newEnv(t)
	wantError(t, e.do(auth.RoleAdmin, http.MethodPost, "/keys",
		map[string]any{"name": "stale", "expires_at": "2026-11-19T00:00:00Z"}), http.StatusBadRequest, "invalid", "expires_at")

	before, _ := e.db.ConfigVersion(context.Background())
	e.do(auth.RoleAdmin, http.MethodPost, "/keys",
		map[string]any{"name": "fresh", "role": "member", "teams": []string{}, "expires_at": "2026-11-21T00:00:00Z"})
	if after, _ := e.db.ConfigVersion(context.Background()); after != before+1 {
		t.Fatalf("config version %d, want %d", after, before+1)
	}
}

func TestPatchKeyPastExpiry(t *testing.T) {
	e := newEnv(t)
	e.do(auth.RoleAdmin, http.MethodPost, "/keys", map[string]any{"name": "k", "role": "viewer"})
	body := decodeBody(t, e.do(auth.RoleAdmin, http.MethodPatch, "/keys/k",
		map[string]any{"expires_at": "2026-11-19T00:00:00Z"}, "If-Match", `"1"`))
	if body["expires_at"] != "2026-11-19T00:00:00Z" {
		t.Fatalf("body %v", body)
	}
}

func TestListKeys(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"b", "a", "c"} {
		if rec := e.do(auth.RoleAdmin, http.MethodPost, "/keys", map[string]any{"name": name}); rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d", name, rec.Code)
		}
	}
	var names []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("too many pages")
		}
		path := "/keys?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		body := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, path, nil))
		for _, it := range body["items"].([]any) {
			k := it.(map[string]any)
			if _, has := k["secret"]; has {
				t.Fatal("a list shows a secret")
			}
			names = append(names, k["name"].(string))
		}
		next, _ := body["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if got := strings.Join(names, ","); got != "a,b,c,the-admin,the-member,the-viewer" {
		t.Fatalf("names %s", got)
	}
	body := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/keys?cursor="+cursorOf("zzz"), nil))
	if items := body["items"].([]any); len(items) != 0 {
		t.Fatalf("items after the end %v", items)
	}
}

func cursorOf(s string) string { return cursor(s) }

func TestGetKey(t *testing.T) {
	e := newEnv(t)
	rec := e.do(auth.RoleViewer, http.MethodGet, "/keys/the-member", nil)
	body := decodeBody(t, rec)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"1"` || body["name"] != "the-member" || body["role"] != "member" {
		t.Fatalf("status %d, etag %q, body %v", rec.Code, rec.Header().Get("ETag"), body)
	}
	wantError(t, e.do(auth.RoleViewer, http.MethodGet, "/keys/nobody", nil), http.StatusNotFound, "not_found", "")
}

func TestUpdateKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.do(auth.RoleAdmin, http.MethodPost, "/keys", map[string]any{"name": "k", "teams": []string{"a"}, "expires_at": "2027-01-01T00:00:00Z"})

	rec := e.do(auth.RoleAdmin, http.MethodPatch, "/keys/k", map[string]any{"role": "admin", "disabled": true}, "If-Match", `"1"`)
	body := decodeBody(t, rec)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"2"` || body["role"] != "admin" || body["disabled"] != true ||
		body["expires_at"] != "2027-01-01T00:00:00Z" || len(body["teams"].([]any)) != 1 {
		t.Fatalf("status %d, body %v", rec.Code, body)
	}
	wantError(t, e.do(auth.RoleAdmin, http.MethodPatch, "/keys/k", map[string]any{"disabled": false}, "If-Match", `"1"`), http.StatusConflict, "version_conflict", "If-Match")

	body = decodeBody(t, e.do(auth.RoleAdmin, http.MethodPatch, "/keys/k", map[string]any{"expires_at": nil, "teams": []string{}}))
	if body["expires_at"] != nil || len(body["teams"].([]any)) != 0 || body["version"] != float64(3) {
		t.Fatalf("body %v", body)
	}

	recs, _ := e.db.ListAudit(ctx, store.AuditQuery{OrgID: store.DefaultOrg, Kind: "keys", Resource: "k", Limit: 10})
	if len(recs) != 3 || recs[0].Action != "update" {
		t.Fatalf("audit %+v", recs)
	}
	var diff struct{ Before, After map[string]any }
	_ = json.Unmarshal(recs[1].Diff, &diff)
	if diff.Before["role"] != "member" || diff.After["role"] != "admin" {
		t.Fatalf("diff %s", recs[1].Diff)
	}

	for _, tt := range []struct {
		body  any
		field string
	}{
		{map[string]any{"role": "boss"}, "role"},
		{map[string]any{"role": ""}, "role"},
		{map[string]any{"teams": []string{""}}, "teams"},
		{map[string]any{"expires_at": "soon"}, "expires_at"},
		{map[string]any{"name": "renamed"}, "name"},
	} {
		wantError(t, e.do(auth.RoleAdmin, http.MethodPatch, "/keys/k", tt.body), http.StatusBadRequest, "invalid", tt.field)
	}
	wantError(t, e.do(auth.RoleAdmin, http.MethodPatch, "/keys/k", map[string]any{}, "If-Match", "abc"), http.StatusBadRequest, "invalid", "If-Match")
	wantError(t, e.do(auth.RoleAdmin, http.MethodPatch, "/keys/nobody", map[string]any{}), http.StatusNotFound, "not_found", "")
}

func TestRotateKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	old := decodeBody(t, e.do(auth.RoleAdmin, http.MethodPost, "/keys", map[string]any{"name": "k"}))["secret"].(string)

	wantError(t, e.do(auth.RoleAdmin, http.MethodPost, "/keys/k/rotate", nil, "If-Match", `"5"`), http.StatusConflict, "version_conflict", "If-Match")
	rec := e.do(auth.RoleAdmin, http.MethodPost, "/keys/k/rotate", nil, "If-Match", `"1"`)
	body := decodeBody(t, rec)
	secret, _ := body["secret"].(string)
	if rec.Code != http.StatusOK || secret == "" || secret == old || body["version"] != float64(2) {
		t.Fatalf("status %d, body %v", rec.Code, body)
	}
	if _, err := e.db.KeyByHash(ctx, auth.HashKey(old)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the old secret still works: %v", err)
	}
	if k, err := e.db.KeyByHash(ctx, auth.HashKey(secret)); err != nil || k.Name != "k" {
		t.Fatalf("the new secret: %+v, %v", k, err)
	}
	recs, _ := e.db.ListAudit(ctx, store.AuditQuery{OrgID: store.DefaultOrg, Limit: 1})
	if recs[0].Action != "rotate" || strings.Contains(string(recs[0].Diff), secret) {
		t.Fatalf("audit %+v", recs[0])
	}
	wantError(t, e.do(auth.RoleAdmin, http.MethodPost, "/keys/nobody/rotate", nil), http.StatusNotFound, "not_found", "")
	wantError(t, e.do(auth.RoleAdmin, http.MethodPost, "/keys/k/rotate", nil, "If-Match", "x"), http.StatusBadRequest, "invalid", "If-Match")
}

func TestDeleteKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.do(auth.RoleAdmin, http.MethodPost, "/keys", map[string]any{"name": "k"})
	wantError(t, e.do(auth.RoleAdmin, http.MethodDelete, "/keys/k", nil, "If-Match", `"2"`), http.StatusConflict, "version_conflict", "If-Match")
	if rec := e.do(auth.RoleAdmin, http.MethodDelete, "/keys/k", nil, "If-Match", `"1"`); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d; body %s", rec.Code, rec.Body)
	}
	if _, err := e.db.KeyByName(ctx, store.DefaultOrg, "k"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	wantError(t, e.do(auth.RoleAdmin, http.MethodDelete, "/keys/k", nil), http.StatusNotFound, "not_found", "")
	wantError(t, e.do(auth.RoleAdmin, http.MethodDelete, "/keys/k", nil, "If-Match", "-1"), http.StatusBadRequest, "invalid", "If-Match")
	recs, _ := e.db.ListAudit(ctx, store.AuditQuery{OrgID: store.DefaultOrg, Limit: 1})
	var diff struct{ Before, After map[string]any }
	_ = json.Unmarshal(recs[0].Diff, &diff)
	if recs[0].Action != "delete" || diff.Before["name"] != "k" || diff.After != nil {
		t.Fatalf("audit %+v %s", recs[0], recs[0].Diff)
	}
}

func TestKeyFromFileIsReadOnly(t *testing.T) {
	e := newEnv(t)
	plain, hash, last4, _ := auth.NewKey()
	_ = plain
	if err := e.db.CreateKey(context.Background(), store.APIKey{OrgID: store.DefaultOrg, Name: "gitops", Hash: hash, Last4: last4, Role: "member", Source: store.SourceFile, CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	if body := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/keys/gitops", nil)); body["managed_by_file"] != true || body["source"] != "file" {
		t.Fatalf("body %v", body)
	}
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, "/keys/gitops", map[string]any{"disabled": true}},
		{http.MethodDelete, "/keys/gitops", nil},
		{http.MethodPost, "/keys/gitops/rotate", nil},
	} {
		wantError(t, e.do(auth.RoleAdmin, tc.method, tc.path, tc.body), http.StatusConflict, "managed_by_file", "")
	}
}

func TestKeyWithUnknownRoleShowsMember(t *testing.T) {
	e := newEnv(t)
	_, hash, last4, _ := auth.NewKey()
	if err := e.db.CreateKey(context.Background(), store.APIKey{OrgID: store.DefaultOrg, Name: "odd", Hash: hash, Last4: last4, Role: "owner", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	if body := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/keys/odd", nil)); body["role"] != "member" {
		t.Fatalf("body %v", body)
	}
}
