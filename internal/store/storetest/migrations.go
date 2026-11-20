package storetest

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// contracting matches statements that take something away from the schema or tighten it, which
// a replica still running the previous version would trip over.
var contracting = regexp.MustCompile(`(?i)\b(DROP\s+(TABLE|COLUMN|INDEX)|RENAME\s+(TO|COLUMN)|ALTER\s+COLUMN|SET\s+NOT\s+NULL|ADD\s+CONSTRAINT)\b`)

// CheckMigrations fails when a migration breaks expand and contract. During a rolling update
// version N and N+1 run against one schema, so a migration may only add: tables, indexes, and
// columns that are nullable or have a default. Taking something away is a later migration, once
// no running version reads it, and its first line must say so: "-- contract: <what and why>".
// Files in released name the migrations that shipped before the rule and are left alone.
func CheckMigrations(t *testing.T, fsys fs.FS, released ...string) {
	t.Helper()
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	skip := map[string]bool{}
	for _, n := range released {
		skip[n] = true
	}
	for _, name := range names {
		if skip[name] {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			t.Fatal(err)
		}
		if msg := checkMigration(string(body)); msg != "" {
			t.Errorf("migration %s: %s", name, msg)
		}
	}
}

func checkMigration(body string) string {
	first, _, _ := strings.Cut(body, "\n")
	if reason, ok := strings.CutPrefix(strings.TrimSpace(first), "-- contract:"); ok {
		if strings.TrimSpace(reason) == "" {
			return "the contract line must say what is removed and why it is safe"
		}
		return ""
	}
	if m := contracting.FindString(body); m != "" {
		return "has " + strings.ToUpper(strings.Join(strings.Fields(m), " ")) + ", which a replica on the previous version cannot run against; " +
			`only add in this migration, and remove in a later one that starts with "-- contract: <what and why>"`
	}
	for stmt := range strings.SplitSeq(body, ";") {
		upper := strings.ToUpper(stmt)
		if strings.Contains(upper, "ADD COLUMN") && strings.Contains(upper, "NOT NULL") && !strings.Contains(upper, "DEFAULT") {
			return "adds a NOT NULL column with no default, so inserts from the previous version would fail"
		}
	}
	return ""
}
