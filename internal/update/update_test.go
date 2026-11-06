package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestLevelOf(t *testing.T) {
	tests := []struct {
		name    string
		changes []Change
		want    string
	}{
		{"nothing", nil, LevelUnknown},
		{"feature", []Change{{Kind: KindAdded}}, LevelFeature},
		{"fix wins over feature", []Change{{Kind: KindAdded}, {Kind: KindFixed}}, LevelFix},
		{"breaking wins over fix", []Change{{Kind: KindFixed}, {Kind: KindChanged, Breaking: true}, {Kind: KindFixed}}, LevelBreaking},
		{"removed is breaking", []Change{{Kind: KindRemoved}}, LevelBreaking},
		{"security wins", []Change{{Kind: KindChanged, Breaking: true}, {Kind: KindSecurity}}, LevelSecurity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LevelOf(tt.changes); got != tt.want {
				t.Fatalf("LevelOf = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseVersion(t *testing.T) {
	a, ok := parseVersion("v0.2.10")
	if !ok || a != (semver{0, 2, 10}) {
		t.Fatalf("parseVersion = %v %v", a, ok)
	}
	b, _ := parseVersion("0.10.0")
	if !a.less(b) || b.less(a) || a.less(a) {
		t.Fatal("ordering is wrong")
	}
	for _, bad := range []string{"dev", "v1.2", "v1.2.x", "v1.-2.3", "v1.2.3-rc1"} {
		if _, ok := parseVersion(bad); ok {
			t.Fatalf("parseVersion(%q) accepted", bad)
		}
	}
}

const changelogDiff = `diff --git a/CHANGELOG.md b/CHANGELOG.md
--- a/CHANGELOG.md
+++ b/CHANGELOG.md
@@ -1,12 +1,17 @@
 # Changelog

 ## [Unreleased]

+### Added
+
+- LiteLLM Proxy provider, defaulting to
+  ` + "`http://localhost:4000/v1`" + `.
+
 ### Fixed

+- Plain error messages.
 - An older fix that was already there.
-- A line someone removed.

 ## [0.2.0] - 2026-11-06

 ### Added

+- A line added to a released section is ignored.
`

func TestParseChangelogDiff(t *testing.T) {
	got := ParseChangelogDiff(changelogDiff)
	want := []Change{
		{Kind: KindAdded, Text: "LiteLLM Proxy provider, defaulting to `http://localhost:4000/v1`."},
		{Kind: KindFixed, Text: "Plain error messages."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseChangelogDiffUnknownHeading(t *testing.T) {
	got := ParseChangelogDiff(" ## [Unreleased]\n+### Improved\n+- Faster.\n+- Smaller.\n")
	if len(got) != 2 || got[0].Kind != KindChanged || got[1].Text != "Smaller." {
		t.Fatalf("got %+v", got)
	}
}

func TestParseAffects(t *testing.T) {
	body := "## transponder checks\n\n<!-- Endpoints this touches: /v1/example -->\n" +
		"Endpoints this touches: `/v1/chat/completions`, /v1/Embeddings\n" +
		"Providers this touches: none\n" +
		"providers this touches: ollama <!-- echo, openai -->\n"
	got := ParseAffects(body)
	want := []string{"endpoint:/v1/chat/completions", "endpoint:/v1/embeddings", "provider:ollama"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if ParseAffects("no checks section") != nil {
		t.Fatal("expected nothing")
	}
}

func TestIsBreaking(t *testing.T) {
	for title, want := range map[string]bool{
		"feat(config)!: split timeouts": true,
		"feat!: drop v0 config":         true,
		"feat(config): add a timeout":   false,
		"fix: handle a ! in the text":   false,
	} {
		if IsBreaking(title) != want {
			t.Fatalf("IsBreaking(%q) = %v", title, !want)
		}
	}
}

type fakeSource struct {
	commits []Commit
	diffs   map[string]string
	prs     map[int]PR
	first   map[string]int
	err     error
}

func (f *fakeSource) Commits(context.Context, string, string) ([]Commit, error) {
	return f.commits, f.err
}

func (f *fakeSource) ChangelogDiff(_ context.Context, sha string) (string, error) {
	return f.diffs[sha], nil
}

func (f *fakeSource) PR(_ context.Context, n int) (PR, error) {
	pr, ok := f.prs[n]
	if !ok {
		return PR{}, errors.New("not found")
	}
	return pr, nil
}

func (f *fakeSource) FirstMergedPR(_ context.Context, author string) (int, error) {
	return f.first[author], nil
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		commits: []Commit{
			{SHA: "a", Subject: "feat(provider): add LiteLLM Proxy support (#78)"},
			{SHA: "b", Subject: "docs(readme): add a diagram (#74)"},
			{SHA: "c", Subject: "feat(config)!: split the timeout (#81)"},
			{SHA: "d", Subject: "fix(auth): check the key (#90)"},
			{SHA: "e", Subject: "chore(release): v0.2.0 (#82)"},
			{SHA: "f", Subject: "fix: direct push without a pull request"},
		},
		diffs: map[string]string{
			"a": " ## [Unreleased]\n+### Added\n+- LiteLLM provider.\n",
			"b": " # Changelog\n",
			"c": " ## [Unreleased]\n+### Changed\n+- `settings.timeout` is now two settings.\n",
			"d": " ## [Unreleased]\n+### Fixed\n+- Keys are compared in constant time.\n",
			"e": " ## [Unreleased]\n+### Added\n+- Should be skipped.\n",
			"f": " ## [Unreleased]\n+### Fixed\n+- Direct fix.\n",
		},
		prs: map[int]PR{
			78: {Number: 78, Title: "feat(provider): add LiteLLM Proxy support", Author: "nikhil-mewar", Body: "Providers this touches: litellm"},
			81: {Number: 81, Title: "feat(config)!: split the timeout", Author: "raven-clown"},
			90: {Number: 90, Title: "fix(auth): check the key", Author: "Satvik-jain", Labels: []string{"security"}, Body: "Endpoints this touches: /admin/v1"},
		},
		first: map[string]int{"nikhil-mewar": 78, "Satvik-jain": 75},
	}
}

func TestBuild(t *testing.T) {
	m, err := Build(context.Background(), newFakeSource(), BuildOptions{
		Project: "transponder", Repo: "raven-clown/transponder", Version: "v0.2.1", Previous: "v0.2.0",
		Date: "2026-11-07", Maintainers: []string{"raven-clown"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{
		{Kind: KindAdded, Text: "LiteLLM provider.", PR: 78, Author: "nikhil-mewar", Affects: []string{"provider:litellm"}},
		{Kind: KindChanged, Text: "`settings.timeout` is now two settings.", PR: 81, Author: "raven-clown", Breaking: true},
		{Kind: KindSecurity, Text: "Keys are compared in constant time.", PR: 90, Author: "Satvik-jain", Affects: []string{"endpoint:/admin/v1"}},
		{Kind: KindFixed, Text: "Direct fix."},
	}
	if !reflect.DeepEqual(m.Changes, want) {
		t.Fatalf("changes\n got %+v\nwant %+v", m.Changes, want)
	}
	if m.Level != LevelSecurity || m.URL != "https://github.com/raven-clown/transponder/releases/tag/v0.2.1" || m.Schema != 1 {
		t.Fatalf("manifest %+v", m)
	}
	if !reflect.DeepEqual(m.NewContributors, []Contributor{{Login: "nikhil-mewar", PR: 78}}) {
		t.Fatalf("new contributors %+v", m.NewContributors)
	}
}

func TestBuildErrors(t *testing.T) {
	src := newFakeSource()
	src.err = errors.New("git failed")
	if _, err := Build(context.Background(), src, BuildOptions{}); err == nil {
		t.Fatal("expected the commits error")
	}
	src = newFakeSource()
	delete(src.prs, 78)
	if _, err := Build(context.Background(), src, BuildOptions{}); err == nil || !strings.Contains(err.Error(), "#78") {
		t.Fatalf("err = %v", err)
	}
}

func TestNotes(t *testing.T) {
	m := Manifest{
		Version: "v0.2.1", Previous: "v0.2.0", URL: "https://github.com/raven-clown/transponder/releases/tag/v0.2.1",
		Changes: []Change{
			{Kind: KindAdded, Text: "LiteLLM provider.", PR: 78, Author: "nikhil-mewar"},
			{Kind: KindChanged, Text: "Two timeouts.", PR: 81, Author: "raven-clown", Breaking: true},
			{Kind: KindFixed, Text: "Direct fix."},
			{Kind: KindFixed, Text: "Bot fix.", PR: 5},
		},
		NewContributors: []Contributor{{Login: "nikhil-mewar", PR: 78}},
	}
	want := `## What's Changed

### Changed

- **Breaking:** Two timeouts. by @raven-clown in #81

### Added

- LiteLLM provider. by @nikhil-mewar in #78

### Fixed

- Direct fix.
- Bot fix. in #5

## New Contributors

- @nikhil-mewar made their first contribution in #78

**Full Changelog**: https://github.com/raven-clown/transponder/compare/v0.2.0...v0.2.1
`
	if got := Notes(m); got != want {
		t.Fatalf("notes\n%s\nwant\n%s", got, want)
	}
}

// releaseServer serves a release list and the manifests named in it.
func releaseServer(t *testing.T, manifests map[string]Manifest, tags ...string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/raven-clown/transponder/releases":
			var list []map[string]any
			for _, tag := range tags {
				rel := map[string]any{"tag_name": tag, "html_url": "https://example.test/" + tag, "published_at": "2026-11-07T10:00:00Z"}
				if tag == "v9.0.0-rc1" {
					rel["prerelease"] = true
				}
				if _, ok := manifests[tag]; ok {
					rel["assets"] = []map[string]string{
						{"name": "transponder_linux_amd64.tar.gz", "browser_download_url": srv.URL + "/nope"},
						{"name": ManifestName, "browser_download_url": srv.URL + "/assets/" + tag},
					}
				}
				list = append(list, rel)
			}
			_ = json.NewEncoder(w).Encode(list)
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			_ = json.NewEncoder(w).Encode(manifests[strings.TrimPrefix(r.URL.Path, "/assets/")])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func check(t *testing.T, srv *httptest.Server, current string) *Report {
	t.Helper()
	c := &Checker{Repo: "raven-clown/transponder", APIBase: srv.URL, Client: srv.Client()}
	rep, err := c.Check(context.Background(), current)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestCheckAndRender(t *testing.T) {
	manifests := map[string]Manifest{
		"v0.2.1": {Version: "v0.2.1", Changes: []Change{
			{Kind: KindFixed, Text: "Ollama keys\n are sent.", PR: 75, Affects: []string{"provider:ollama"}},
			{Kind: KindAdded, Text: "LiteLLM provider.", PR: 78, Affects: []string{"provider:litellm"}},
		}},
		"v0.3.0": {Version: "v0.3.0", Level: LevelBreaking, URL: "https://example.test/notes", Changes: []Change{
			{Kind: KindChanged, Text: "Two timeouts.", PR: 81, Breaking: true},
		}},
	}
	srv := releaseServer(t, manifests, "v0.3.0", "v9.0.0-rc1", "nightly", "v0.2.1", "v0.2.0", "v0.2.2")
	rep := check(t, srv, "v0.2.0")
	if rep.Latest != "v0.3.0" || len(rep.Releases) != 3 || rep.Level() != LevelBreaking || rep.UpToDate() || rep.Dev {
		t.Fatalf("report %+v", rep)
	}
	if rep.Releases[0].Level != LevelFix || rep.Releases[0].URL != "https://example.test/v0.2.1" {
		t.Fatalf("v0.2.1 manifest %+v", rep.Releases[0])
	}
	var out bytes.Buffer
	rep.Render(&out, "transponder", map[string]bool{"provider:ollama": true})
	want := `transponder v0.2.0 is running. v0.3.0 is out, 3 releases ahead.

Impact: breaking changes. Read them below before you upgrade.

Affects your setup (1):
  fixed       Ollama keys are sent. (v0.2.1 #75)
              you use provider:ollama

Breaking changes, read before upgrading (1):
  changed!    Two timeouts. (v0.3.0 #81)

Other changes (1):
  added       LiteLLM provider. (v0.2.1 #78)

v0.2.2 published no change summary, see https://example.test/v0.2.2

Release notes: https://example.test/notes
Upgrade: docker pull ghcr.io/raven-clown/transponder:v0.3.0, or download the binary from the release notes.
`
	if out.String() != want {
		t.Fatalf("render\n%s\nwant\n%s", out.String(), want)
	}
}

func TestRenderStates(t *testing.T) {
	srv := releaseServer(t, map[string]Manifest{"v0.2.1": {Version: "v0.2.1", Changes: []Change{{Kind: KindAdded, Text: "New."}}}}, "v0.2.0", "v0.2.1")
	tests := []struct{ current, want string }{
		{"v0.2.1", "transponder v0.2.1 is the latest release.\n"},
		{"dev", "transponder dev is a development build. The latest release is v0.2.1.\n\nImpact: new features. Safe to upgrade."},
		{"v0.2.0", "1 release ahead"},
	}
	for _, tt := range tests {
		var out bytes.Buffer
		check(t, srv, tt.current).Render(&out, "transponder", nil)
		if !strings.Contains(out.String(), tt.want) {
			t.Fatalf("%s:\n%s\nwant it to contain %q", tt.current, out.String(), tt.want)
		}
	}
	empty := releaseServer(t, nil)
	var out bytes.Buffer
	check(t, empty, "v0.1.0").Render(&out, "transponder", nil)
	if out.String() != "transponder v0.1.0: no releases are published yet.\n" {
		t.Fatalf("got %q", out.String())
	}
}

func TestCheckFailures(t *testing.T) {
	var assetURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/a/b/releases":
			http.Error(w, "rate limited", http.StatusForbidden)
		case "/repos/a/huge/releases":
			_, _ = fmt.Fprint(w, `"`+strings.Repeat("x", maxBody+1)+`"`)
		case "/repos/a/bad-asset/releases":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"tag_name": "v1.0.0", "html_url": "u",
				"assets": []map[string]string{{"name": ManifestName, "browser_download_url": assetURL}}}})
		case "/asset":
			_, _ = fmt.Fprint(w, `{"version":"v2.0.0"}`)
		}
	}))
	defer srv.Close()
	assetURL = srv.URL + "/asset"
	for _, repo := range []string{"a/b", "a/huge"} {
		c := &Checker{Repo: repo, APIBase: srv.URL, Client: srv.Client()}
		if _, err := c.Check(context.Background(), "v0.1.0"); err == nil {
			t.Fatalf("%s: expected an error", repo)
		}
	}
	c := &Checker{Repo: "a/bad-asset", APIBase: srv.URL, Client: srv.Client()}
	rep, err := c.Check(context.Background(), "v0.1.0")
	if err != nil || len(rep.Releases) != 1 || rep.Releases[0].Level != LevelUnknown {
		t.Fatalf("a manifest for another version must be ignored: %+v %v", rep, err)
	}
	if rep.Level() != LevelUnknown || !strings.Contains(impact(rep.Level()), "no summary") {
		t.Fatalf("level %q", rep.Level())
	}
}

func TestImpactWording(t *testing.T) {
	for _, level := range []string{LevelSecurity, LevelFix} {
		if impact(level) == impact(LevelUnknown) {
			t.Fatalf("%s has no wording of its own", level)
		}
	}
}

func TestPreviousTag(t *testing.T) {
	tags := []string{"v0.1.0", "v0.2.0", " v0.1.10", "v0.3.0", "nightly", "v0.2.0-rc1"}
	for version, want := range map[string]string{"v0.3.0": "v0.2.0", "v0.2.0": "v0.1.10", "v0.1.0": "", "dev": ""} {
		if got := PreviousTag(tags, version); got != want {
			t.Fatalf("PreviousTag(%s) = %q, want %q", version, got, want)
		}
	}
}
