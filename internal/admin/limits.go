package admin

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"

	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/store"
)

type budgetJSON struct {
	ID       string  `json:"id"`
	Subject  string  `json:"subject"`
	Period   string  `json:"period"`
	LimitUSD float64 `json:"limit_usd"`
	metaJSON
}

func viewBudget(it store.ConfigItem) (any, error) {
	var b budgetBody
	if err := json.Unmarshal(it.Body, &b); err != nil {
		return nil, err
	}
	return budgetJSON{ID: it.ID, Subject: b.Subject, Period: b.Period, LimitUSD: b.LimitUSD, metaJSON: metaOf(it)}, nil
}

type budgetWrite struct {
	Subject  *string  `json:"subject"`
	Period   *string  `json:"period"`
	LimitUSD *float64 `json:"limit_usd"`
}

func (h *handler) budgetSpec() *itemSpec {
	return &itemSpec{
		kind: kindBudgets, what: "budget", param: "id", prefix: "bg_",
		view: viewBudget,
		prepare: func(raw []byte) (store.ConfigItem, error) {
			var body budgetWrite
			if err := decodeBytes(raw, &body); err != nil {
				return store.ConfigItem{}, err
			}
			if body.LimitUSD == nil {
				return store.ConfigItem{}, invalid("limit_usd", "limit_usd is needed")
			}
			var it store.ConfigItem
			return it, applyBudget(&it, budgetBody{}, body)
		},
		patch: func(it *store.ConfigItem, raw []byte) error {
			var body budgetWrite
			if err := decodeBytes(raw, &body); err != nil {
				return err
			}
			var b budgetBody
			if err := json.Unmarshal(it.Body, &b); err != nil {
				return err
			}
			return applyBudget(it, b, body)
		},
	}
}

func applyBudget(it *store.ConfigItem, b budgetBody, w budgetWrite) error {
	if w.Subject != nil {
		b.Subject = *w.Subject
	}
	if w.Period != nil {
		b.Period = *w.Period
	}
	if w.LimitUSD != nil {
		b.LimitUSD = *w.LimitUSD
	}
	if b.Period != "daily" && b.Period != "monthly" {
		return invalid("period", "period must be daily or monthly")
	}
	if b.LimitUSD < 0 || b.LimitUSD > 1e9 || math.IsNaN(b.LimitUSD) {
		return invalid("limit_usd", "limit_usd must be an amount in USD from 0 to 1e9")
	}
	if _, err := limits.ParseBudget(b.Subject, b.Period, b.LimitUSD); err != nil {
		return invalid("subject", "%v", err)
	}
	raw, err := json.Marshal(b)
	it.Body = raw
	return err
}

type profileJSON struct {
	Name     string         `json:"name"`
	Priority int            `json:"priority"`
	Limits   map[string]any `json:"limits"`
	Subjects []string       `json:"subjects"`
	Default  bool           `json:"default"`
	metaJSON
}

func viewProfile(it store.ConfigItem) (any, error) {
	b, err := decodeProfile(it.Body)
	if err != nil {
		return nil, err
	}
	out := profileJSON{Name: it.ID, Priority: b.Priority, Limits: b.Limits, Subjects: b.Subjects, Default: b.Default, metaJSON: metaOf(it)}
	if out.Limits == nil {
		out.Limits = map[string]any{}
	}
	if out.Subjects == nil {
		out.Subjects = []string{}
	}
	return out, nil
}

type profileWrite struct {
	Name     string          `json:"name"`
	Priority *int            `json:"priority"`
	Limits   json.RawMessage `json:"limits"`
	Subjects *[]string       `json:"subjects"`
	Default  *bool           `json:"default"`
}

func (h *handler) profileSpec() *itemSpec {
	return &itemSpec{
		kind: kindProfiles, what: "profile", param: "name",
		view: viewProfile,
		prepare: func(raw []byte) (store.ConfigItem, error) {
			var body profileWrite
			if err := decodeBytes(raw, &body); err != nil {
				return store.ConfigItem{}, err
			}
			if !namePattern.MatchString(body.Name) {
				return store.ConfigItem{}, invalid("name", "name must be 1 to 64 letters, digits, dots, dashes or underscores")
			}
			it := store.ConfigItem{ID: body.Name}
			return it, applyProfile(&it, profileBody{}, body)
		},
		patch: func(it *store.ConfigItem, raw []byte) error {
			var body profileWrite
			if err := decodeBytes(raw, &body); err != nil {
				return err
			}
			if body.Name != "" && body.Name != it.ID {
				return invalid("name", "a profile cannot be renamed; create a new one")
			}
			b, err := decodeProfile(it.Body)
			if err != nil {
				return err
			}
			return applyProfile(it, b, body)
		},
	}
}

func applyProfile(it *store.ConfigItem, b profileBody, w profileWrite) error {
	if w.Priority != nil {
		b.Priority = *w.Priority
	}
	if w.Limits != nil {
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(w.Limits))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			return invalid("limits", "limits must be an object of limit keys, such as {\"llm.tokens\": {\"per_day\": 100000}}")
		}
		b.Limits = nil
		if m != nil {
			b.Limits = numbers(m).(map[string]any)
		}
	}
	if w.Subjects != nil {
		b.Subjects = *w.Subjects
	}
	if w.Default != nil {
		b.Default = *w.Default
	}
	if _, err := limits.ParseProfile(it.ID, b.Limits); err != nil {
		return invalid("limits", "%v", err)
	}
	seen := map[string]bool{}
	for _, s := range b.Subjects {
		kind, id, _ := strings.Cut(s, ":")
		if id == "" || (kind != "key" && kind != "user" && kind != "team") {
			return invalid("subjects", "subject %q must be key:<name>, user:<id> or team:<name>", s)
		}
		if seen[s] {
			return invalid("subjects", "subject %q is listed twice", s)
		}
		seen[s] = true
	}
	if len(b.Subjects) == 0 {
		b.Subjects = nil
	}
	raw, err := json.Marshal(b)
	it.Body = raw
	return err
}
