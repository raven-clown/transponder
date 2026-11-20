package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/raven-clown/transponder/internal/secrets"
	"github.com/raven-clown/transponder/internal/store"
)

// ActorCLI is the audit actor of a change made by a transponder command, such as secrets rotate.
const ActorCLI = "cli"

// Reseal seals every provider key stored through the admin API again under box's current key,
// opening it with box's current or a previous key, and returns how many it changed. It writes
// them in one config write with an audit record each, so every replica reloads, and writes
// nothing when a key does not open or none needs it.
func Reseal(ctx context.Context, st SyncStore, box *secrets.Box, now func() time.Time) (int, error) {
	pending, err := resealable(ctx, st, box)
	if err != nil || pending == 0 {
		return 0, err
	}
	var n int
	_, err = st.UpdateConfig(ctx, func(ctx context.Context) error {
		n = 0
		items, err := st.ListConfigItems(ctx, store.DefaultOrg, kindProviders)
		if err != nil {
			return err
		}
		for _, it := range items {
			if it.Secret == "" {
				continue
			}
			sealed, changed, err := box.Reseal(it.Secret, secretContext(it.ID))
			if err != nil {
				return fmt.Errorf("provider %q api_key: %w", it.ID, err)
			}
			if !changed {
				continue
			}
			at := now()
			it.Secret, it.UpdatedAt = sealed, at
			if _, err := st.UpdateConfigItem(ctx, it, it.Version); err != nil {
				return fmt.Errorf("provider %q: %w", it.ID, err)
			}
			// The body holds no secret; before and after are the same, the action says what changed.
			diff, err := json.Marshal(map[string]any{"before": rawOrNil(it.Body), "after": rawOrNil(it.Body)})
			if err != nil {
				return err
			}
			if err := st.AddAudit(ctx, store.AuditRecord{OrgID: store.DefaultOrg, Time: at, Actor: ActorCLI,
				Action: actionRotate, Kind: kindProviders, Resource: it.ID, Diff: diff}); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// resealable counts the stored provider keys that do not open with box's current key, and
// fails when one opens with none of its keys.
func resealable(ctx context.Context, st SyncStore, box *secrets.Box) (int, error) {
	items, err := st.ListConfigItems(ctx, store.DefaultOrg, kindProviders)
	if err != nil {
		return 0, err
	}
	var n int
	for _, it := range items {
		if it.Secret == "" {
			continue
		}
		_, changed, err := box.Reseal(it.Secret, secretContext(it.ID))
		if err != nil {
			return 0, fmt.Errorf("provider %q api_key: %w", it.ID, err)
		}
		if changed {
			n++
		}
	}
	return n, nil
}
