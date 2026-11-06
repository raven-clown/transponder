// Package update describes what each release changed and tells a running
// build which newer releases exist, how much they matter and which of their
// changes touch its own config. It only reads release data and never replaces
// the binary.
package update

import (
	"strconv"
	"strings"
)

// ManifestName is the release asset that carries a Manifest.
const ManifestName = "release.json"

// Change kinds follow the CHANGELOG headings.
const (
	KindSecurity   = "security"
	KindAdded      = "added"
	KindChanged    = "changed"
	KindDeprecated = "deprecated"
	KindRemoved    = "removed"
	KindFixed      = "fixed"
)

// Release levels, from the most to the least urgent.
const (
	LevelSecurity = "security"
	LevelBreaking = "breaking"
	LevelFix      = "fix"
	LevelFeature  = "feature"
	LevelUnknown  = "unknown"
)

// Manifest is the machine-readable summary of one release.
type Manifest struct {
	Schema          int           `json:"schema"`
	Project         string        `json:"project"`
	Version         string        `json:"version"`
	Previous        string        `json:"previous,omitempty"`
	Date            string        `json:"date"`
	URL             string        `json:"url"`
	Level           string        `json:"level"`
	Changes         []Change      `json:"changes"`
	NewContributors []Contributor `json:"new_contributors,omitempty"`
}

// Contributor is someone whose first merged pull request is in the release.
type Contributor struct {
	Login string `json:"login"`
	PR    int    `json:"pr"`
}

// Change is one CHANGELOG entry with where it came from and what it touches.
// Affects holds keys such as "provider:ollama" or "endpoint:/v1/embeddings".
type Change struct {
	Kind     string   `json:"kind"`
	Text     string   `json:"text"`
	PR       int      `json:"pr,omitempty"`
	Author   string   `json:"author,omitempty"`
	Breaking bool     `json:"breaking,omitempty"`
	Affects  []string `json:"affects,omitempty"`
}

// LevelOf ranks a set of changes: any security entry makes a security
// release, then any breaking one, then any fix, then anything else.
func LevelOf(changes []Change) string {
	level := LevelFeature
	if len(changes) == 0 {
		return LevelUnknown
	}
	for _, c := range changes {
		switch {
		case c.Kind == KindSecurity:
			return LevelSecurity
		case c.Breaking || c.Kind == KindRemoved:
			level = LevelBreaking
		case c.Kind == KindFixed && level == LevelFeature:
			level = LevelFix
		}
	}
	return level
}

func levelRank(level string) int {
	switch level {
	case LevelSecurity:
		return 4
	case LevelBreaking:
		return 3
	case LevelFix:
		return 2
	case LevelFeature:
		return 1
	}
	return 0
}

// semver is a parsed vMAJOR.MINOR.PATCH. Pre-release suffixes are not parsed,
// since only full releases are compared.
type semver [3]int

func parseVersion(v string) (semver, bool) {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var s semver
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		s[i] = n
	}
	return s, true
}

func (a semver) less(b semver) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
