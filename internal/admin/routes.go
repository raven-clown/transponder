package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/store"
)

// maxPosition keeps positions in the 19 digits a cursor gives them.
const maxPosition = 1_000_000_000_000_000

type routeJSON struct {
	ID       string       `json:"id"`
	Position int64        `json:"position"`
	Match    string       `json:"match"`
	Allow    []string     `json:"allow"`
	Targets  []targetBody `json:"targets"`
	metaJSON
}

func viewRoute(it store.ConfigItem) (any, error) {
	var b routeBody
	if err := json.Unmarshal(it.Body, &b); err != nil {
		return nil, err
	}
	allow := b.Allow
	if allow == nil {
		allow = []string{}
	}
	return routeJSON{ID: it.ID, Position: it.Position, Match: b.Match, Allow: allow, Targets: b.Targets, metaJSON: metaOf(it)}, nil
}

type routeWrite struct {
	Match    *string       `json:"match"`
	Allow    *[]string     `json:"allow"`
	Targets  *[]targetBody `json:"targets"`
	Position *int64        `json:"position"`
}

func (h *handler) routeSpec() *itemSpec {
	return &itemSpec{
		kind: kindRoutes, what: "route", param: "id", prefix: "rt_",
		view: viewRoute,
		prepare: func(raw []byte) (store.ConfigItem, error) {
			var body routeWrite
			if err := decodeBytes(raw, &body); err != nil {
				return store.ConfigItem{}, err
			}
			it := store.ConfigItem{Position: -1}
			return it, applyRoute(&it, routeBody{}, body)
		},
		patch: func(it *store.ConfigItem, raw []byte) error {
			var body routeWrite
			if err := decodeBytes(raw, &body); err != nil {
				return err
			}
			var b routeBody
			if err := json.Unmarshal(it.Body, &b); err != nil {
				return err
			}
			return applyRoute(it, b, body)
		},
		finish: func(ctx context.Context, it *store.ConfigItem) error {
			var b routeBody
			if err := json.Unmarshal(it.Body, &b); err != nil {
				return err
			}
			for i, t := range b.Targets {
				_, err := h.d.Store.ConfigItem(ctx, it.OrgID, kindProviders, t.Provider)
				if errors.Is(err, store.ErrNotFound) {
					return invalid(fmt.Sprintf("targets[%d].provider", i), "there is no provider %q", t.Provider)
				}
				if err != nil {
					return err
				}
			}
			if it.Position >= 0 {
				return nil
			}
			// A new route without a position goes last.
			routes, err := h.d.Store.ListConfigItems(ctx, it.OrgID, kindRoutes)
			if err != nil {
				return err
			}
			it.Position = routeStep
			if n := len(routes); n > 0 {
				it.Position = min(routes[n-1].Position+routeStep, maxPosition)
			}
			return nil
		},
	}
}

// applyRoute applies the fields of w that are set to b, checks the result and stores it.
func applyRoute(it *store.ConfigItem, b routeBody, w routeWrite) error {
	if w.Match != nil {
		b.Match = *w.Match
	}
	if w.Allow != nil {
		b.Allow = *w.Allow
	}
	if w.Targets != nil {
		b.Targets = *w.Targets
	}
	if w.Position != nil {
		if *w.Position < 0 || *w.Position > maxPosition {
			return invalid("position", "position must be from 0 to %d", int64(maxPosition))
		}
		it.Position = *w.Position
	}
	if b.Match == "" {
		return invalid("match", "match must name a model or a pattern with *")
	}
	if len(b.Allow) == 0 {
		b.Allow = nil
	}
	if _, err := router.ParseAccess(b.Allow); err != nil {
		return invalid("allow", "%v", err)
	}
	if len(b.Targets) == 0 {
		return invalid("targets", "a route needs at least one target")
	}
	for i, t := range b.Targets {
		if t.Provider == "" {
			return invalid(fmt.Sprintf("targets[%d].provider", i), "every target names a provider")
		}
	}
	raw, err := json.Marshal(b)
	it.Body = raw
	return err
}
