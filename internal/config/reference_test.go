package config

import (
	"bufio"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/raven-clown/transponder/internal/limits"
)

const referencePath = "../../docs/configuration.md"

// referenceRow is one row of a table in docs/configuration.md.
type referenceRow struct {
	cells []string
	line  int
}

// referenceTables returns the rows of every table whose header starts with header, by first cell.
func referenceTables(t *testing.T, header string) map[string]referenceRow {
	t.Helper()
	f, err := os.Open(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows := map[string]referenceRow{}
	in := false
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, header):
			in = true
		case !strings.HasPrefix(line, "|"):
			in = false
		case in && !strings.HasPrefix(line, "|---"):
			cells := strings.Split(strings.Trim(line, "|"), "|")
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			name := strings.Trim(cells[0], "`")
			if _, dup := rows[name]; dup {
				t.Errorf("%s:%d: %s is listed twice", referencePath, n, name)
			}
			rows[name] = referenceRow{cells: cells, line: n}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

// configKeys lists every key of the file by its dotted path, with [] for a list of items.
func configKeys(typ reflect.Type, prefix string, out map[string]reflect.Type) {
	for f := range typ.Fields() {
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		path := prefix + tag
		switch {
		case f.Type.Kind() == reflect.Struct:
			configKeys(f.Type, path+".", out)
		case f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.Struct:
			configKeys(f.Type.Elem(), path+"[].", out)
		default:
			out[path] = f.Type
		}
	}
}

// valueAt reads the field at a dotted path that has no list in it.
func valueAt(v reflect.Value, path string) reflect.Value {
	for _, part := range strings.Split(path, ".") {
		for i := range v.NumField() {
			tag, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("yaml"), ",")
			if tag == part {
				v = v.Field(i)
				break
			}
		}
	}
	return v
}

// literal writes a default the way the reference does: durations as 1m or 30s.
func literal(v reflect.Value) string {
	if d, ok := v.Interface().(time.Duration); ok {
		s := d.String()
		if strings.HasSuffix(s, "m0s") {
			s = strings.TrimSuffix(s, "0s")
		}
		if strings.HasSuffix(s, "h0m") {
			s = strings.TrimSuffix(s, "0m")
		}
		return s
	}
	switch v.Kind() {
	case reflect.String:
		return v.String()
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64)
	}
	return ""
}

var oneLiteral = regexp.MustCompile("^`[^`]*`$")

// TestConfigReference keeps docs/configuration.md in step with Bootstrap: every key is listed
// once, with the environment variable that sets it and the default Load gives it.
func TestConfigReference(t *testing.T) {
	documented := referenceTables(t, "| Key | Default | Environment variable |")
	keys := map[string]reflect.Type{}
	configKeys(reflect.TypeFor[Bootstrap](), "", keys)
	envKeys := leafPaths(reflect.TypeFor[Bootstrap](), "")

	plain, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The OIDC defaults only apply once an issuer is set.
	oidc, err := Load("", []string{"TRANSPONDER_AUTH__OIDC__ISSUER=https://idp.example.com", "TRANSPONDER_AUTH__OIDC__AUDIENCE=t"})
	if err != nil {
		t.Fatal(err)
	}

	var missing []string
	for key := range keys {
		if _, ok := documented[key]; !ok {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	for _, key := range missing {
		t.Errorf("%s does not list the config key %s", referencePath, key)
	}

	for key, row := range documented {
		if _, ok := keys[key]; !ok {
			t.Errorf("%s:%d: %s is not a config key", referencePath, row.line, key)
			continue
		}
		if len(row.cells) != 4 {
			t.Errorf("%s:%d: %s has %d cells, want 4", referencePath, row.line, key, len(row.cells))
			continue
		}
		wantEnv := "file only"
		if _, ok := envKeys[strings.ReplaceAll(key, ".", "__")]; ok {
			wantEnv = "`" + envPrefix + strings.ToUpper(strings.ReplaceAll(key, ".", "__")) + "`"
		}
		if row.cells[2] != wantEnv {
			t.Errorf("%s:%d: %s is set by %s, want %s", referencePath, row.line, key, row.cells[2], wantEnv)
		}
		if strings.Contains(key, "[]") || keys[key].Kind() == reflect.Map || keys[key].Kind() == reflect.Slice {
			continue
		}
		from := plain
		if strings.HasPrefix(key, "auth.oidc.") && key != "auth.oidc.issuer" && key != "auth.oidc.audience" {
			from = oidc
		}
		v := valueAt(reflect.ValueOf(from).Elem(), key)
		want := "`" + literal(v) + "`"
		cell := row.cells[1]
		switch {
		case !v.IsZero() && cell != want:
			t.Errorf("%s:%d: %s defaults to %s, the reference says %s", referencePath, row.line, key, want, cell)
		case oneLiteral.MatchString(cell) && cell != want:
			t.Errorf("%s:%d: %s defaults to %s, the reference says %s", referencePath, row.line, key, want, cell)
		}
	}
}

// TestConfigReferenceLimits checks that every limit in the profile table, with its example, is
// one a profile accepts.
func TestConfigReferenceLimits(t *testing.T) {
	rows := referenceTables(t, "| Limit | Example |")
	if len(rows) == 0 {
		t.Fatal("no limits table")
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			var value any
			if err := yaml.Unmarshal([]byte(strings.Trim(row.cells[1], "`")), &value); err != nil {
				t.Fatal(err)
			}
			if _, err := limits.ParseProfile("p", map[string]any{name: value}); err == nil {
				return
			}
			dot := strings.LastIndex(name, ".")
			if dot < 0 {
				t.Fatalf("%s:%d: %s is not a limit a profile accepts", referencePath, row.line, name)
			}
			group := map[string]any{name[:dot]: map[string]any{name[dot+1:]: value}}
			if _, err := limits.ParseProfile("p", group); err != nil {
				t.Fatalf("%s:%d: %s: %v", referencePath, row.line, name, err)
			}
		})
	}
	if _, err := limits.ParseProfile("p", map[string]any{"requests": map[string]any{"nope": 1}}); err == nil {
		t.Fatal("a profile accepts an unknown limit, so this test proves nothing")
	}
}
