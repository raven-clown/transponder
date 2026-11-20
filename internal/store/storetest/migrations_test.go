package storetest

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestCheckMigration(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"new table", "CREATE TABLE t (id INTEGER);\nCREATE INDEX t_id ON t (id);", ""},
		{"nullable column", "ALTER TABLE t ADD COLUMN note TEXT;", ""},
		{"column with default", "ALTER TABLE t ADD COLUMN n INTEGER NOT NULL DEFAULT 0;", ""},
		{"column without default", "ALTER TABLE t ADD COLUMN n INTEGER NOT NULL;", "NOT NULL column with no default"},
		{"drop column", "ALTER TABLE t DROP COLUMN old;", "DROP COLUMN"},
		{"drop table", "drop   table t;", "DROP TABLE"},
		{"rename column", "ALTER TABLE t RENAME COLUMN a TO b;", "RENAME COLUMN"},
		{"rename table", "ALTER TABLE t RENAME TO u;", "RENAME TO"},
		{"tighten", "ALTER TABLE t ALTER COLUMN a SET NOT NULL;", "ALTER COLUMN"},
		{"marked contract", "-- contract: drop usage.user, unread since v0.2.0\nALTER TABLE usage DROP COLUMN user;", ""},
		{"empty contract reason", "-- contract:\nALTER TABLE usage DROP COLUMN user;", "must say what is removed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkMigration(tt.body)
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
				t.Fatalf("checkMigration = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

func TestCheckMigrationsSkipsReleased(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_init.sql":   {Data: []byte("CREATE TABLE t (a INTEGER);")},
		"0002_rename.sql": {Data: []byte("ALTER TABLE t RENAME COLUMN a TO b;")},
	}
	CheckMigrations(t, fsys, "0002_rename.sql")
}
