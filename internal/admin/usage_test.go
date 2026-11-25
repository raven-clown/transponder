package admin

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/store"
)

// seedUsage stores calls on the 1st and 2nd of this month and one last month.
func seedUsage(t *testing.T, e *env) {
	t.Helper()
	day1 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	recs := []store.UsageRecord{
		{Time: day1, User: "alice", Teams: []string{"platform", "ml"}, Key: "alice-key", Project: "p1", Provider: "openai", Model: "gpt-5", Route: "gpt-*", InputTokens: 10, OutputTokens: 5, CostNUSD: 1_500_000_000},
		{Time: day1.Add(time.Hour), User: "alice", Teams: []string{"platform"}, Key: "alice-key", Project: "p2", Provider: "anthropic", Model: "claude", InputTokens: 20, OutputTokens: 1, CacheReadTokens: 2, CacheWriteTokens: 3, ReasoningTokens: 4, CostNUSD: 500_000_000},
		{Time: day1.AddDate(0, 0, 1), User: "bob", Key: "bob-key", Project: "p1", Provider: "openai", Model: "gpt-5", InputTokens: 1, OutputTokens: 1, CostNUSD: 7},
		{Time: day1.AddDate(0, -1, 0), User: "old", CostNUSD: 99},
	}
	for _, r := range recs {
		r.OrgID = store.DefaultOrg
		if err := e.db.RecordUsage(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
}

func groups(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, it := range body["items"].([]any) {
		m := it.(map[string]any)
		out[m["group"].(string)] = m
	}
	return out
}

func TestUsageTotals(t *testing.T) {
	e := newEnv(t)
	seedUsage(t, e)
	body := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/usage", nil))
	if body["group_by"] != "user" || body["from"] != "2026-11-01T00:00:00Z" || body["to"] != "2026-11-20T12:00:00Z" {
		t.Fatalf("body %v", body)
	}
	g := groups(t, body)
	if len(g) != 2 {
		t.Fatalf("groups %v", g)
	}
	alice := g["alice"]
	if alice["requests"] != float64(2) || alice["input_tokens"] != float64(30) || alice["cost_nusd"] != float64(2_000_000_000) ||
		alice["cost_usd"] != float64(2) || alice["reasoning_tokens"] != float64(4) || alice["cache_write_tokens"] != float64(3) {
		t.Fatalf("alice %v", alice)
	}

	tests := []struct {
		query string
		want  map[string]float64 // group: requests
	}{
		{"group_by=team", map[string]float64{"platform": 2, "ml": 1}},
		{"group_by=model", map[string]float64{"gpt-5": 2, "claude": 1}},
		{"group_by=provider&user=alice", map[string]float64{"openai": 1, "anthropic": 1}},
		{"group_by=project&team=platform", map[string]float64{"p1": 1, "p2": 1}},
		{"group_by=key&key=bob-key", map[string]float64{"bob-key": 1}},
		{"group_by=route&project=p1", map[string]float64{"gpt-*": 1, "": 1}},
		{"group_by=day", map[string]float64{"2026-11-01": 2, "2026-11-02": 1}},
		{"group_by=user&from=2026-10-01&to=2026-11-01", map[string]float64{"old": 1}},
		{"group_by=user&from=2026-11-02T00:00:00Z", map[string]float64{"bob": 1}},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			g := groups(t, decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/usage?"+tt.query, nil)))
			if len(g) != len(tt.want) {
				t.Fatalf("groups %v, want %v", g, tt.want)
			}
			for name, n := range tt.want {
				if g[name]["requests"] != n {
					t.Fatalf("group %q: %v, want %v requests", name, g[name], n)
				}
			}
		})
	}
}

func TestUsagePages(t *testing.T) {
	e := newEnv(t)
	seedUsage(t, e)
	first := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/usage?group_by=project&limit=1", nil))
	next, _ := first["next_cursor"].(string)
	if items := first["items"].([]any); len(items) != 1 || items[0].(map[string]any)["group"] != "p1" || next == "" {
		t.Fatalf("first page %v", first)
	}
	second := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/usage?group_by=project&limit=1&cursor="+next, nil))
	if items := second["items"].([]any); len(items) != 1 || items[0].(map[string]any)["group"] != "p2" || second["next_cursor"] != nil {
		t.Fatalf("second page %v", second)
	}
}

func TestUsageReadsInBatches(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := range usageBatch + 5 {
		r := store.UsageRecord{OrgID: store.DefaultOrg, Time: testNow.Add(-time.Duration(i+1) * time.Second), User: "many", CostNUSD: 1}
		if err := e.db.RecordUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	g := groups(t, decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/usage", nil)))
	if g["many"]["requests"] != float64(usageBatch+5) {
		t.Fatalf("groups %v", g)
	}
}

func TestUsageInvalid(t *testing.T) {
	e := newEnv(t)
	for _, tt := range []struct{ query, field string }{
		{"group_by=color", "group_by"},
		{"from=yesterday", "from"},
		{"to=2026-13-01", "to"},
		{"from=2026-11-02&to=2026-11-01", "to"},
		{"limit=1000", "limit"},
	} {
		wantError(t, e.do(auth.RoleViewer, http.MethodGet, "/usage?"+tt.query, nil), http.StatusBadRequest, "invalid", tt.field)
	}
}

func TestRequests(t *testing.T) {
	e := newEnv(t)
	seedUsage(t, e)
	first := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/requests?limit=3", nil))
	items := first["items"].([]any)
	next, _ := first["next_cursor"].(string)
	if len(items) != 3 || next == "" {
		t.Fatalf("first page %v", first)
	}
	top := items[0].(map[string]any)
	if top["user"] != "old" || top["cost_nusd"] != float64(99) || len(top["teams"].([]any)) != 0 {
		t.Fatalf("newest %v", top)
	}
	second := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/requests?limit=3&cursor="+next, nil))
	if items := second["items"].([]any); len(items) != 1 || items[0].(map[string]any)["user"] != "alice" || second["next_cursor"] != nil {
		t.Fatalf("second page %v", second)
	}
	filtered := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/requests?user=alice&project=p2&from=2026-11-01&to=2026-11-02", nil))
	if items := filtered["items"].([]any); len(items) != 1 || items[0].(map[string]any)["model"] != "claude" {
		t.Fatalf("filtered %v", filtered)
	}
	wantError(t, e.do(auth.RoleViewer, http.MethodGet, "/requests?from=x", nil), http.StatusBadRequest, "invalid", "from")
	wantError(t, e.do(auth.RoleViewer, http.MethodGet, "/requests?limit=-1", nil), http.StatusBadRequest, "invalid", "limit")
}

func TestAuditList(t *testing.T) {
	e := newEnv(t)
	e.do(auth.RoleAdmin, http.MethodPost, "/keys", map[string]any{"name": "a"})
	e.do(auth.RoleAdmin, http.MethodPost, "/keys", map[string]any{"name": "b"})
	e.do(auth.RoleAdmin, http.MethodPatch, "/keys/a", map[string]any{"disabled": true})

	first := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/audit?limit=2", nil))
	items := first["items"].([]any)
	next, _ := first["next_cursor"].(string)
	if len(items) != 2 || next == "" {
		t.Fatalf("first page %v", first)
	}
	top := items[0].(map[string]any)
	diff := top["diff"].(map[string]any)
	if top["action"] != "update" || top["resource"] != "a" || top["actor"] != "key:the-admin" || top["request_id"] != "req-test" ||
		diff["before"].(map[string]any)["disabled"] != false || diff["after"].(map[string]any)["disabled"] != true {
		t.Fatalf("newest %v", top)
	}
	second := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/audit?limit=2&cursor="+next, nil))
	if items := second["items"].([]any); len(items) != 1 || second["next_cursor"] != nil {
		t.Fatalf("second page %v", second)
	}
	for _, tt := range []struct {
		query string
		want  int
	}{
		{"action=create", 2},
		{"action=update", 1},
		{"action=delete", 0},
		{"resource=keys", 3},
		{"resource=keys/a", 2},
		{"resource=providers", 0},
		{"actor=key:the-admin", 3},
		{"actor=user:nobody", 0},
		{"from=2026-11-20T12:00:00Z&to=2026-11-21", 3},
		{"to=2026-11-20", 0},
	} {
		body := decodeBody(t, e.do(auth.RoleViewer, http.MethodGet, "/audit?"+tt.query, nil))
		if n := len(body["items"].([]any)); n != tt.want {
			t.Errorf("%s: %d records, want %d", tt.query, n, tt.want)
		}
	}
	wantError(t, e.do(auth.RoleViewer, http.MethodGet, "/audit?from=never", nil), http.StatusBadRequest, "invalid", "from")
	wantError(t, e.do(auth.RoleViewer, http.MethodGet, "/audit?action=read", nil), http.StatusBadRequest, "invalid", "action")
}
