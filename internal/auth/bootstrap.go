package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

// EnsureAdmin creates an admin key and prints it once to stderr when the default org has none.
func EnsureAdmin(ctx context.Context, keys store.KeyStore, now func() time.Time, stderr io.Writer) error {
	has, err := keys.HasAdminKey(ctx, store.DefaultOrg, now())
	if err != nil {
		return fmt.Errorf("checking for an admin key: %w", err)
	}
	if has {
		return nil
	}
	plain, hash, last4, err := NewKey()
	if err != nil {
		return err
	}
	k := store.APIKey{
		OrgID:     store.DefaultOrg,
		Name:      "admin",
		Hash:      hash,
		Last4:     last4,
		Role:      string(RoleAdmin),
		CreatedAt: now(),
	}
	err = keys.CreateKey(ctx, k)
	if errors.Is(err, store.ErrConflict) {
		k.Name = "admin-" + last4
		err = keys.CreateKey(ctx, k)
	}
	if err != nil {
		return fmt.Errorf("creating the admin key: %w", err)
	}
	_, err = fmt.Fprintf(stderr, "transponder: created an admin API key. It is shown once, so save it now:\n%s\n", plain)
	return err
}
