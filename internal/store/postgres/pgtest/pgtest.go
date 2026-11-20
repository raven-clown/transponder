// Package pgtest gives tests a Postgres database of their own. Tests that need one are skipped
// unless TRANSPONDER_TEST_POSTGRES names a server, so a plain go test passes without Postgres.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Env is the variable holding the postgres:// URL of the test server.
const Env = "TRANSPONDER_TEST_POSTGRES"

// DSN returns the URL of an empty schema on the test server, dropped when the test ends. It
// skips the test when no server is configured.
func DSN(t *testing.T) string {
	t.Helper()
	base := os.Getenv(Env)
	if base == "" {
		t.Skipf("%s is not set", Env)
	}
	u, err := url.Parse(base)
	if err != nil || !strings.HasPrefix(u.Scheme, "postgres") {
		t.Fatalf("%s must be a postgres:// URL", Env)
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	schema := "tp_test_" + hex.EncodeToString(raw[:])
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connecting to %s: %v", Env, err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Errorf("dropping %s: %v", schema, err)
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("dropping %s: %v", schema, err)
		}
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
