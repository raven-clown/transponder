package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/sqlite"
)

// BenchmarkAuthenticateKey is the key check at the start of every call: hash the key and look
// it up in SQLite.
func BenchmarkAuthenticateKey(b *testing.B) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	plain, hash, last4, err := auth.NewKey()
	if err != nil {
		b.Fatal(err)
	}
	if err := db.CreateKey(ctx, store.APIKey{OrgID: store.DefaultOrg, Name: "bench", Hash: hash, Last4: last4, Role: string(auth.RoleMember), CreatedAt: time.Now()}); err != nil {
		b.Fatal(err)
	}
	a := auth.NewAuthenticator(db, time.Now)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer "+plain)
		for pb.Next() {
			if _, err := a.Authenticate(r); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
