package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/sqlite"
)

// demoKey is an API key the demo knows in advance, so its traffic and its README can use it.
type demoKey struct {
	name string
	role auth.Role
}

// demoKeys are the callers of the demo. The limits in demo/transponder.yaml name them.
var demoKeys = []demoKey{
	{"alice", auth.RoleMember},
	{"bob", auth.RoleMember},
	{"batch", auth.RoleMember},  // 10 calls a minute
	{"trial", auth.RoleMember},  // a fifth of a cent a day
	{"viewer", auth.RoleViewer}, // may read /metrics on the gateway's port
}

// secret is the key itself. A real key is random and shown once; these are fixed and
// public, which is only fit for a demo on your own machine.
func (k demoKey) secret() string { return "tp_demo_" + k.name }

func runSeed(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "./transponder.db", "path of the SQLite store transponder will open")
	if err := fs.Parse(args); err != nil {
		return err
	}
	created, err := seed(ctx, *db, time.Now())
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "seed: %d of %d demo keys created in %s\n", created, len(demoKeys), *db)
	return nil
}

// seed creates the demo keys in the store at path, as the admin API will once it can make
// keys. Keys that are there already are left alone, so it can run on every start.
func seed(ctx context.Context, path string, now time.Time) (int, error) {
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	created := 0
	for _, k := range demoKeys {
		secret := k.secret()
		err := db.CreateKey(ctx, store.APIKey{
			OrgID: store.DefaultOrg, Name: k.name, Hash: auth.HashKey(secret), Last4: secret[len(secret)-4:],
			Role: string(k.role), CreatedAt: now,
		})
		switch {
		case errors.Is(err, store.ErrConflict):
		case err != nil:
			return created, fmt.Errorf("creating the key %s: %w", k.name, err)
		default:
			created++
		}
	}
	return created, nil
}
