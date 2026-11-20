package auth

import (
	"context"
	"sync"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

type fakeKeys struct {
	mu   sync.Mutex
	keys []store.APIKey
}

func (f *fakeKeys) CreateKey(_ context.Context, k store.APIKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.keys {
		if e.OrgID == k.OrgID && e.Name == k.Name || e.Hash == k.Hash {
			return store.ErrConflict
		}
	}
	f.keys = append(f.keys, k)
	return nil
}

func (f *fakeKeys) KeyByHash(_ context.Context, h [32]byte) (store.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.keys {
		if e.Hash == h {
			return e, nil
		}
	}
	return store.APIKey{}, store.ErrNotFound
}

func (f *fakeKeys) HasAdminKey(_ context.Context, org string, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.keys {
		if e.OrgID == org && e.Role == string(RoleAdmin) && !e.Disabled && (e.ExpiresAt.IsZero() || e.ExpiresAt.After(now)) {
			return true, nil
		}
	}
	return false, nil
}
