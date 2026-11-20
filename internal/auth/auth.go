// Package auth identifies callers.
package auth

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

// Role is what a caller may do.
type Role string

// Roles shared by every mothership project.
const (
	RoleAdmin  Role = "admin"
	RoleViewer Role = "viewer"
	RoleMember Role = "member"
)

// ErrUnauthenticated means the request carries no valid credentials.
var ErrUnauthenticated = errors.New("auth: unauthenticated")

// Identity is the caller of a request.
type Identity struct {
	OrgID string
	User  string
	Teams []string
	Key   string
	Role  Role
}

// Authenticator checks the credentials on a request.
type Authenticator struct {
	keys store.KeyStore
	now  func() time.Time
}

// NewAuthenticator returns an Authenticator that looks keys up in keys.
func NewAuthenticator(keys store.KeyStore, now func() time.Time) *Authenticator {
	return &Authenticator{keys: keys, now: now}
}

// Authenticate reads Authorization: Bearer or x-api-key and returns the caller.
func (a *Authenticator) Authenticate(r *http.Request) (Identity, error) {
	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Api-Key"))
	}
	if token == "" {
		return Identity{}, ErrUnauthenticated
	}
	k, err := a.keys.KeyByHash(r.Context(), HashKey(token))
	if errors.Is(err, store.ErrNotFound) {
		return Identity{}, ErrUnauthenticated
	}
	if err != nil {
		return Identity{}, err
	}
	if k.Disabled || (!k.ExpiresAt.IsZero() && !k.ExpiresAt.After(a.now())) {
		return Identity{}, ErrUnauthenticated
	}
	return Identity{OrgID: k.OrgID, User: k.Name, Teams: k.Teams, Key: k.Name, Role: Role(k.Role)}, nil
}

func bearer(h string) string {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}
