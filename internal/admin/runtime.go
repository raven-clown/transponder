package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/secrets"
	"github.com/raven-clown/transponder/internal/store"
)

// The kinds of runtime config items.
const (
	kindProviders = "providers"
	kindRoutes    = "routes"
	kindBudgets   = "budgets"
	kindProfiles  = "profiles"
)

var runtimeKinds = []string{kindProviders, kindRoutes, kindBudgets, kindProfiles}

// The bodies of the items as the store keeps them.
type providerBody struct {
	Type     string `json:"type"`
	BaseURL  string `json:"base_url,omitempty"`
	KeyLast4 string `json:"api_key_last4,omitempty"`
}

type targetBody struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
}

type routeBody struct {
	Match   string       `json:"match"`
	Allow   []string     `json:"allow,omitempty"`
	Targets []targetBody `json:"targets"`
}

type budgetBody struct {
	Subject  string  `json:"subject"`
	Period   string  `json:"period"`
	LimitUSD float64 `json:"limit_usd"`
}

type profileBody struct {
	Priority int            `json:"priority,omitempty"`
	Limits   map[string]any `json:"limits,omitempty"`
	Subjects []string       `json:"subjects,omitempty"`
	Default  bool           `json:"default,omitempty"`
}

// RuntimeStore is what loading the runtime config reads.
type RuntimeStore interface {
	store.ConfigItemStore
}

// secretContext binds a sealed provider key to its provider.
func secretContext(name string) string { return kindProviders + "/" + name }

// last4 is what is shown of a secret.
func last4(s string) string {
	if len(s) <= 4 {
		return ""
	}
	return s[len(s)-4:]
}

// fileID names the i-th route or budget of the file, which have no name of their own.
func fileID(i int) string { return fmt.Sprintf("file-%03d", i+1) }

// routeStep is the gap between route positions, so a route can be put between two others.
const routeStep = 1000

// fileItems returns the providers, routes, budgets and profiles of the file as store items.
func fileItems(cfg *config.Bootstrap) (map[string][]store.ConfigItem, error) {
	out := map[string][]store.ConfigItem{}
	add := func(kind, id string, position int64, body any) error {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		out[kind] = append(out[kind], store.ConfigItem{OrgID: store.DefaultOrg, Kind: kind, ID: id, Position: position, Body: raw, Source: store.SourceFile})
		return nil
	}
	for _, p := range cfg.Providers {
		b := providerItem{providerBody: providerBody{Type: p.Type, BaseURL: p.BaseURL, KeyLast4: last4(p.APIKey)}, KeySet: p.APIKey != ""}
		if err := add(kindProviders, p.Name, 0, b); err != nil {
			return nil, err
		}
	}
	for i, r := range cfg.Routes {
		b := routeBody{Match: r.Match, Allow: r.Allow}
		for _, t := range r.Targets {
			b.Targets = append(b.Targets, targetBody{Provider: t.Provider, Model: t.Model})
		}
		if err := add(kindRoutes, fileID(i), int64(i+1)*routeStep, b); err != nil {
			return nil, err
		}
	}
	for i, b := range cfg.Budgets {
		if err := add(kindBudgets, fileID(i), 0, budgetBody{Subject: b.Subject, Period: b.Period, LimitUSD: b.LimitUSD}); err != nil {
			return nil, err
		}
	}
	for _, p := range cfg.Profiles {
		b := profileBody{Priority: p.Priority, Limits: p.Limits, Default: cfg.DefaultProfile == p.Name}
		for _, bind := range cfg.ProfileBindings {
			if bind.Profile == p.Name {
				b.Subjects = append(b.Subjects, bind.Subject)
			}
		}
		if err := add(kindProfiles, p.Name, 0, b); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// fileChange is one write that brings the store in line with the file.
type fileChange struct {
	action string
	item   store.ConfigItem
	old    store.ConfigItem
}

// fileChanges compares the file's items with the stored ones.
func fileChanges(ctx context.Context, st RuntimeStore, want map[string][]store.ConfigItem) ([]fileChange, error) {
	var out []fileChange
	for _, kind := range runtimeKinds {
		stored, err := st.ListConfigItems(ctx, store.DefaultOrg, kind)
		if err != nil {
			return nil, err
		}
		byID := map[string]store.ConfigItem{}
		for _, it := range stored {
			byID[it.ID] = it
		}
		wanted := map[string]bool{}
		for _, it := range want[kind] {
			wanted[it.ID] = true
			old, ok := byID[it.ID]
			switch {
			case !ok:
				out = append(out, fileChange{action: actionCreate, item: it})
			case old.Source != store.SourceFile || old.Position != it.Position || !bytes.Equal(old.Body, it.Body) || old.Secret != "":
				out = append(out, fileChange{action: actionUpdate, item: it, old: old})
			}
		}
		for _, it := range stored {
			if it.Source == store.SourceFile && !wanted[it.ID] {
				out = append(out, fileChange{action: actionDelete, old: it})
			}
		}
	}
	return out, nil
}

// SyncStore is what SyncFile writes to.
type SyncStore interface {
	RuntimeStore
	store.ConfigStore
	store.AuditStore
}

// SyncFile makes the providers, routes, budgets and profiles of the file the store's items of
// source file: new ones are added, changed ones updated, and ones no longer in the file
// deleted. A file item takes over an API item of the same name. Each change is in the audit
// log with the actor file. When nothing changed, the config version stays as it is.
func SyncFile(ctx context.Context, st SyncStore, cfg *config.Bootstrap, now func() time.Time, logger *slog.Logger) error {
	want, err := fileItems(cfg)
	if err != nil {
		return err
	}
	changes, err := fileChanges(ctx, st, want)
	if err != nil || len(changes) == 0 {
		return err
	}
	// Replicas that start together may race to write the same items; the loser reads again.
	for attempt := 0; ; attempt++ {
		_, err = st.UpdateConfig(ctx, func(ctx context.Context) error {
			changes, err := fileChanges(ctx, st, want)
			if err != nil {
				return err
			}
			for _, c := range changes {
				if err := applyFileChange(ctx, st, c, now(), logger); err != nil {
					return err
				}
			}
			return nil
		})
		raced := errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrVersionConflict) || errors.Is(err, store.ErrNotFound)
		if !raced || attempt == 2 {
			return err
		}
	}
}

func applyFileChange(ctx context.Context, st SyncStore, c fileChange, at time.Time, logger *slog.Logger) error {
	var err error
	it := c.item
	it.CreatedAt, it.UpdatedAt = at, at
	switch c.action {
	case actionCreate:
		err = st.CreateConfigItem(ctx, it)
	case actionUpdate:
		if c.old.Source != store.SourceFile {
			logger.WarnContext(ctx, "an item in the config file replaces one made through the admin API", "kind", it.Kind, "id", it.ID)
		}
		_, err = st.UpdateConfigItem(ctx, it, c.old.Version)
	case actionDelete:
		it = c.old
		err = st.DeleteConfigItem(ctx, it.OrgID, it.Kind, it.ID, it.Version)
	}
	if err != nil {
		return err
	}
	diff, err := json.Marshal(map[string]any{"before": rawOrNil(c.old.Body), "after": rawOrNil(c.item.Body)})
	if err != nil {
		return err
	}
	return st.AddAudit(ctx, store.AuditRecord{OrgID: store.DefaultOrg, Time: at, Actor: "file", Action: c.action, Kind: it.Kind, Resource: it.ID, Diff: diff})
}

func rawOrNil(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

// Runtime returns file with its providers, routes, profiles, bindings and budgets replaced by
// the store's items, validated. Provider keys of file items come from file; those made through
// the API are opened with box.
func Runtime(ctx context.Context, st RuntimeStore, box *secrets.Box, file *config.Bootstrap) (*config.Bootstrap, error) {
	if file == nil {
		file = &config.Bootstrap{}
	}
	rc := *file
	rc.Providers, rc.Routes, rc.Profiles, rc.ProfileBindings, rc.Budgets = nil, nil, nil, nil, nil
	rc.DefaultProfile, rc.ProfilesFile = "", ""
	fileKeys := map[string]string{}
	for _, p := range file.Providers {
		fileKeys[p.Name] = p.APIKey
	}
	items := map[string][]store.ConfigItem{}
	for _, kind := range runtimeKinds {
		list, err := st.ListConfigItems(ctx, store.DefaultOrg, kind)
		if err != nil {
			return nil, err
		}
		items[kind] = list
	}
	for _, it := range items[kindProviders] {
		var b providerBody
		if err := json.Unmarshal(it.Body, &b); err != nil {
			return nil, fmt.Errorf("provider %q: %w", it.ID, err)
		}
		p := config.Provider{Name: it.ID, Type: b.Type, BaseURL: b.BaseURL}
		switch {
		case it.Source == store.SourceFile:
			p.APIKey = fileKeys[it.ID]
		case it.Secret != "":
			key, err := box.Open(it.Secret, secretContext(it.ID))
			if err != nil {
				return nil, fmt.Errorf("provider %q api_key: %w", it.ID, err)
			}
			p.APIKey = key
		}
		rc.Providers = append(rc.Providers, p)
	}
	for _, it := range items[kindRoutes] {
		var b routeBody
		if err := json.Unmarshal(it.Body, &b); err != nil {
			return nil, fmt.Errorf("route %q: %w", it.ID, err)
		}
		r := config.Route{Match: b.Match, Allow: b.Allow}
		for _, t := range b.Targets {
			r.Targets = append(r.Targets, config.Target{Provider: t.Provider, Model: t.Model})
		}
		rc.Routes = append(rc.Routes, r)
	}
	for _, it := range items[kindBudgets] {
		var b budgetBody
		if err := json.Unmarshal(it.Body, &b); err != nil {
			return nil, fmt.Errorf("budget %q: %w", it.ID, err)
		}
		rc.Budgets = append(rc.Budgets, config.BudgetConfig{Subject: b.Subject, Period: b.Period, LimitUSD: b.LimitUSD})
	}
	bound := map[string]string{}
	for _, it := range items[kindProfiles] {
		b, err := decodeProfile(it.Body)
		if err != nil {
			return nil, fmt.Errorf("profile %q: %w", it.ID, err)
		}
		rc.Profiles = append(rc.Profiles, config.ProfileConfig{Name: it.ID, Priority: b.Priority, Limits: b.Limits})
		if b.Default {
			if rc.DefaultProfile != "" {
				return nil, fmt.Errorf("config: profiles %q and %q are both the default", rc.DefaultProfile, it.ID)
			}
			rc.DefaultProfile = it.ID
		}
		for _, s := range b.Subjects {
			if other, ok := bound[s]; ok {
				return nil, fmt.Errorf("config: %s is bound to both profile %q and profile %q", s, other, it.ID)
			}
			bound[s] = it.ID
			rc.ProfileBindings = append(rc.ProfileBindings, config.BindingConfig{Subject: s, Profile: it.ID})
		}
	}
	if err := rc.ValidateRuntime(); err != nil {
		return nil, err
	}
	return &rc, nil
}

// decodeProfile reads a profile body with its limits as the YAML file gives them: whole
// numbers as int, so the limits parser reads JSON and YAML alike.
func decodeProfile(raw []byte) (profileBody, error) {
	var b profileBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&b); err != nil {
		return b, err
	}
	if b.Limits != nil {
		b.Limits = numbers(b.Limits).(map[string]any)
	}
	return b, nil
}

func numbers(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			v[k] = numbers(x)
		}
		return v
	case []any:
		for i, x := range v {
			v[i] = numbers(x)
		}
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil && n >= math.MinInt && n <= math.MaxInt {
			return int(n)
		}
		f, _ := v.Float64()
		return f
	}
	return v
}

// usedBy returns the ids of the routes that send calls to provider.
func usedBy(routes []store.ConfigItem, provider string) []string {
	var out []string
	for _, it := range routes {
		var b routeBody
		if json.Unmarshal(it.Body, &b) != nil {
			continue
		}
		if slices.ContainsFunc(b.Targets, func(t targetBody) bool { return t.Provider == provider }) {
			out = append(out, it.ID)
		}
	}
	return out
}
