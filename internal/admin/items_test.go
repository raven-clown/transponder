package admin

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/secrets"
	"github.com/raven-clown/transponder/internal/store"
)

func (e *env) create(t *testing.T, path string, body any) map[string]any {
	t.Helper()
	rec := e.do(auth.RoleAdmin, http.MethodPost, path, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST %s: status %d; body %s", path, rec.Code, rec.Body)
	}
	return decodeBody(t, rec)
}

func (e *env) auditCount(t *testing.T) int {
	t.Helper()
	recs, err := e.db.ListAudit(context.Background(), store.AuditQuery{OrgID: store.DefaultOrg, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	return len(recs)
}

func TestProviders(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	body := e.create(t, "/providers", map[string]any{"name": "openai", "type": "openai", "api_key": "sk-test-secret-1234"})
	key := body["api_key"].(map[string]any)
	if key["set"] != true || key["last4"] != "1234" || body["base_url"] != "" || body["managed_by_file"] != false {
		t.Fatalf("body %v", body)
	}
	it, err := e.db.ConfigItem(ctx, store.DefaultOrg, "providers", "openai")
	if err != nil || !strings.HasPrefix(it.Secret, "v1.") || strings.Contains(it.Secret+string(it.Body), "sk-test") {
		t.Fatalf("stored %+v, %v", it, err)
	}
	rc, err := Runtime(ctx, e.db, e.box, nil)
	if err != nil || len(rc.Providers) != 1 || rc.Providers[0].APIKey != "sk-test-secret-1234" {
		t.Fatalf("runtime %+v, %v", rc, err)
	}
	recs, _ := e.db.ListAudit(ctx, store.AuditQuery{OrgID: store.DefaultOrg, Limit: 10})
	if len(recs) != 1 || recs[0].Kind != "providers" || strings.Contains(string(recs[0].Diff), "sk-test") {
		t.Fatalf("audit %+v", recs)
	}

	wantError(t, e.do(auth.RoleAdmin, http.MethodPost, "/providers", map[string]any{"name": "openai", "type": "openai"}), http.StatusConflict, "already_exists", "name")
	for _, tt := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"name": "bad name", "type": "openai"}, "name"},
		{map[string]any{"name": "x", "type": "bedrock"}, "type"},
		{map[string]any{"name": "x", "type": "cloudflare"}, "base_url"},
		{map[string]any{"name": "x", "type": "openai", "base_url": "ftp://host"}, "base_url"},
		{map[string]any{"name": "x", "type": "openai", "region": "eu"}, "region"},
	} {
		wantError(t, e.do(auth.RoleAdmin, http.MethodPost, "/providers", tt.body), http.StatusBadRequest, "invalid", tt.field)
	}

	// A stored key does not follow the provider to another host unless it is sent again.
	for _, patch := range []map[string]any{{"base_url": "https://attacker.example.com/v1"}, {"type": "xai"}} {
		wantError(t, e.do(auth.RoleAdmin, http.MethodPatch, "/providers/openai", patch), http.StatusBadRequest, "invalid", "api_key")
	}
	if rec := e.do(auth.RoleAdmin, http.MethodPatch, "/providers/openai", map[string]any{"type": "openai", "base_url": ""}); rec.Code != http.StatusOK {
		t.Fatalf("an unchanged type and base_url: %d %s", rec.Code, rec.Body)
	}
	rec := e.do(auth.RoleAdmin, http.MethodPatch, "/providers/openai", map[string]any{"base_url": "https://proxy.example.com/v1", "api_key": "sk-test-secret-1234"}, "If-Match", `"2"`)
	body = decodeBody(t, rec)
	if rec.Code != http.StatusOK || body["base_url"] != "https://proxy.example.com/v1" || body["api_key"].(map[string]any)["set"] != true || body["version"] != float64(3) {
		t.Fatalf("patch %d %v", rec.Code, body)
	}
	wantError(t, e.do(auth.RoleAdmin, http.MethodPatch, "/providers/openai", map[string]any{"type": "anthropic"}, "If-Match", `"1"`), http.StatusConflict, "version_conflict", "If-Match")
	body = decodeBody(t, e.do(auth.RoleAdmin, http.MethodPatch, "/providers/openai", map[string]any{"api_key": nil, "type": "anthropic"}))
	if body["api_key"].(map[string]any)["set"] != false || body["type"] != "anthropic" {
		t.Fatalf("cleared key: %v", body)
	}
	body = decodeBody(t, e.do(auth.RoleAdmin, http.MethodPatch, "/providers/openai", map[string]any{"api_key": "abc"}))
	if k := body["api_key"].(map[string]any); k["set"] != true || k["last4"] != nil {
		t.Fatalf("short key: %v", body)
	}
	wantError(t, e.do(auth.RoleAdmin, http.MethodPatch, "/providers/openai", map[string]any{"api_key": 5}), http.StatusBadRequest, "invalid", "api_key")
	wantError(t, e.do(auth.RoleAdmin, http.MethodPatch, "/providers/openai", map[string]any{"type": "nope", "api_key": nil}), http.StatusBadRequest, "invalid", "type")
	wantError(t, e.do(auth.RoleAdmin, http.MethodPatch, "/providers/nobody", map[string]any{}), http.StatusNotFound, "not_found", "")

	get := e.do(auth.RoleViewer, http.MethodGet, "/providers/openai", nil)
	if get.Code != http.StatusOK || get.Header().Get("ETag") != `"5"` {
		t.Fatalf("get %d %q", get.Code, get.Header().Get("ETag"))
	}
	wantError(t, e.do(auth.RoleViewer, http.MethodGet, "/providers/nobody", nil), http.StatusNotFound, "not_found", "")
}

func TestProviderInUse(t *testing.T) {
	e := newEnv(t)
	e.create(t, "/providers", map[string]any{"name": "e", "type": "echo"})
	route := e.create(t, "/routes", map[string]any{"match": "*", "targets": []map[string]any{{"provider": "e"}}})
	rec := e.do(auth.RoleAdmin, http.MethodDelete, "/providers/e", nil)
	wantError(t, rec, http.StatusConflict, "in_use", "")
	if !strings.Contains(rec.Body.String(), route["id"].(string)) {
		t.Fatalf("the error does not name the route: %s", rec.Body)
	}
	if rec := e.do(auth.RoleAdmin, http.MethodDelete, "/routes/"+route["id"].(string), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete route %d", rec.Code)
	}
	if rec := e.do(auth.RoleAdmin, http.MethodDelete, "/providers/e", nil, "If-Match", `"1"`); rec.Code != http.StatusNoContent {
		t.Fatalf("delete provider %d %s", rec.Code, rec.Body)
	}
	wantError(t, e.do(auth.RoleAdmin, http.MethodDelete, "/providers/e", nil), http.StatusNotFound, "not_found", "")
	wantError(t, e.do(auth.RoleAdmin, http.MethodDelete, "/providers/e", nil, "If-Match", "x"), http.StatusBadRequest, "invalid", "If-Match")
}

func TestProviderKeyWithoutSecretsKey(t *testing.T) {
	e := newEnv(t).with(nil, nil, nil)
	e = e.with(e.db, nil, nil)
	wantError(t, e.do(auth.RoleAdmin, http.MethodPost, "/providers", map[string]any{"name": "o", "type": "openai", "api_key": "sk-1"}),
		http.StatusBadRequest, "secrets_key_missing", "api_key")
	e.create(t, "/providers", map[string]any{"name": "o", "type": "ollama"})
}

func TestStoredKeyNeedsTheSameSecretsKey(t *testing.T) {
	e := newEnv(t)
	e.create(t, "/providers", map[string]any{"name": "o", "type": "openai", "api_key": "sk-1234567"})
	other, _ := secrets.New(bytes.Repeat([]byte{2}, 32))
	e2 := e.with(e.db, nil, other)
	// Any write now fails validation: the stored key does not open, so the config would not load.
	wantError(t, e2.do(auth.RoleAdmin, http.MethodPost, "/budgets", map[string]any{"subject": "user:*", "period": "daily", "limit_usd": 1}),
		http.StatusBadRequest, "invalid_config", "")
	if _, err := Runtime(context.Background(), e.db, other, nil); err == nil {
		t.Fatal("a key sealed under another secrets_key opened")
	}
}

func TestResealAfterSecretsKeyChange(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.create(t, "/providers", map[string]any{"name": "o", "type": "openai", "api_key": "sk-1234567"})
	e.create(t, "/providers", map[string]any{"name": "local", "type": "ollama"})
	newBox, _ := secrets.New(bytes.Repeat([]byte{2}, 32))
	if _, err := Runtime(ctx, e.db, newBox, nil); !errors.Is(err, secrets.ErrWrongKey) {
		t.Fatalf("Runtime with the new key alone: %v, want ErrWrongKey", err)
	}
	both := newBox.WithPrevious(e.box)
	if rc, err := Runtime(ctx, e.db, both, nil); err != nil || rc.Providers[1].APIKey != "sk-1234567" {
		t.Fatalf("Runtime with the previous key: %+v, %v", rc, err)
	}
	before, _ := e.db.ConfigVersion(ctx)
	n, err := Reseal(ctx, e.db, both, func() time.Time { return testNow })
	if err != nil || n != 1 {
		t.Fatalf("Reseal = %d, %v; want 1", n, err)
	}
	if v, _ := e.db.ConfigVersion(ctx); v != before+1 {
		t.Fatalf("version %d, want %d so every replica reloads", v, before+1)
	}
	rc, err := Runtime(ctx, e.db, newBox, nil)
	if err != nil || rc.Providers[1].APIKey != "sk-1234567" {
		t.Fatalf("Runtime with the new key alone after Reseal: %+v, %v", rc, err)
	}
	recs, _ := e.db.ListAudit(ctx, store.AuditQuery{OrgID: store.DefaultOrg, Actor: ActorCLI, Limit: 10})
	if len(recs) != 1 || recs[0].Action != actionRotate || recs[0].Kind != kindProviders || recs[0].Resource != "o" || strings.Contains(string(recs[0].Diff), "sk-") {
		t.Fatalf("audit %+v", recs)
	}
	// Running it again changes nothing.
	if n, err := Reseal(ctx, e.db, both, time.Now); err != nil || n != 0 {
		t.Fatalf("second Reseal = %d, %v", n, err)
	}
	if v, _ := e.db.ConfigVersion(ctx); v != before+1 {
		t.Fatalf("an empty reseal raised the version to %d", v)
	}
	// Without the key the values were sealed under, nothing is written.
	third, _ := secrets.New(bytes.Repeat([]byte{3}, 32))
	if _, err := Reseal(ctx, e.db, third, time.Now); !errors.Is(err, secrets.ErrWrongKey) || !strings.Contains(err.Error(), `"o"`) {
		t.Fatalf("Reseal with an unrelated key: %v", err)
	}
	if v, _ := e.db.ConfigVersion(ctx); v != before+1 {
		t.Fatalf("a failed reseal raised the version to %d", v)
	}
}

func TestRoutes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	wantError(t, e.do(auth.RoleAdmin, http.MethodPost, "/routes", map[string]any{"match": "*", "targets": []map[string]any{{"provider": "ghost"}}}),
		http.StatusBadRequest, "invalid", "targets[0].provider")
	e.create(t, "/providers", map[string]any{"name": "e", "type": "echo"})
	first := e.create(t, "/routes", map[string]any{"match": "a-*", "targets": []map[string]any{{"provider": "e", "model": "up"}}, "allow": []string{"team:ml"}})
	second := e.create(t, "/routes", map[string]any{"match": "b-*", "targets": []map[string]any{{"provider": "e"}}})
	middle := e.create(t, "/routes", map[string]any{"match": "m-*", "targets": []map[string]any{{"provider": "e"}}, "position": 1500})
	if !strings.HasPrefix(first["id"].(string), "rt_") || first["position"] != float64(1000) || second["position"] != float64(2000) || middle["position"] != float64(1500) {
		t.Fatalf("routes %v %v %v", first, second, middle)
	}
	if a := first["allow"].([]any); len(a) != 1 || len(second["allow"].([]any)) != 0 {
		t.Fatalf("allow %v %v", first["allow"], second["allow"])
	}
	var matches []string
	cursor := ""
	for range 5 {
		path := "/routes?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		page := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, path, nil))
		for _, it := range page["items"].([]any) {
			matches = append(matches, it.(map[string]any)["match"].(string))
		}
		next, _ := page["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if got := strings.Join(matches, ","); got != "a-*,m-*,b-*" {
		t.Fatalf("order %s", got)
	}
	rc, err := Runtime(ctx, e.db, e.box, nil)
	if err != nil || len(rc.Routes) != 3 || rc.Routes[1].Match != "m-*" || rc.Routes[0].Targets[0].Model != "up" || rc.Routes[0].Allow[0] != "team:ml" {
		t.Fatalf("runtime %+v, %v", rc, err)
	}

	for _, tt := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"targets": []map[string]any{{"provider": "e"}}}, "match"},
		{map[string]any{"match": "x"}, "targets"},
		{map[string]any{"match": "x", "targets": []map[string]any{{"model": "m"}}}, "targets[0].provider"},
		{map[string]any{"match": "x", "targets": []map[string]any{{"provider": "e"}}, "allow": []string{"group:x"}}, "allow"},
		{map[string]any{"match": "x", "targets": []map[string]any{{"provider": "e"}}, "position": -1}, "position"},
	} {
		wantError(t, e.do(auth.RoleAdmin, http.MethodPost, "/routes", tt.body), http.StatusBadRequest, "invalid", tt.field)
	}

	id := second["id"].(string)
	body := decodeBody(t, e.do(auth.RoleAdmin, http.MethodPatch, "/routes/"+id, map[string]any{"position": 0, "allow": []string{}}, "If-Match", `"1"`))
	if body["position"] != float64(0) || body["match"] != "b-*" || body["version"] != float64(2) {
		t.Fatalf("patch %v", body)
	}
	wantError(t, e.do(auth.RoleAdmin, http.MethodPatch, "/routes/"+id, map[string]any{"targets": []map[string]any{{"provider": "ghost"}}}), http.StatusBadRequest, "invalid", "targets[0].provider")
	wantError(t, e.do(auth.RoleAdmin, http.MethodPatch, "/routes/"+id, map[string]any{"match": ""}), http.StatusBadRequest, "invalid", "match")
	if rec := e.do(auth.RoleViewer, http.MethodGet, "/routes/"+id, nil); rec.Code != http.StatusOK || decodeBody(t, rec)["position"] != float64(0) {
		t.Fatalf("get %d", rec.Code)
	}
}

func TestBudgets(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	body := e.create(t, "/budgets", map[string]any{"subject": "user:*", "period": "monthly", "limit_usd": 20.5})
	id := body["id"].(string)
	if !strings.HasPrefix(id, "bg_") || body["limit_usd"] != 20.5 || body["period"] != "monthly" {
		t.Fatalf("body %v", body)
	}
	rc, err := Runtime(ctx, e.db, e.box, nil)
	if err != nil || len(rc.Budgets) != 1 || rc.Budgets[0].LimitUSD != 20.5 {
		t.Fatalf("runtime %+v, %v", rc, err)
	}
	for _, tt := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"subject": "user:*", "period": "weekly", "limit_usd": 1}, "period"},
		{map[string]any{"subject": "user:*", "period": "daily", "limit_usd": -1}, "limit_usd"},
		{map[string]any{"subject": "user:*", "period": "daily"}, "limit_usd"},
		{map[string]any{"subject": "team:*", "period": "daily", "limit_usd": 1}, "subject"},
		{map[string]any{"subject": "nobody", "period": "daily", "limit_usd": 1}, "subject"},
	} {
		wantError(t, e.do(auth.RoleAdmin, http.MethodPost, "/budgets", tt.body), http.StatusBadRequest, "invalid", tt.field)
	}
	body = decodeBody(t, e.do(auth.RoleAdmin, http.MethodPatch, "/budgets/"+id, map[string]any{"limit_usd": 50, "period": "daily"}))
	if body["limit_usd"] != float64(50) || body["period"] != "daily" || body["subject"] != "user:*" {
		t.Fatalf("patch %v", body)
	}
	wantError(t, e.do(auth.RoleAdmin, http.MethodPatch, "/budgets/"+id, map[string]any{"subject": "x"}), http.StatusBadRequest, "invalid", "subject")
	if rec := e.do(auth.RoleAdmin, http.MethodDelete, "/budgets/"+id, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete %d", rec.Code)
	}
	if list := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/budgets", nil)); len(list["items"].([]any)) != 0 {
		t.Fatalf("list %v", list)
	}
}

func TestProfiles(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	body := e.create(t, "/profiles", map[string]any{
		"name": "gold", "priority": 2, "default": true, "subjects": []string{"team:ml"},
		"limits": map[string]any{"llm.tokens": map[string]any{"per_day": 1000, "rate": "100/m"}, "llm.cost_usd": map[string]any{"per_day": 2.5}},
	})
	limitsOut := body["limits"].(map[string]any)
	if body["default"] != true || body["priority"] != float64(2) || limitsOut["llm.tokens"].(map[string]any)["per_day"] != float64(1000) {
		t.Fatalf("body %v", body)
	}
	rc, err := Runtime(ctx, e.db, e.box, nil)
	if err != nil || rc.DefaultProfile != "gold" || len(rc.ProfileBindings) != 1 || rc.ProfileBindings[0].Subject != "team:ml" {
		t.Fatalf("runtime %+v, %v", rc, err)
	}
	lc, err := rc.LimitsConfig()
	if err != nil || len(lc.Profiles) != 1 || lc.Profiles[0].TokensPerDay != 1000 || lc.Profiles[0].USDPerDay != 2_500_000_000 {
		t.Fatalf("limits %+v, %v", lc, err)
	}

	for _, tt := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"name": "a b"}, "name"},
		{map[string]any{"name": "x", "limits": map[string]any{"llm.wishes": 3}}, "limits"},
		{map[string]any{"name": "x", "limits": []int{1}}, "limits"},
		{map[string]any{"name": "x", "subjects": []string{"group:a"}}, "subjects"},
		{map[string]any{"name": "x", "subjects": []string{"user:a", "user:a"}}, "subjects"},
	} {
		wantError(t, e.do(auth.RoleAdmin, http.MethodPost, "/profiles", tt.body), http.StatusBadRequest, "invalid", tt.field)
	}
	empty := e.create(t, "/profiles", map[string]any{"name": "plain"})
	if len(empty["limits"].(map[string]any)) != 0 || len(empty["subjects"].([]any)) != 0 || empty["default"] != false {
		t.Fatalf("empty profile %v", empty)
	}

	body = decodeBody(t, e.do(auth.RoleAdmin, http.MethodPatch, "/profiles/gold", map[string]any{"limits": nil, "subjects": []string{}, "priority": 1}))
	if len(body["limits"].(map[string]any)) != 0 || len(body["subjects"].([]any)) != 0 || body["priority"] != float64(1) || body["default"] != true {
		t.Fatalf("patch %v", body)
	}
	wantError(t, e.do(auth.RoleAdmin, http.MethodPatch, "/profiles/gold", map[string]any{"name": "platinum"}), http.StatusBadRequest, "invalid", "name")
	wantError(t, e.do(auth.RoleAdmin, http.MethodPatch, "/profiles/gold", map[string]any{"limits": map[string]any{"requests": "lots"}}), http.StatusBadRequest, "invalid", "limits")
}

// A change that is valid alone but leaves a config that does not load is refused, and nothing
// of it is written: no item, no audit record, no new config version.
func TestInvalidConfigRollsBack(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.create(t, "/profiles", map[string]any{"name": "a", "default": true, "subjects": []string{"user:x"}})
	version, _ := e.db.ConfigVersion(ctx)
	audits := e.auditCount(t)
	reloads := e.reload.count()

	rec := e.do(auth.RoleAdmin, http.MethodPost, "/profiles", map[string]any{"name": "b", "default": true})
	wantError(t, rec, http.StatusBadRequest, "invalid_config", "")
	if !strings.Contains(rec.Body.String(), "both the default") {
		t.Fatalf("message %s", rec.Body)
	}
	wantError(t, e.do(auth.RoleAdmin, http.MethodPost, "/profiles", map[string]any{"name": "c", "subjects": []string{"user:x"}}), http.StatusBadRequest, "invalid_config", "")
	e.create(t, "/profiles", map[string]any{"name": "d"})
	wantError(t, e.do(auth.RoleAdmin, http.MethodPatch, "/profiles/d", map[string]any{"default": true}), http.StatusBadRequest, "invalid_config", "")

	if _, err := e.db.ConfigItem(ctx, store.DefaultOrg, "profiles", "b"); err == nil {
		t.Fatal("the refused profile was stored")
	}
	if v, _ := e.db.ConfigVersion(ctx); v != version+1 {
		t.Fatalf("config version %d, want %d (only profile d)", v, version+1)
	}
	if n := e.auditCount(t); n != audits+1 {
		t.Fatalf("audit records %d, want %d", n, audits+1)
	}
	if e.reload.count() != reloads+1 {
		t.Fatalf("reloads %d, want %d", e.reload.count(), reloads+1)
	}
}

func TestItemStoreErrors(t *testing.T) {
	base := newEnv(t)
	e := base.on(failingItems{failing{Store: base.db, err: errStore}})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/providers"},
		{http.MethodGet, "/providers/x"},
		{http.MethodPost, "/budgets"},
		{http.MethodPatch, "/routes/x"},
		{http.MethodDelete, "/profiles/x"},
	} {
		var body any
		switch tc.method {
		case http.MethodPost:
			body = map[string]any{"subject": "user:*", "period": "daily", "limit_usd": 1}
		case http.MethodPatch:
			body = map[string]any{}
		}
		wantError(t, e.raw(auth.RoleAdmin, tc.method, tc.path, body), http.StatusInternalServerError, "internal", "")
	}
	for _, path := range []string{"/routes", "/profiles"} {
		wantError(t, e.raw(auth.RoleAdmin, http.MethodPost, path, "{"), http.StatusBadRequest, "invalid", "")
	}
	wantError(t, e.raw(auth.RoleAdmin, http.MethodPost, "/routes", `{"match":"`+strings.Repeat("a", 2<<20)+`"}`), http.StatusRequestEntityTooLarge, "body_too_large", "")
	wantError(t, e.raw(auth.RoleAdmin, http.MethodPatch, "/routes/x", `{"match":"`+strings.Repeat("a", 2<<20)+`"}`), http.StatusRequestEntityTooLarge, "body_too_large", "")
}

type failingItems struct{ failing }

func (f failingItems) ListConfigItems(context.Context, string, string) ([]store.ConfigItem, error) {
	return nil, f.err
}
func (f failingItems) ConfigItem(context.Context, string, string, string) (store.ConfigItem, error) {
	return store.ConfigItem{}, f.err
}

// Items with a body the API cannot read answer 500 rather than a half view.
func TestBrokenItemBody(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, kind := range runtimeKinds {
		it := store.ConfigItem{OrgID: store.DefaultOrg, Kind: kind, ID: "broken", Body: []byte(`[1]`), CreatedAt: testNow}
		if err := e.db.CreateConfigItem(ctx, it); err != nil {
			t.Fatal(err)
		}
		wantError(t, e.raw(auth.RoleViewer, http.MethodGet, "/"+kind+"/broken", nil), http.StatusInternalServerError, "internal", "")
		wantError(t, e.raw(auth.RoleViewer, http.MethodGet, "/"+kind, nil), http.StatusInternalServerError, "internal", "")
		wantError(t, e.raw(auth.RoleAdmin, http.MethodPatch, "/"+kind+"/broken", map[string]any{}), http.StatusInternalServerError, "internal", "")
		wantError(t, e.raw(auth.RoleAdmin, http.MethodDelete, "/"+kind+"/broken", nil), http.StatusInternalServerError, "internal", "")
		if _, err := Runtime(ctx, e.db, e.box, nil); err == nil {
			t.Fatalf("%s: a broken body loaded", kind)
		}
		if err := e.db.DeleteConfigItem(ctx, store.DefaultOrg, kind, "broken", 0); err != nil {
			t.Fatal(err)
		}
	}
}
