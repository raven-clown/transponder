package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/raven-clown/transponder/internal/store"
)

// ErrUnavailable means the caller could not be checked because the identity provider's keys
// could not be fetched. It is not the caller's fault, and a retry may work.
var ErrUnavailable = errors.New("auth: identity provider unavailable")

// ErrIssuerTrailingSlash marks a refused token whose issuer differs from the configured one
// only by a trailing slash. The gateway logs that hint once at warn level.
var ErrIssuerTrailingSlash = errors.New("the two differ only by a trailing slash")

// DefaultClockSkew is how far the clocks of transponder and the identity provider may differ.
const DefaultClockSkew = time.Minute

// signingAlgs are the signatures accepted on a token. All are asymmetric: a token signed
// with a shared secret, or not signed, is never accepted.
var signingAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

// OIDCConfig says which tokens are accepted and how their claims become an identity.
type OIDCConfig struct {
	// Issuer is the iss claim a token must carry. The keys are found through its discovery
	// document unless JWKSURL is set.
	Issuer string
	// Audience must be among the aud claim of a token.
	Audience string
	// JWKSURL is where the signing keys are. Empty means the issuer's discovery document says.
	JWKSURL string
	// UserClaim names the claim that holds the user id. Empty means sub.
	UserClaim string
	// TeamsClaim names the claim that holds the user's teams. Empty means none.
	TeamsClaim string
	// RolesClaim names the claim whose values RoleMapping turns into a role.
	RolesClaim string
	// RoleMapping gives a role to a value of RolesClaim. A caller with several mapped values
	// gets the highest role, and a caller with none is a member.
	RoleMapping map[string]Role
	// ClockSkew is the allowance on exp and nbf. Zero means DefaultClockSkew; negative means none.
	ClockSkew time.Duration
	// Client fetches the discovery document and the keys. Nil means http.DefaultClient.
	Client *http.Client
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// OIDC verifies tokens from one OpenID Connect issuer.
type OIDC struct {
	cfg  OIDCConfig
	keys *keySet
}

// NewOIDC returns a verifier for cfg. It does not reach the issuer: the keys are fetched
// when the first token arrives.
func NewOIDC(cfg OIDCConfig) (*OIDC, error) {
	if err := checkHTTPURL(cfg.Issuer); err != nil {
		return nil, fmt.Errorf("auth: oidc issuer %q: %w", cfg.Issuer, err)
	}
	if cfg.Audience == "" {
		return nil, errors.New("auth: oidc needs an audience")
	}
	if cfg.JWKSURL != "" {
		if err := checkHTTPURL(cfg.JWKSURL); err != nil {
			return nil, fmt.Errorf("auth: oidc jwks url %q: %w", cfg.JWKSURL, err)
		}
	}
	for value, role := range cfg.RoleMapping {
		if role.rank() == 0 {
			return nil, fmt.Errorf("auth: oidc role mapping for %q names unknown role %q", value, role)
		}
	}
	if len(cfg.RoleMapping) > 0 && cfg.RolesClaim == "" {
		return nil, errors.New("auth: oidc role mapping needs a roles claim")
	}
	if cfg.UserClaim == "" {
		cfg.UserClaim = "sub"
	}
	switch {
	case cfg.ClockSkew == 0:
		cfg.ClockSkew = DefaultClockSkew
	case cfg.ClockSkew < 0:
		cfg.ClockSkew = 0
	}
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &OIDC{cfg: cfg, keys: &keySet{issuer: cfg.Issuer, jwksURL: cfg.JWKSURL, client: cfg.Client, now: cfg.Now}}, nil
}

// MapsAdmin reports whether some claim value makes its holder an admin.
func (o *OIDC) MapsAdmin() bool {
	for _, role := range o.cfg.RoleMapping {
		if role == RoleAdmin {
			return true
		}
	}
	return false
}

// Ping reports whether the issuer's keys have been fetched, fetching them when they have not.
func (o *OIDC) Ping(ctx context.Context) error {
	_, err := o.keys.lookup(ctx, "", false)
	return err
}

// Verify checks a token's signature, issuer, audience and validity, and returns its caller.
// A token that fails any check gives an error wrapping ErrUnauthenticated that says which.
func (o *OIDC) Verify(ctx context.Context, token string) (Identity, error) {
	jws, err := jose.ParseSignedCompact(token, signingAlgs)
	if err != nil {
		return Identity{}, rejectf("malformed token or unsupported signature: %v", err)
	}
	// The issuer is read before the signature is checked, so a token from somewhere else
	// never causes a key fetch. Nothing else is trusted until the signature holds.
	var peek struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(jws.UnsafePayloadWithoutVerification(), &peek); err != nil || peek.Issuer != o.cfg.Issuer {
		if err == nil && peek.Issuer != "" && strings.TrimSuffix(peek.Issuer, "/") == strings.TrimSuffix(o.cfg.Issuer, "/") {
			// Still refused: the issuer must match exactly. The reason spells out
			// the fix and quotes the token's value, whichever side has the slash.
			return Identity{}, fmt.Errorf("%w: issuer is %q, want %q: %w; set auth.oidc.issuer to %q",
				ErrUnauthenticated, peek.Issuer, o.cfg.Issuer, ErrIssuerTrailingSlash, peek.Issuer)
		}
		return Identity{}, rejectf("issuer is %q, want %q", peek.Issuer, o.cfg.Issuer)
	}
	header := jws.Signatures[0].Protected
	verify := func(keys []jose.JSONWebKey) ([]byte, bool) {
		for _, k := range keys {
			if k.Algorithm != "" && k.Algorithm != header.Algorithm {
				continue
			}
			if payload, err := jws.Verify(k.Key); err == nil {
				return payload, true
			}
		}
		return nil, false
	}
	keys, err := o.keys.lookup(ctx, header.KeyID, false)
	if err != nil {
		return Identity{}, err
	}
	payload, verified := verify(keys)
	// A token without kid names no key to look for, so a key the cached set lacks is only
	// found by reading the set again, as for an unknown kid.
	if !verified && header.KeyID == "" {
		if keys, err = o.keys.lookup(ctx, "", true); err != nil {
			return Identity{}, err
		}
		payload, verified = verify(keys)
	}
	if !verified {
		return Identity{}, rejectf("no key of the issuer verifies the signature (kid %q)", header.KeyID)
	}

	var claims map[string]any
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil {
		return Identity{}, rejectf("claims are not a JSON object: %v", err)
	}
	if err := o.checkClaims(claims); err != nil {
		return Identity{}, err
	}
	user, _ := lookupClaim(claims, o.cfg.UserClaim).(string)
	if user == "" {
		return Identity{}, rejectf("claim %q is missing or not a string", o.cfg.UserClaim)
	}
	id := Identity{OrgID: store.DefaultOrg, User: user, Role: RoleMember}
	if o.cfg.TeamsClaim != "" {
		id.Teams = claimStrings(lookupClaim(claims, o.cfg.TeamsClaim))
	}
	if o.cfg.RolesClaim != "" {
		for _, value := range claimStrings(lookupClaim(claims, o.cfg.RolesClaim)) {
			if role, ok := o.cfg.RoleMapping[value]; ok && role.rank() > id.Role.rank() {
				id.Role = role
			}
		}
	}
	return id, nil
}

// checkClaims checks the audience and the validity window of a token whose signature and
// issuer are already known to be good.
func (o *OIDC) checkClaims(claims map[string]any) error {
	found := false
	for _, aud := range claimStrings(claims["aud"]) {
		if aud == o.cfg.Audience {
			found = true
			break
		}
	}
	if !found {
		return rejectf("audience does not include %q", o.cfg.Audience)
	}
	now := o.cfg.Now()
	exp, ok := claimTime(claims["exp"])
	if !ok {
		return rejectf("claim exp is missing")
	}
	if !now.Before(exp.Add(o.cfg.ClockSkew)) {
		return rejectf("token expired at %s", exp.UTC().Format(time.RFC3339))
	}
	// nbf and iat are optional, but one that is present has to be a time and not ahead of now.
	for _, name := range []string{"nbf", "iat"} {
		v, present := claims[name]
		if !present {
			continue
		}
		at, ok := claimTime(v)
		if !ok {
			return rejectf("claim %s is not a time", name)
		}
		if now.Add(o.cfg.ClockSkew).Before(at) {
			if name == "iat" {
				return rejectf("token is issued in the future, at %s", at.UTC().Format(time.RFC3339))
			}
			return rejectf("token is not valid before %s", at.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

func rejectf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnauthenticated, fmt.Sprintf(format, args...))
}

// lookupClaim returns the claim called name. When there is none and name has dots, it is
// read as a path into nested objects, as in Keycloak's realm_access.roles.
func lookupClaim(claims map[string]any, name string) any {
	if v, ok := claims[name]; ok {
		return v
	}
	var cur any = claims
	for part := range strings.SplitSeq(name, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[part]
	}
	return cur
}

// claimStrings reads a claim that is one string or a list of strings.
func claimStrings(v any) []string {
	switch v := v.(type) {
	case string:
		if v != "" {
			return []string{v}
		}
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// claimTime reads a claim that is a number of seconds since 1970.
func claimTime(v any) (time.Time, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return time.Time{}, false
	}
	secs, err := n.Float64()
	// The upper bound keeps the conversion to whole seconds from overflowing.
	if err != nil || secs < 0 || secs > 1e12 {
		return time.Time{}, false
	}
	return time.Unix(int64(secs), 0), true
}

// looksLikeJWT reports whether token has the three dot-separated parts of a signed JWT. API
// keys never contain a dot.
func looksLikeJWT(token string) bool {
	return !strings.HasPrefix(token, keyPrefix) && strings.Count(token, ".") == 2
}
