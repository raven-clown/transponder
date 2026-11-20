package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/admin"
	"github.com/raven-clown/transponder/internal/secrets"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/sqlite"
)

func TestSecretsRotate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "transponder.db")
	oldKey, newKey := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	oldBox, _ := secrets.New(oldKey)
	newBox, _ := secrets.New(newKey)

	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := oldBox.Seal("sk-stored-1234", "providers/oa")
	if err != nil {
		t.Fatal(err)
	}
	it := store.ConfigItem{OrgID: store.DefaultOrg, Kind: "providers", ID: "oa", Body: []byte(`{"type":"openai"}`),
		Secret: sealed, Source: store.SourceAPI, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if _, err := db.UpdateConfig(ctx, func(ctx context.Context) error { return db.CreateConfigItem(ctx, it) }); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	enc := base64.StdEncoding.EncodeToString
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := fmt.Sprintf("store:\n  sqlite: %q\nsecrets_key: %s\n", dbPath, enc(newKey))

	tests := []struct {
		name, body string
		args       []string
		code       int
		out        string
	}{
		{name: "no subcommand", args: []string{}, code: 2, out: "usage: transponder secrets rotate"},
		{name: "unknown subcommand", args: []string{"show"}, code: 2, out: "usage: transponder secrets rotate"},
		{name: "no previous key", body: base, code: 1, out: "set previous_secrets_key"},
		{name: "a previous key the values were not sealed under", body: base + "previous_secrets_key: " + enc(bytes.Repeat([]byte{3}, 32)) + "\n", code: 1, out: `provider "oa" api_key`},
		{name: "rotates", body: base + "previous_secrets_key: " + enc(oldKey) + "\n", code: 0, out: "sealed 1 provider keys"},
		{name: "a second run has nothing to do", body: base + "previous_secrets_key: " + enc(oldKey) + "\n", code: 0, out: "sealed 0 provider keys"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"secrets"}, tt.args...)
			if tt.args == nil {
				args = append(args, "rotate", "--config", write(fmt.Sprintf("c%d.yaml", i), tt.body))
			}
			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code != tt.code || !strings.Contains(stdout.String()+stderr.String(), tt.out) {
				t.Fatalf("code %d, stdout %q, stderr %q; want %d and %q", code, stdout.String(), stderr.String(), tt.code, tt.out)
			}
		})
	}

	db, err = sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rc, err := admin.Runtime(ctx, db, newBox, nil)
	if err != nil || len(rc.Providers) != 1 || rc.Providers[0].APIKey != "sk-stored-1234" {
		t.Fatalf("after rotate the new key alone opens: %+v, %v", rc, err)
	}
}
