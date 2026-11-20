package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docsWithConfig are the pages whose YAML examples are transponder config files.
var docsWithConfig = []string{"../../README.md", "../../docs/configuration.md"}

// otherFile names the file an example is for on its first line, such as "# prometheus.yml" for a
// Prometheus scrape config. Only examples for transponder's own file are loaded.
var otherFile = regexp.MustCompile(`^# ([a-z]+)\.ya?ml\b`)

func otherProgram(body string) bool {
	m := otherFile.FindStringSubmatch(body)
	return m != nil && m[1] != "transponder"
}

var envName = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)

type yamlExample struct {
	line int
	body string
}

func yamlExamples(t *testing.T, path string) []yamlExample {
	t.Helper()
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []yamlExample
	var cur *yamlExample
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		switch {
		case cur == nil && strings.TrimSpace(line) == "```yaml":
			cur = &yamlExample{line: n + 1}
		case cur != nil && strings.TrimSpace(line) == "```":
			if !otherProgram(cur.body) {
				out = append(out, *cur)
			}
			cur = nil
		case cur != nil:
			cur.body += line + "\n"
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestDocsConfigExamples runs every config example in the docs through --config-test, with the
// variables it names set, so an example cannot drift from what transponder accepts.
func TestDocsConfigExamples(t *testing.T) {
	for _, doc := range docsWithConfig {
		examples := yamlExamples(t, doc)
		if len(examples) == 0 {
			t.Fatalf("%s has no YAML examples", doc)
		}
		for _, ex := range examples {
			t.Run(fmt.Sprintf("%s:%d", filepath.Base(doc), ex.line), func(t *testing.T) {
				for _, m := range envName.FindAllStringSubmatch(ex.body, -1) {
					t.Setenv(m[1], "placeholder")
				}
				path := filepath.Join(t.TempDir(), "transponder.yaml")
				if err := os.WriteFile(path, []byte(ex.body), 0o600); err != nil {
					t.Fatal(err)
				}
				var stdout, stderr bytes.Buffer
				if code := run([]string{"--config-test", "--config", path}, &stdout, &stderr); code != 0 {
					t.Fatalf("%s:%d does not load: %s", doc, ex.line, stderr.String())
				}
			})
		}
	}
}
