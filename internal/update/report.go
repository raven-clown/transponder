package update

import (
	"fmt"
	"io"
	"strings"
)

// Render writes a report for a person. used holds the Affects keys that match
// the running config, such as "provider:ollama", so changes touching them are
// listed first.
func (r *Report) Render(w io.Writer, project string, used map[string]bool) {
	p := &printer{w: w}
	switch {
	case r.Latest == "":
		p.line("%s %s: no releases are published yet.", project, r.Current)
		return
	case r.Dev:
		p.line("%s %s is a development build. The latest release is %s.", project, r.Current, r.Latest)
	case r.UpToDate():
		p.line("%s %s is the latest release.", project, r.Current)
		return
	default:
		n := len(r.Releases)
		p.line("%s %s is running. %s is out, %d %s ahead.", project, r.Current, r.Latest, n, plural(n, "release", "releases"))
	}
	p.line("")
	p.line("Impact: %s", impact(r.Level()))

	var mine, breaking, other []entry
	var unknown []Manifest
	for _, m := range r.Releases {
		if m.Level == LevelUnknown && len(m.Changes) == 0 {
			unknown = append(unknown, m)
			continue
		}
		for _, c := range m.Changes {
			e := entry{c, m.Version, matches(c.Affects, used)}
			switch {
			case len(e.hits) > 0:
				mine = append(mine, e)
			case c.Breaking || c.Kind == KindRemoved:
				breaking = append(breaking, e)
			default:
				other = append(other, e)
			}
		}
	}
	p.section("Affects your setup", mine)
	p.section("Breaking changes, read before upgrading", breaking)
	p.section("Other changes", other)
	for _, m := range unknown {
		p.line("")
		p.line("%s published no change summary, see %s", m.Version, m.URL)
	}
	latest := r.Releases[len(r.Releases)-1]
	p.line("")
	p.line("Release notes: %s", latest.URL)
	p.line("Upgrade: docker pull ghcr.io/raven-clown/%s:%s, or download the binary from the release notes.", project, latest.Version)
}

type entry struct {
	Change
	version string
	hits    []string
}

func matches(affects []string, used map[string]bool) []string {
	var hits []string
	for _, a := range affects {
		if used[a] {
			hits = append(hits, a)
		}
	}
	return hits
}

// impact turns a level into advice on how soon to upgrade.
func impact(level string) string {
	switch level {
	case LevelSecurity:
		return "security fixes. Upgrade soon."
	case LevelBreaking:
		return "breaking changes. Read them below before you upgrade."
	case LevelFix:
		return "bug fixes. Safe to upgrade."
	case LevelFeature:
		return "new features. Safe to upgrade."
	}
	return "some releases published no summary. Check their notes before you upgrade."
}

type printer struct{ w io.Writer }

func (p *printer) line(format string, args ...any) {
	_, _ = fmt.Fprintf(p.w, format+"\n", args...)
}

func (p *printer) section(title string, entries []entry) {
	if len(entries) == 0 {
		return
	}
	p.line("")
	p.line("%s (%d):", title, len(entries))
	for _, e := range entries {
		kind := e.Kind
		if e.Breaking {
			kind += "!"
		}
		ref := e.version
		if e.PR > 0 {
			ref = fmt.Sprintf("%s #%d", e.version, e.PR)
		}
		p.line("  %-11s %s (%s)", kind, oneLine(e.Text), ref)
		if len(e.hits) > 0 {
			p.line("  %-11s you use %s", "", strings.Join(e.hits, ", "))
		}
	}
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
