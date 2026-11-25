// Package auth identifies callers.
package auth

import (
	"errors"
	"fmt"
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

// Reason says why credentials were refused. The set is fixed, so it can be a metric label.
type Reason string

// The reasons credentials can be refused for.
const (
	ReasonMissing       Reason = "missing"
	ReasonUnknownKey    Reason = "unknown_key"
	ReasonDisabledKey   Reason = "disabled_key"
	ReasonExpiredKey    Reason = "expired_key"
	ReasonBadToken      Reason = "bad_token"
	ReasonWrongIssuer   Reason = "wrong_issuer"
	ReasonWrongAudience Reason = "wrong_audience"
	ReasonExpiredToken  Reason = "expired_token"
)

// Reasons lists every Reason.
var Reasons = []Reason{
	ReasonMissing, ReasonUnknownKey, ReasonDisabledKey, ReasonExpiredKey,
	ReasonBadToken, ReasonWrongIssuer, ReasonWrongAudience, ReasonExpiredToken,
}

// Refusal is the error for credentials that were refused. It wraps ErrUnauthenticated and
// names the reason. Its text may quote the token's claims, never the token itself.
type Refusal struct {
	Reason Reason
	msg    string
	cause  error
}

// Error implements error.
func (e *Refusal) Error() string { return ErrUnauthenticated.Error() + ": " + e.msg }

// Unwrap lets errors.Is match ErrUnauthenticated and the cause, when there is one.
func (e *Refusal) Unwrap() []error {
	if e.cause != nil {
		return []error{ErrUnauthenticated, e.cause}
	}
	return []error{ErrUnauthenticated}
}

// ReasonOf returns why err refused the credentials, when a refusal is in its chain.
func ReasonOf(err error) (Reason, bool) {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Reason, true
	}
	return "", false
}

func refuse(reason Reason, format string, args ...any) error {
	return &Refusal{Reason: reason, msg: fmt.Sprintf(format, args...)}
}

// Identity is the caller of a request. User is the user id from the token, or the key name
// for an API key. Key is the key name, empty when the caller signed in with a token.
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
	oidc *OIDC
}

// NewAuthenticator returns an Authenticator that looks keys up in keys.
func NewAuthenticator(keys store.KeyStore, now func() time.Time) *Authenticator {
	return &Authenticator{keys: keys, now: now}
}

// WithOIDC makes a also accept tokens that o verifies. API keys keep working next to them.
func (a *Authenticator) WithOIDC(o *OIDC) *Authenticator {
	a.oidc = o
	return a
}

// Authenticate reads Authorization: Bearer or x-api-key and returns the caller. A JWT is
// checked against the OIDC issuer when one is configured; anything else is looked up as an
// API key. The error wraps ErrUnauthenticated when the credentials are missing or not valid,
// and ErrUnavailable when the identity provider's keys could not be fetched.
func (a *Authenticator) Authenticate(r *http.Request) (Identity, error) {
	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Api-Key"))
	}
	if token == "" {
		return Identity{}, refuse(ReasonMissing, "no API key or token was sent")
	}
	if a.oidc != nil && looksLikeJWT(token) {
		return a.oidc.Verify(r.Context(), token)
	}
	k, err := a.keys.KeyByHash(r.Context(), HashKey(token))
	if errors.Is(err, store.ErrNotFound) {
		return Identity{}, refuse(ReasonUnknownKey, "no API key matches")
	}
	if err != nil {
		return Identity{}, err
	}
	if k.Disabled {
		return Identity{}, refuse(ReasonDisabledKey, "the API key is disabled")
	}
	if !k.ExpiresAt.IsZero() && !k.ExpiresAt.After(a.now()) {
		return Identity{}, refuse(ReasonExpiredKey, "the API key expired at %s", k.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return Identity{OrgID: k.OrgID, User: k.Name, Teams: k.Teams, Key: k.Name, Role: keyRole(k.Role)}, nil
}

// keyRole is the role of a stored key. A key with no role, or one this version does not
// know, is a member: it can call the gateway as before and nothing more.
func keyRole(s string) Role {
	r, err := ParseRole(s)
	if err != nil {
		return RoleMember
	}
	return r
}

func bearer(h string) string {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}
