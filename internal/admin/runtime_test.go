package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/store"
)

func fileConfig() *config.Bootstrap {
	return &config.Bootstrap{
		Providers: []config.Provider{
			{Name: "oa", Type: "openai", APIKey: "sk-from-file-9876"},
			{Name: "local", Type: "ollama"},
		},
		Routes: []config.Route{
			{Match: "gpt-*", Allow: []string{"team:ml"}, Targets: []config.Target{{Provider: "oa"}}},
			{Match: "*", Targets: []config.Target{{Provider: "local", Model: "llama3"}}},
		},
		Budgets:         []config.BudgetConfig{{Subject: "user:*", Period: "monthly", LimitUSD: 20}},
		Profiles:        []config.ProfileConfig{{Name: "std", Limits: map[string]any{"llm.max_output_tokens": 2048}}},
		DefaultProfile:  "std",
		ProfileBindings: []config.BindingConfig{{Subject: "team:ml", Profile: "std"}},
	}
}

func (e *env) sync(t *testing.T, cfg *config.Bootstrap) {
	t.Helper()
	if err := SyncFile(context.Background(), e.db, cfg, func() time.Time { return testNow }, slog.New(slog.NewJSONHandler(e.logs, nil))); err != nil {
		t.Fatal(err)
	}
}

func TestSyncFile(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cfg := fileConfig()
	e = e.with(e.db, cfg, e.box)
	v0, _ := e.db.ConfigVersion(ctx)
	e.sync(t, cfg)
	if v, _ := e.db.ConfigVersion(ctx); v != v0+1 {
		t.Fatalf("version %d, want one raise from %d", v, v0)
	}
	recs, _ := e.db.ListAudit(ctx, store.AuditQuery{OrgID: store.DefaultOrg, Actor: "file", Limit: 50})
	if len(recs) != 6 {
		t.Fatalf("%d audit records, want 6", len(recs))
	}
	for _, r := range recs {
		if strings.Contains(string(r.Diff), "sk-from-file") {
			t.Fatalf("an audit diff holds the file's key: %s", r.Diff)
		}
	}

	// The same file again changes nothing.
	e.sync(t, cfg)
	if v, _ := e.db.ConfigVersion(ctx); v != v0+1 {
		t.Fatalf("an unchanged file raised the version to %d", v)
	}

	rc, err := Runtime(ctx, e.db, e.box, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Providers) != 2 || rc.Providers[1].Name != "oa" || rc.Providers[1].APIKey != "sk-from-file-9876" ||
		len(rc.Routes) != 2 || rc.Routes[0].Match != "gpt-*" || rc.Routes[1].Targets[0].Model != "llama3" ||
		len(rc.Budgets) != 1 || rc.DefaultProfile != "std" || len(rc.ProfileBindings) != 1 {
		t.Fatalf("runtime %+v", rc)
	}
	if lc, err := rc.LimitsConfig(); err != nil || lc.Profiles[0].MaxOutputTokens != 2048 {
		t.Fatalf("limits %+v, %v", lc, err)
	}

	// The API shows them as read-only.
	p := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/providers/oa", nil))
	if p["managed_by_file"] != true || p["source"] != "file" || p["api_key"].(map[string]any)["last4"] != "9876" {
		t.Fatalf("provider %v", p)
	}
	if l := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/providers/local", nil)); l["api_key"].(map[string]any)["set"] != false {
		t.Fatalf("provider without a key %v", l)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPatch, "/providers/oa"},
		{http.MethodDelete, "/providers/oa"},
		{http.MethodPatch, "/routes/file-001"},
		{http.MethodDelete, "/budgets/file-001"},
		{http.MethodPatch, "/profiles/std"},
	} {
		var body any
		if tc.method == http.MethodPatch {
			body = map[string]any{}
		}
		wantError(t, e.do(auth.RoleAdmin, tc.method, tc.path, body), http.StatusConflict, "managed_by_file", "")
	}
	// An API route can go before the file's routes and use the file's providers.
	r := e.create(t, "/routes", map[string]any{"match": "special", "targets": []map[string]any{{"provider": "oa"}}, "position": 10})
	if r["position"] != float64(10) {
		t.Fatalf("route %v", r)
	}

	// An API item the file now declares is taken over; file items the file dropped go.
	e.create(t, "/providers", map[string]any{"name": "extra", "type": "echo"})
	next := fileConfig()
	next.Providers = append(next.Providers, config.Provider{Name: "extra", Type: "ollama", BaseURL: "http://gpu:11434"})
	next.Routes = next.Routes[:1]
	next.Budgets = nil
	e.sync(t, next)
	extra, err := e.db.ConfigItem(ctx, store.DefaultOrg, "providers", "extra")
	if err != nil || extra.Source != store.SourceFile || !strings.Contains(string(extra.Body), "gpu:11434") {
		t.Fatalf("taken over %+v, %v", extra, err)
	}
	if !strings.Contains(e.logs.String(), "replaces one made through the admin API") {
		t.Fatalf("the take-over was not logged: %s", e.logs)
	}
	routes, _ := e.db.ListConfigItems(ctx, store.DefaultOrg, "routes")
	if len(routes) != 2 || routes[0].Source != store.SourceAPI || routes[1].ID != "file-001" {
		t.Fatalf("routes %+v", routes)
	}
	if budgets, _ := e.db.ListConfigItems(ctx, store.DefaultOrg, "budgets"); len(budgets) != 0 {
		t.Fatalf("budgets %+v", budgets)
	}
}

func TestSyncFileStoreErrors(t *testing.T) {
	e := newEnv(t)
	broken := failingItems{failing{Store: e.db, err: errStore}}
	if err := SyncFile(context.Background(), broken, fileConfig(), time.Now, slog.New(slog.DiscardHandler)); !errors.Is(err, errStore) {
		t.Fatalf("err = %v", err)
	}
	if _, err := Runtime(context.Background(), broken, nil, nil); !errors.Is(err, errStore) {
		t.Fatalf("Runtime err = %v", err)
	}
	if err := SyncFile(context.Background(), conflicting{e.db}, fileConfig(), time.Now, slog.New(slog.DiscardHandler)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a write that keeps conflicting: err = %v", err)
	}
}

// conflicting fails every config write as if another replica got there first.
type conflicting struct{ Store }

func (c conflicting) CreateConfigItem(context.Context, store.ConfigItem) error {
	return store.ErrConflict
}

func TestRuntimeRefuses(t *testing.T) {
	ctx := context.Background()
	item := func(kind, id, body string) store.ConfigItem {
		return store.ConfigItem{OrgID: store.DefaultOrg, Kind: kind, ID: id, Body: []byte(body), CreatedAt: testNow}
	}
	tests := []struct {
		name  string
		items []store.ConfigItem
		want  string
	}{
		{"two defaults", []store.ConfigItem{item("profiles", "a", `{"default":true}`), item("profiles", "b", `{"default":true}`)}, "both the default"},
		{"one subject, two profiles", []store.ConfigItem{item("profiles", "a", `{"subjects":["user:x"]}`), item("profiles", "b", `{"subjects":["user:x"]}`)}, "bound to both"},
		{"route to nothing", []store.ConfigItem{item("routes", "r", `{"match":"*","targets":[{"provider":"ghost"}]}`)}, "not declared"},
		{"unknown type", []store.ConfigItem{item("providers", "p", `{"type":"bedrock"}`)}, "unknown type"},
		{"sealed key without a secrets key", []store.ConfigItem{func() store.ConfigItem {
			it := item("providers", "p", `{"type":"openai"}`)
			it.Secret = "v1.abc"
			return it
		}()}, "api_key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			for _, it := range tt.items {
				if err := e.db.CreateConfigItem(ctx, it); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Runtime(ctx, e.db, nil, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to say %q", err, tt.want)
			}
		})
	}
}

func TestNumbers(t *testing.T) {
	b, err := decodeProfile([]byte(`{"limits":{"a":{"per_day":5,"usd":2.5},"list":[1,1.5],"big":1e30}}`))
	if err != nil {
		t.Fatal(err)
	}
	a := b.Limits["a"].(map[string]any)
	if _, ok := a["per_day"].(int); !ok {
		t.Fatalf("per_day %T", a["per_day"])
	}
	if a["usd"] != 2.5 || b.Limits["list"].([]any)[0] != 1 || b.Limits["big"] != 1e30 {
		t.Fatalf("limits %v", b.Limits)
	}
	if _, err := decodeProfile([]byte(`{`)); err == nil {
		t.Fatal("a broken profile decoded")
	}
	raw, _ := json.Marshal(profileBody{})
	if b, err := decodeProfile(raw); err != nil || b.Limits != nil {
		t.Fatalf("empty profile %+v, %v", b, err)
	}
}

func TestNewIDAndLast4(t *testing.T) {
	a, _ := newID("rt_")
	b, _ := newID("rt_")
	if a == b || len(a) != len("rt_")+13 || strings.ToLower(a) != a {
		t.Fatalf("ids %q %q", a, b)
	}
	if last4("abcd") != "" || last4("abcde") != "bcde" {
		t.Fatal("last4")
	}
}
