package admin

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/store"
)

// usageBatch is how many records a totals request reads from the store at a time.
const usageBatch = 1000

const nanoPerUSD = 1e9

type totalJSON struct {
	Group            string  `json:"group"`
	Requests         int64   `json:"requests"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CostNUSD         int64   `json:"cost_nusd"`
	CostUSD          float64 `json:"cost_usd"`
}

type usageJSON struct {
	listJSON[totalJSON]
	GroupBy string `json:"group_by"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// groupers give the groups a record counts in.
var groupers = map[string]func(store.UsageRecord) []string{
	"user":     func(r store.UsageRecord) []string { return []string{r.User} },
	"team":     func(r store.UsageRecord) []string { return r.Teams },
	"key":      func(r store.UsageRecord) []string { return []string{r.Key} },
	"project":  func(r store.UsageRecord) []string { return []string{r.Project} },
	"model":    func(r store.UsageRecord) []string { return []string{r.Model} },
	"provider": func(r store.UsageRecord) []string { return []string{r.Provider} },
	"route":    func(r store.UsageRecord) []string { return []string{r.Route} },
	"day":      func(r store.UsageRecord) []string { return []string{r.Time.UTC().Format("2006-01-02")} },
}

func (h *handler) usage(w http.ResponseWriter, r *http.Request) {
	p, err := readPage(r)
	if err != nil {
		h.fail(w, r, "usage", err)
		return
	}
	q := r.URL.Query()
	groupBy := q.Get("group_by")
	if groupBy == "" {
		groupBy = "user"
	}
	group, ok := groupers[groupBy]
	if !ok {
		names := make([]string, 0, len(groupers))
		for name := range groupers {
			names = append(names, name)
		}
		slices.Sort(names)
		h.fail(w, r, "usage", invalid("group_by", "group_by must be one of %s", strings.Join(names, ", ")))
		return
	}
	from, to, err := readRange(q, h.d.Now())
	if err != nil {
		h.fail(w, r, "usage", err)
		return
	}
	id, _ := auth.FromContext(r.Context())
	team := q.Get("team")
	totals := map[string]*totalJSON{}
	sq := store.UsageQuery{OrgID: orgOf(id), From: from, To: to, User: q.Get("user"), Key: q.Get("key"), Project: q.Get("project"), Limit: usageBatch}
	for {
		recs, err := h.d.Store.ListUsage(r.Context(), sq)
		if err != nil {
			h.fail(w, r, "usage", err)
			return
		}
		for _, rec := range recs {
			if team != "" && !slices.Contains(rec.Teams, team) {
				continue
			}
			for _, g := range group(rec) {
				t := totals[g]
				if t == nil {
					t = &totalJSON{Group: g}
					totals[g] = t
				}
				t.Requests++
				t.InputTokens += rec.InputTokens
				t.OutputTokens += rec.OutputTokens
				t.CacheReadTokens += rec.CacheReadTokens
				t.CacheWriteTokens += rec.CacheWriteTokens
				t.ReasoningTokens += rec.ReasoningTokens
				t.CostNUSD += rec.CostNUSD
			}
		}
		if len(recs) < usageBatch {
			break
		}
		sq.BeforeID = recs[len(recs)-1].ID
	}
	items := make([]totalJSON, 0, len(totals))
	for _, t := range totals {
		t.CostUSD = float64(t.CostNUSD) / nanoPerUSD
		items = append(items, *t)
	}
	slices.SortFunc(items, func(a, b totalJSON) int { return strings.Compare(a.Group, b.Group) })
	writeJSON(w, http.StatusOK, usageJSON{
		listJSON: keyPage(items, p, func(t totalJSON) string { return t.Group }),
		GroupBy:  groupBy,
		From:     formatTime(from),
		To:       formatTime(to),
	})
}

type requestJSON struct {
	ID               int64    `json:"id"`
	Time             string   `json:"time"`
	RequestID        string   `json:"request_id"`
	User             string   `json:"user"`
	Teams            []string `json:"teams"`
	Key              string   `json:"key"`
	Project          string   `json:"project"`
	Route            string   `json:"route"`
	Provider         string   `json:"provider"`
	Model            string   `json:"model"`
	Endpoint         string   `json:"endpoint"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	ReasoningTokens  int64    `json:"reasoning_tokens"`
	CostNUSD         int64    `json:"cost_nusd"`
	CostUSD          float64  `json:"cost_usd"`
	Estimated        bool     `json:"estimated"`
	Status           int      `json:"status"`
	LatencyMS        int64    `json:"latency_ms"`
	Fallbacks        int      `json:"fallbacks"`
}

func (h *handler) requests(w http.ResponseWriter, r *http.Request) {
	p, err := readPage(r)
	if err != nil {
		h.fail(w, r, "requests", err)
		return
	}
	q := r.URL.Query()
	from, to, err := readRange(q, time.Time{})
	if err != nil {
		h.fail(w, r, "requests", err)
		return
	}
	id, _ := auth.FromContext(r.Context())
	recs, err := h.d.Store.ListUsage(r.Context(), store.UsageQuery{
		OrgID: orgOf(id), From: from, To: to, User: q.Get("user"), Key: q.Get("key"), Project: q.Get("project"),
		BeforeID: p.afterID, Limit: p.limit + 1,
	})
	if err != nil {
		h.fail(w, r, "requests", err)
		return
	}
	items := make([]requestJSON, 0, len(recs))
	for _, rec := range recs {
		teams := rec.Teams
		if teams == nil {
			teams = []string{}
		}
		items = append(items, requestJSON{
			ID: rec.ID, Time: formatTime(rec.Time), RequestID: rec.RequestID, User: rec.User, Teams: teams,
			Key: rec.Key, Project: rec.Project, Route: rec.Route, Provider: rec.Provider, Model: rec.Model,
			Endpoint: rec.Endpoint, InputTokens: rec.InputTokens, OutputTokens: rec.OutputTokens,
			CacheReadTokens: rec.CacheReadTokens, CacheWriteTokens: rec.CacheWriteTokens,
			ReasoningTokens: rec.ReasoningTokens, CostNUSD: rec.CostNUSD, CostUSD: float64(rec.CostNUSD) / nanoPerUSD,
			Estimated: rec.Estimated, Status: rec.Status, LatencyMS: rec.Latency.Milliseconds(), Fallbacks: rec.Fallbacks,
		})
	}
	writeIDPage(w, items, p.limit, func(r requestJSON) int64 { return r.ID })
}
