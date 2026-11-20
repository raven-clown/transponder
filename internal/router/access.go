package router

import (
	"fmt"
	"slices"
	"strings"

	"github.com/raven-clown/transponder/internal/auth"
)

// Access says who may call a route. The zero value lets every authenticated caller in.
type Access struct {
	entries []string
	users   map[string]bool
	teams   map[string]bool
	keys    map[string]bool
	roles   []auth.Role
}

// ParseAccess reads an allow list. Each entry is user:<id>, team:<name>, key:<name> or
// role:<admin|viewer|member>. An empty list lets everyone in.
func ParseAccess(allow []string) (Access, error) {
	var a Access
	for _, entry := range allow {
		kind, name, _ := strings.Cut(entry, ":")
		if name == "" {
			return Access{}, fmt.Errorf("allow entry %q must be user:<id>, team:<name>, key:<name> or role:<role>", entry)
		}
		switch kind {
		case "user":
			a.users = add(a.users, name)
		case "team":
			a.teams = add(a.teams, name)
		case "key":
			a.keys = add(a.keys, name)
		case "role":
			role, err := auth.ParseRole(name)
			if err != nil {
				return Access{}, fmt.Errorf("allow entry %q: %w", entry, err)
			}
			a.roles = append(a.roles, role)
		default:
			return Access{}, fmt.Errorf("allow entry %q must be user:<id>, team:<name>, key:<name> or role:<role>", entry)
		}
		a.entries = append(a.entries, entry)
	}
	return a, nil
}

func add(set map[string]bool, name string) map[string]bool {
	if set == nil {
		set = map[string]bool{}
	}
	set[name] = true
	return set
}

// Open reports whether every authenticated caller may use the route.
func (a Access) Open() bool { return len(a.entries) == 0 }

// Entries returns the allow list as it was written.
func (a Access) Entries() []string { return slices.Clone(a.entries) }

// Permits reports whether id may use the route. A role entry lets in that role and the ones
// above it, so role:viewer also admits admins.
func (a Access) Permits(id auth.Identity) bool {
	if a.Open() {
		return true
	}
	if a.users[id.User] || (id.Key != "" && a.keys[id.Key]) {
		return true
	}
	for _, team := range id.Teams {
		if a.teams[team] {
			return true
		}
	}
	for _, role := range a.roles {
		if id.Role.Allows(role) {
			return true
		}
	}
	return false
}
