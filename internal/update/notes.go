package update

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Commit is one squash-merged commit between two tags.
type Commit struct {
	SHA     string
	Subject string
}

// PR is what a release needs from a merged pull request.
type PR struct {
	Number int
	Title  string
	Author string
	Body   string
	Labels []string
}

// Source reads the history of a repository. The release tool backs it with
// git and the GitHub API, and tests use a fake.
type Source interface {
	Commits(ctx context.Context, from, to string) ([]Commit, error)
	ChangelogDiff(ctx context.Context, sha string) (string, error)
	PR(ctx context.Context, number int) (PR, error)
	// FirstMergedPR is the number of the author's first merged pull request.
	FirstMergedPR(ctx context.Context, author string) (int, error)
}

// BuildOptions name the release being built.
type BuildOptions struct {
	Project  string
	Repo     string
	Version  string
	Previous string
	Date     string
	// Maintainers are left out of New Contributors.
	Maintainers []string
}

var prRef = regexp.MustCompile(`\(#(\d+)\)\s*$`)

// Build collects the CHANGELOG lines each commit added under Unreleased and
// tags them with the pull request, its author and what it touches.
func Build(ctx context.Context, src Source, o BuildOptions) (Manifest, error) {
	m := Manifest{
		Schema:   1,
		Project:  o.Project,
		Version:  o.Version,
		Previous: o.Previous,
		Date:     o.Date,
		URL:      "https://github.com/" + o.Repo + "/releases/tag/" + o.Version,
	}
	commits, err := src.Commits(ctx, o.Previous, o.Version)
	if err != nil {
		return m, err
	}
	authors := map[string][]int{}
	var order []string
	for _, c := range commits {
		if strings.HasPrefix(c.Subject, "chore(release)") {
			continue
		}
		diff, err := src.ChangelogDiff(ctx, c.SHA)
		if err != nil {
			return m, err
		}
		changes := ParseChangelogDiff(diff)
		if len(changes) == 0 {
			continue
		}
		number := 0
		if sm := prRef.FindStringSubmatch(c.Subject); sm != nil {
			number, _ = strconv.Atoi(sm[1])
		}
		pr := PR{Number: number, Title: c.Subject}
		if number > 0 {
			if pr, err = src.PR(ctx, number); err != nil {
				return m, fmt.Errorf("pull request #%d: %w", number, err)
			}
		}
		breaking := IsBreaking(pr.Title) || slices.Contains(pr.Labels, "breaking")
		security := slices.Contains(pr.Labels, "security")
		affects := ParseAffects(pr.Body)
		for _, ch := range changes {
			ch.PR, ch.Author, ch.Affects = number, pr.Author, affects
			ch.Breaking = ch.Breaking || breaking
			if security {
				ch.Kind = KindSecurity
			}
			m.Changes = append(m.Changes, ch)
		}
		if pr.Author != "" && number > 0 {
			if _, seen := authors[pr.Author]; !seen {
				order = append(order, pr.Author)
			}
			authors[pr.Author] = append(authors[pr.Author], number)
		}
	}
	m.Level = LevelOf(m.Changes)
	for _, a := range order {
		if slices.Contains(o.Maintainers, a) || strings.HasSuffix(a, "[bot]") {
			continue
		}
		first, err := src.FirstMergedPR(ctx, a)
		if err != nil {
			return m, err
		}
		if slices.Contains(authors[a], first) {
			m.NewContributors = append(m.NewContributors, Contributor{Login: a, PR: first})
		}
	}
	return m, nil
}

// ParseChangelogDiff reads a full-context diff of CHANGELOG.md and returns
// the entries added under Unreleased, with the kind taken from their heading.
// Continuation lines of a wrapped entry are joined to it.
func ParseChangelogDiff(diff string) []Change {
	var out []Change
	section, heading := "", ""
	last := -1
	for _, raw := range strings.Split(diff, "\n") {
		if raw == "" || strings.HasPrefix(raw, "+++") || strings.HasPrefix(raw, "---") {
			continue
		}
		mark, line := raw[0], raw[1:]
		if mark != ' ' && mark != '+' {
			continue
		}
		switch {
		case strings.HasPrefix(line, "## "):
			section, heading, last = line, "", -1
			continue
		case strings.HasPrefix(line, "### "):
			heading, last = strings.ToLower(strings.TrimSpace(line[4:])), -1
			continue
		}
		if !strings.Contains(strings.ToLower(section), "unreleased") || mark != '+' {
			if mark != '+' {
				last = -1
			}
			continue
		}
		text := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(text, "- "):
			out = append(out, Change{Kind: kindOf(heading), Text: strings.TrimSpace(text[2:])})
			last = len(out) - 1
		case text != "" && last >= 0:
			out[last].Text += " " + text
		}
	}
	return out
}

func kindOf(heading string) string {
	switch heading {
	case KindSecurity, KindAdded, KindChanged, KindDeprecated, KindRemoved, KindFixed:
		return heading
	}
	return KindChanged
}

var breakingTitle = regexp.MustCompile(`^[a-z]+(\([^)]*\))?!:`)

// IsBreaking reports whether a Conventional Commits title has the "!" marker.
func IsBreaking(title string) bool { return breakingTitle.MatchString(title) }

var touchesLine = regexp.MustCompile(`(?mi)^(endpoints|providers) this touches:(.*)$`)
var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// ParseAffects reads the "Endpoints this touches" and "Providers this
// touches" lines of a pull request body.
func ParseAffects(body string) []string {
	body = htmlComment.ReplaceAllString(body, "")
	var out []string
	for _, sm := range touchesLine.FindAllStringSubmatch(body, -1) {
		prefix := "endpoint:"
		if strings.EqualFold(sm[1], "providers") {
			prefix = "provider:"
		}
		for _, item := range strings.Split(sm[2], ",") {
			item = strings.Trim(strings.TrimSpace(item), "`")
			if item == "" || strings.EqualFold(item, "none") {
				continue
			}
			out = append(out, prefix+strings.ToLower(item))
		}
	}
	return out
}

// Notes renders release notes from a manifest.
func Notes(m Manifest) string {
	var b strings.Builder
	b.WriteString("## What's Changed\n")
	groups := []struct{ kind, title string }{
		{KindSecurity, "Security"}, {KindRemoved, "Removed"}, {KindChanged, "Changed"},
		{KindDeprecated, "Deprecated"}, {KindAdded, "Added"}, {KindFixed, "Fixed"},
	}
	for _, g := range groups {
		var lines []string
		for _, c := range m.Changes {
			if c.Kind != g.kind {
				continue
			}
			line := "- "
			if c.Breaking {
				line += "**Breaking:** "
			}
			line += c.Text
			if c.Author != "" && c.PR > 0 {
				line += fmt.Sprintf(" by @%s in #%d", c.Author, c.PR)
			} else if c.PR > 0 {
				line += fmt.Sprintf(" in #%d", c.PR)
			}
			lines = append(lines, line)
		}
		if len(lines) > 0 {
			fmt.Fprintf(&b, "\n### %s\n\n%s\n", g.title, strings.Join(lines, "\n"))
		}
	}
	if len(m.NewContributors) > 0 {
		b.WriteString("\n## New Contributors\n\n")
		for _, nc := range m.NewContributors {
			fmt.Fprintf(&b, "- @%s made their first contribution in #%d\n", nc.Login, nc.PR)
		}
	}
	if m.Previous != "" {
		repo := strings.TrimSuffix(strings.TrimPrefix(m.URL, "https://github.com/"), "/releases/tag/"+m.Version)
		fmt.Fprintf(&b, "\n**Full Changelog**: https://github.com/%s/compare/%s...%s\n", repo, m.Previous, m.Version)
	}
	return b.String()
}

// PreviousTag is the highest release tag below version, or "" for the first
// release.
func PreviousTag(tags []string, version string) string {
	cur, ok := parseVersion(version)
	if !ok {
		return ""
	}
	best, bestTag := semver{}, ""
	for _, t := range tags {
		v, ok := parseVersion(strings.TrimSpace(t))
		if ok && v.less(cur) && (bestTag == "" || best.less(v)) {
			best, bestTag = v, strings.TrimSpace(t)
		}
	}
	return bestTag
}
