package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// ErrForbidden means the caller is known but their role does not allow the action.
var ErrForbidden = errors.New("auth: forbidden")

// ParseRole returns the role named s. An empty name is a member, so a key stored without a
// role keeps the access it had before roles were checked.
func ParseRole(s string) (Role, error) {
	switch r := Role(s); r {
	case "":
		return RoleMember, nil
	case RoleAdmin, RoleViewer, RoleMember:
		return r, nil
	}
	return "", fmt.Errorf("auth: unknown role %q, want admin, member or viewer", s)
}

// rank orders the roles: an admin may do what a viewer may, and a viewer what a member may.
func (r Role) rank() int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleViewer:
		return 2
	case RoleMember:
		return 1
	}
	return 0
}

// Allows reports whether a caller with role r may do what needs the role need.
func (r Role) Allows(need Role) bool {
	return need.rank() > 0 && r.rank() >= need.rank()
}

// Require returns ErrForbidden unless the caller's role is need or higher.
func (id Identity) Require(need Role) error {
	if !id.Role.Allows(need) {
		return fmt.Errorf("%w: needs role %s", ErrForbidden, need)
	}
	return nil
}

// Subject names the caller for records such as the audit log: key:<name> for an API key,
// user:<id> otherwise.
func (id Identity) Subject() string {
	if id.Key != "" {
		return "key:" + id.Key
	}
	return "user:" + id.User
}

type identityKey struct{}

// WithIdentity returns a context that carries the caller.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// FromContext returns the caller stored by WithIdentity.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// Middleware authenticates each request and puts the caller on its context. When there is
// no valid caller, or the caller's role is below need, it calls deny with ErrUnauthenticated,
// ErrForbidden or the error of the lookup, and the next handler does not run.
func (a *Authenticator) Middleware(need Role, deny func(http.ResponseWriter, *http.Request, error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := a.Authenticate(r)
			if err == nil {
				err = id.Require(need)
			}
			if err != nil {
				deny(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
		})
	}
}
