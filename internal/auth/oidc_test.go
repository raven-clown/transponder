package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/raven-clown/transponder/internal/auth/authtest"
	"github.com/raven-clown/transponder/internal/store"
)

// verifier returns an OIDC for the provider, on the provider's clock, with change applied to
// its config.
func verifier(p *authtest.IDP, change func(*OIDCConfig)) *OIDC {
	cfg := OIDCConfig{
		Issuer:      p.Issuer(),
		Audience:    authtest.Audience,
		TeamsClaim:  "groups",
		RolesClaim:  "roles",
		RoleMapping: map[string]Role{"tp-admins": RoleAdmin, "tp-auditors": RoleViewer},
		Client:      p.Client(),
		Now:         p.Clock.Now,
	}
	if change != nil {
		change(&cfg)
	}
	o, err := NewOIDC(cfg)
	if err != nil {
		panic(err)
	}
	return o
}

func TestOIDCVerify(t *testing.T) {
	p := authtest.NewIDP(t)
	ec := p.NewKey("ec-1")
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.Publish("rsa-1", &rsaKey.PublicKey, "RS256")
	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p.Publish("ed-1", edPub, "")
	stranger := authtest.ECKey(t)
	o := verifier(p, nil)
	now := p.Clock.Now()

	with := func(change func(map[string]any)) map[string]any {
		c := p.Claims()
		change(c)
		return c
	}
	ecToken := func(c map[string]any) string { return authtest.Sign(t, jose.ES256, ec, "ec-1", c) }
	bothTeams := []string{"platform", "ml"}

	accepted := map[string]struct {
		token     string
		wantTeams []string
		wantRole  Role
	}{
		"valid es256":          {ecToken(p.Claims()), bothTeams, RoleViewer},
		"valid rs256":          {authtest.Sign(t, jose.RS256, rsaKey, "rsa-1", p.Claims()), bothTeams, RoleViewer},
		"valid eddsa":          {authtest.Sign(t, jose.EdDSA, edPriv, "ed-1", p.Claims()), bothTeams, RoleViewer},
		"no kid in the header": {authtest.Sign(t, jose.ES256, ec, "", p.Claims()), bothTeams, RoleViewer},
		"audience among several": {
			ecToken(with(func(c map[string]any) { c["aud"] = []string{"other", authtest.Audience} })), bothTeams, RoleViewer,
		},
		"expired within the skew": {
			ecToken(with(func(c map[string]any) { c["exp"] = now.Add(-30 * time.Second).Unix() })), bothTeams, RoleViewer,
		},
		"not yet valid within the skew": {
			ecToken(with(func(c map[string]any) { c["nbf"] = now.Add(30 * time.Second).Unix() })), bothTeams, RoleViewer,
		},
		"no teams claim": {
			ecToken(with(func(c map[string]any) { delete(c, "groups") })), nil, RoleViewer,
		},
		"teams as one string": {
			ecToken(with(func(c map[string]any) { c["groups"] = "platform" })), []string{"platform"}, RoleViewer,
		},
		"teams of mixed types": {
			ecToken(with(func(c map[string]any) { c["groups"] = []any{"platform", 7, ""} })), []string{"platform"}, RoleViewer,
		},
		"no roles claim is a member": {
			ecToken(with(func(c map[string]any) { delete(c, "roles") })), bothTeams, RoleMember,
		},
		"unmapped role is a member": {
			ecToken(with(func(c map[string]any) { c["roles"] = []string{"something-else"} })), bothTeams, RoleMember,
		},
		"highest mapped role wins": {
			ecToken(with(func(c map[string]any) { c["roles"] = []string{"tp-auditors", "x", "tp-admins"} })), bothTeams, RoleAdmin,
		},
	}
	for name, tt := range accepted {
		t.Run(name, func(t *testing.T) {
			id, err := o.Verify(context.Background(), tt.token)
			if err != nil {
				t.Fatal(err)
			}
			if id.User != "alice@example.com" || id.Role != tt.wantRole || !slices.Equal(id.Teams, tt.wantTeams) || id.Key != "" || id.OrgID != store.DefaultOrg {
				t.Fatalf("identity = %+v", id)
			}
		})
	}

	refused := map[string]string{
		"expired":           ecToken(with(func(c map[string]any) { c["exp"] = now.Add(-2 * time.Minute).Unix() })),
		"not yet valid":     ecToken(with(func(c map[string]any) { c["nbf"] = now.Add(2 * time.Minute).Unix() })),
		"wrong audience":    ecToken(with(func(c map[string]any) { c["aud"] = "logbook" })),
		"missing audience":  ecToken(with(func(c map[string]any) { delete(c, "aud") })),
		"wrong issuer":      ecToken(with(func(c map[string]any) { c["iss"] = "https://elsewhere.example.com" })),
		"missing issuer":    ecToken(with(func(c map[string]any) { delete(c, "iss") })),
		"missing exp":       ecToken(with(func(c map[string]any) { delete(c, "exp") })),
		"exp not a number":  ecToken(with(func(c map[string]any) { c["exp"] = "tomorrow" })),
		"exp out of range":  ecToken(with(func(c map[string]any) { c["exp"] = 1e30 })),
		"missing sub":       ecToken(with(func(c map[string]any) { delete(c, "sub") })),
		"sub not a string":  ecToken(with(func(c map[string]any) { c["sub"] = 42 })),
		"unknown key":       authtest.Sign(t, jose.ES256, stranger, "ec-9", p.Claims()),
		"another key's kid": authtest.Sign(t, jose.ES256, stranger, "ec-1", p.Claims()),
		"wrong alg for key": authtest.Sign(t, jose.PS256, rsaKey, "rsa-1", p.Claims()),
		"unsigned":          authtest.Handmade(t, `{"alg":"none","typ":"JWT"}`, p.Claims()),
		"shared secret":     authtest.Handmade(t, `{"alg":"HS256","typ":"JWT","kid":"ec-1"}`, p.Claims()),
		"forged signature":  authtest.Handmade(t, `{"alg":"ES256","typ":"JWT","kid":"ec-1"}`, p.Claims()),
		"not a token":       "a.b.c",
	}
	for name, token := range refused {
		t.Run(name, func(t *testing.T) {
			if _, err := o.Verify(context.Background(), token); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestOIDCRejectsIssuerThatDiffersOnlyByTrailingSlash(t *testing.T) {
	p := authtest.NewIDP(t)
	key := p.NewKey("issuer-slash")
	for _, tt := range []struct {
		name, tokenIssuer, wantIssuer string
		wantHint                      bool
	}{
		{"token has the slash", p.Issuer() + "/", p.Issuer(), true},
		{"config has the slash", p.Issuer(), p.Issuer() + "/", true},
		{"plainly different", "https://elsewhere.example.com", p.Issuer(), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			claims := p.Claims()
			claims["iss"] = tt.tokenIssuer
			token := authtest.Sign(t, jose.ES256, key, "issuer-slash", claims)
			o := verifier(p, func(c *OIDCConfig) { c.Issuer = tt.wantIssuer })
			_, err := o.Verify(context.Background(), token)
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
			got := err.Error()
			hasHint := strings.Contains(got, "trailing slash") && strings.Contains(got, tt.tokenIssuer)
			if hasHint != tt.wantHint {
				t.Fatalf("err = %q, hint = %v, want %v", got, hasHint, tt.wantHint)
			}
			if tt.wantHint && !errors.Is(err, ErrIssuerTrailingSlash) {
				t.Fatalf("err = %v, want ErrIssuerTrailingSlash", err)
			}
		})
	}
}

func TestOIDCClaimNames(t *testing.T) {
	p := authtest.NewIDP(t)
	key := p.NewKey("k")
	c := p.Claims()
	c["email"] = "alice@corp.example"
	c["realm_access"] = map[string]any{"roles": []string{"tp-admins"}, "teams": map[string]any{"names": []string{"core"}}}
	c["a.b"] = []string{"dotted"}
	token := authtest.Sign(t, jose.ES256, key, "k", c)
	tests := map[string]struct {
		change    func(*OIDCConfig)
		wantUser  string
		wantTeams []string
		wantRole  Role
	}{
		"user from another claim": {func(c *OIDCConfig) { c.UserClaim = "email" }, "alice@corp.example", []string{"platform", "ml"}, RoleViewer},
		"nested roles":            {func(c *OIDCConfig) { c.RolesClaim = "realm_access.roles" }, "alice@example.com", []string{"platform", "ml"}, RoleAdmin},
		"deeply nested teams":     {func(c *OIDCConfig) { c.TeamsClaim = "realm_access.teams.names" }, "alice@example.com", []string{"core"}, RoleViewer},
		"claim with a dot in it":  {func(c *OIDCConfig) { c.TeamsClaim = "a.b" }, "alice@example.com", []string{"dotted"}, RoleViewer},
		"path through a string":   {func(c *OIDCConfig) { c.TeamsClaim = "sub.names" }, "alice@example.com", nil, RoleViewer},
		"no teams or roles claim": {func(c *OIDCConfig) { c.TeamsClaim, c.RolesClaim, c.RoleMapping = "", "", nil }, "alice@example.com", nil, RoleMember},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			id, err := verifier(p, tt.change).Verify(context.Background(), token)
			if err != nil {
				t.Fatal(err)
			}
			if id.User != tt.wantUser || id.Role != tt.wantRole || !slices.Equal(id.Teams, tt.wantTeams) {
				t.Fatalf("identity = %+v", id)
			}
		})
	}
	t.Run("user claim missing", func(t *testing.T) {
		_, err := verifier(p, func(c *OIDCConfig) { c.UserClaim = "upn" }).Verify(context.Background(), token)
		if !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	})
}

func TestOIDCClockSkew(t *testing.T) {
	p := authtest.NewIDP(t)
	key := p.NewKey("k")
	c := p.Claims()
	c["exp"] = p.Clock.Now().Add(-30 * time.Second).Unix()
	token := authtest.Sign(t, jose.ES256, key, "k", c)
	tests := map[string]struct {
		skew    time.Duration
		wantErr bool
	}{
		"default":   {0, false},
		"longer":    {5 * time.Minute, false},
		"shorter":   {10 * time.Second, true},
		"none":      {-1, true},
		"just past": {30 * time.Second, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := verifier(p, func(c *OIDCConfig) { c.ClockSkew = tt.skew }).Verify(context.Background(), token)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestOIDCWrongIssuerFetchesNothing(t *testing.T) {
	p := authtest.NewIDP(t)
	key := p.NewKey("k")
	c := p.Claims()
	c["iss"] = "https://elsewhere.example.com"
	o := verifier(p, nil)
	for range 20 {
		if _, err := o.Verify(context.Background(), authtest.Sign(t, jose.ES256, key, "k", c)); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	}
	if p.FetchCount() != 0 || p.DiscoverCount() != 0 {
		t.Fatalf("fetches = %d, discoveries = %d, want none", p.FetchCount(), p.DiscoverCount())
	}
}

func TestOIDCUnknownKeyRefreshIsBounded(t *testing.T) {
	p := authtest.NewIDP(t)
	key := p.NewKey("k")
	stranger := authtest.ECKey(t)
	o := verifier(p, nil)
	ctx := context.Background()
	if _, err := o.Verify(ctx, authtest.Sign(t, jose.ES256, key, "k", p.Claims())); err != nil {
		t.Fatal(err)
	}
	flood := func() {
		t.Helper()
		for i := range 200 {
			token := authtest.Sign(t, jose.ES256, stranger, fmt.Sprintf("nope-%d", i), p.Claims())
			if _, err := o.Verify(ctx, token); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
		}
	}
	flood()
	if got := p.FetchCount(); got != 1 {
		t.Fatalf("fetches right after the first = %d, want 1", got)
	}
	p.Clock.Advance(jwksRefreshInterval)
	flood()
	if got := p.FetchCount(); got != 2 {
		t.Fatalf("fetches after one interval = %d, want 2", got)
	}
	p.Clock.Advance(jwksRefreshInterval - time.Second)
	flood()
	if got := p.FetchCount(); got != 2 {
		t.Fatalf("fetches inside the interval = %d, want 2", got)
	}
	if got := p.DiscoverCount(); got != 1 {
		t.Fatalf("discoveries = %d, want 1", got)
	}
	// The flood does not get in the way of a caller with a good token.
	if _, err := o.Verify(ctx, authtest.Sign(t, jose.ES256, key, "k", p.Claims())); err != nil {
		t.Fatal(err)
	}
}

func TestOIDCRotatedKey(t *testing.T) {
	p := authtest.NewIDP(t)
	oldKey := p.NewKey("2026-10")
	o := verifier(p, nil)
	ctx := context.Background()
	if _, err := o.Verify(ctx, authtest.Sign(t, jose.ES256, oldKey, "2026-10", p.Claims())); err != nil {
		t.Fatal(err)
	}

	// The provider starts signing with a new key and still publishes the old one.
	newKey := p.NewKey("2026-11")
	p.Clock.Advance(jwksRefreshInterval)
	if _, err := o.Verify(ctx, authtest.Sign(t, jose.ES256, newKey, "2026-11", p.Claims())); err != nil {
		t.Fatalf("token signed with the new key: %v", err)
	}
	if _, err := o.Verify(ctx, authtest.Sign(t, jose.ES256, oldKey, "2026-10", p.Claims())); err != nil {
		t.Fatalf("token signed with the old key while it is still published: %v", err)
	}
	if got := p.FetchCount(); got != 2 {
		t.Fatalf("fetches = %d, want 2", got)
	}

	// The provider drops the old key. Once the key set is read again, its tokens are refused.
	p.Set(func(p *authtest.IDP) { p.Keys = p.Keys[1:] })
	p.Clock.Advance(jwksMaxAge)
	p.DrainFetches()
	if _, err := o.Verify(ctx, authtest.Sign(t, jose.ES256, newKey, "2026-11", p.Claims())); err != nil {
		t.Fatal(err)
	}
	p.AwaitFetch()
	// This waits for the refresh that is running, or finds the new key set already in place.
	if _, err := o.Verify(ctx, authtest.Sign(t, jose.ES256, authtest.ECKey(t), "2026-12", p.Claims())); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
	if _, err := o.Verify(ctx, authtest.Sign(t, jose.ES256, oldKey, "2026-10", p.Claims())); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("token signed with the dropped key: err = %v, want ErrUnauthenticated", err)
	}
}

func TestOIDCProviderDown(t *testing.T) {
	p := authtest.NewIDP(t)
	key := p.NewKey("k")
	o := verifier(p, nil)
	ctx := context.Background()
	token := func() string { return authtest.Sign(t, jose.ES256, key, "k", p.Claims()) }

	p.Set(func(p *authtest.IDP) { p.Status = http.StatusBadGateway })
	for range 5 {
		if _, err := o.Verify(ctx, token()); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
	}
	if err := o.Ping(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Ping = %v, want ErrUnavailable", err)
	}
	if got := p.FetchCount(); got != 1 {
		t.Fatalf("fetches while down = %d, want 1", got)
	}

	p.Set(func(p *authtest.IDP) { p.Status = 0 })
	p.Clock.Advance(jwksRefreshInterval)
	if _, err := o.Verify(ctx, token()); err != nil {
		t.Fatalf("after the provider came back: %v", err)
	}
	if err := o.Ping(ctx); err != nil {
		t.Fatalf("Ping = %v", err)
	}

	// With keys in hand, a provider that goes away again does not stop callers.
	p.Set(func(p *authtest.IDP) { p.Status = http.StatusBadGateway })
	p.Clock.Advance(jwksMaxAge)
	p.DrainFetches()
	if _, err := o.Verify(ctx, token()); err != nil {
		t.Fatalf("with old keys and the provider down: %v", err)
	}
	p.AwaitFetch()
	if _, err := o.Verify(ctx, token()); err != nil {
		t.Fatalf("after the failed refresh: %v", err)
	}
	if err := o.Ping(ctx); err != nil {
		t.Fatalf("Ping with old keys = %v", err)
	}
}

func TestOIDCConcurrentCallersShareOneFetch(t *testing.T) {
	p := authtest.NewIDP(t)
	key := p.NewKey("k")
	gate := make(chan struct{})
	p.Set(func(p *authtest.IDP) { p.Gate = gate })
	o := verifier(p, nil)
	token := authtest.Sign(t, jose.ES256, key, "k", p.Claims())

	const callers = 20
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			_, err := o.Verify(context.Background(), token)
			errs <- err
		})
	}
	p.AwaitFetch()
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := p.FetchCount(); got != 1 {
		t.Fatalf("fetches = %d, want 1", got)
	}
}

// A token without a kid, signed with a key the cached set does not have yet, reads the set
// again within the refresh interval rather than failing until the cache is an hour old.
func TestOIDCRotatedKeyWithoutKid(t *testing.T) {
	p := authtest.NewIDP(t)
	oldKey := p.NewKey("old")
	o := verifier(p, nil)
	ctx := context.Background()
	if _, err := o.Verify(ctx, authtest.Sign(t, jose.ES256, oldKey, "", p.Claims())); err != nil {
		t.Fatal(err)
	}
	newKey := p.NewKey("new")
	p.Clock.Advance(jwksRefreshInterval)
	if _, err := o.Verify(ctx, authtest.Sign(t, jose.ES256, newKey, "", p.Claims())); err != nil {
		t.Fatalf("a token without kid signed with the new key: %v", err)
	}
	// A bad signature without kid reads the set again at most once per interval.
	stranger := authtest.ECKey(t)
	for range 5 {
		if _, err := o.Verify(ctx, authtest.Sign(t, jose.ES256, stranger, "", p.Claims())); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v", err)
		}
	}
	if got := p.FetchCount(); got != 2 {
		t.Fatalf("fetches = %d, want 2", got)
	}
}

func TestOIDCTimeClaims(t *testing.T) {
	p := authtest.NewIDP(t)
	key := p.NewKey("k")
	o := verifier(p, nil)
	now := p.Clock.Now()
	tests := []struct {
		name string
		edit func(map[string]any)
		ok   bool
	}{
		{"issued now", func(map[string]any) {}, true},
		{"issued within the skew ahead", func(c map[string]any) { c["iat"] = now.Add(30 * time.Second).Unix() }, true},
		{"issued a day ahead", func(c map[string]any) { c["iat"] = now.Add(24 * time.Hour).Unix() }, false},
		{"iat that is not a number", func(c map[string]any) { c["iat"] = "yesterday" }, false},
		{"nbf that is not a number", func(c map[string]any) { c["nbf"] = "9999999999" }, false},
		{"nbf in the past", func(c map[string]any) { c["nbf"] = now.Add(-time.Hour).Unix() }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := p.Claims()
			tt.edit(c)
			_, err := o.Verify(context.Background(), authtest.Sign(t, jose.ES256, key, "k", c))
			if tt.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tt.ok && !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestOIDCCallerGivesUpDuringFetch(t *testing.T) {
	p := authtest.NewIDP(t)
	key := p.NewKey("k")
	gate := make(chan struct{})
	p.Set(func(p *authtest.IDP) { p.Gate = gate })
	o := verifier(p, nil)
	token := authtest.Sign(t, jose.ES256, key, "k", p.Claims())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := o.Verify(ctx, token)
		done <- err
	}()
	p.AwaitFetch()
	cancel()
	err := <-done
	// The fetch goes on for the next caller. The gate is opened before any check can fail,
	// so the provider's server can close.
	close(gate)
	// Unavailable, so the caller is answered 503 like any wait on the provider, not 500.
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable wrapping context.Canceled", err)
	}
	if _, err := o.Verify(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if got := p.FetchCount(); got != 1 {
		t.Fatalf("fetches = %d, want 1", got)
	}
}

func TestOIDCDiscovery(t *testing.T) {
	tests := map[string]struct {
		idp           func(*authtest.IDP)
		cfg           func(*authtest.IDP, *OIDCConfig)
		wantErr       bool
		wantDiscovers int
	}{
		"discovered":           {wantDiscovers: 1},
		"issuer with a slash":  {cfg: func(_ *authtest.IDP, c *OIDCConfig) { c.Issuer += "/" }, idp: func(p *authtest.IDP) { p.DocIssuer = p.Issuer() + "/" }, wantDiscovers: 1},
		"jwks url configured":  {cfg: func(p *authtest.IDP, c *OIDCConfig) { c.JWKSURL = p.Issuer() + "/keys" }},
		"document for another": {idp: func(p *authtest.IDP) { p.DocIssuer = "https://elsewhere.example.com" }, wantErr: true, wantDiscovers: 1},
		"jwks uri not http":    {idp: func(p *authtest.IDP) { p.JWKSURI = "file:///etc/passwd" }, wantErr: true, wantDiscovers: 1},
		"jwks uri relative":    {idp: func(p *authtest.IDP) { p.JWKSURI = "/keys" }, wantErr: true, wantDiscovers: 1},
		"no discovery there":   {cfg: func(_ *authtest.IDP, c *OIDCConfig) { c.Issuer += "/realms/none" }, wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := authtest.NewIDP(t)
			key := p.NewKey("k")
			if tt.idp != nil {
				p.Set(tt.idp)
			}
			o := verifier(p, func(c *OIDCConfig) {
				if tt.cfg != nil {
					tt.cfg(p, c)
				}
			})
			c := p.Claims()
			c["iss"] = o.cfg.Issuer
			_, err := o.Verify(context.Background(), authtest.Sign(t, jose.ES256, key, "k", c))
			if tt.wantErr != errors.Is(err, ErrUnavailable) || (!tt.wantErr && err != nil) {
				t.Fatalf("err = %v", err)
			}
			if got := p.DiscoverCount(); got != tt.wantDiscovers {
				t.Fatalf("discoveries = %d, want %d", got, tt.wantDiscovers)
			}
		})
	}
}

func TestOIDCKeySetContents(t *testing.T) {
	p := authtest.NewIDP(t)
	key := p.NewKey("good")
	good := p.Keys[0]
	goodJSON, err := good.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	enc := good
	enc.KeyID, enc.Use = "enc", "enc"
	encJSON, err := enc.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	private, err := (&jose.JSONWebKey{Key: authtest.ECKey(t), KeyID: "private"}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	unknown := `{"kty":"LWE","kid":"future","x":"AAAA"}`
	list := func(keys ...string) string { return `{"keys":[` + strings.Join(keys, ",") + `]}` }
	tests := map[string]struct {
		jwks    string
		kid     string
		wantErr error
	}{
		"other keys are skipped":        {list(unknown, string(encJSON), string(private), string(goodJSON)), "good", nil},
		"only unusable keys":            {list(unknown, string(encJSON)), "good", ErrUnavailable},
		"token names an encryption key": {list(string(encJSON), string(goodJSON)), "enc", ErrUnauthenticated},
		"token names a private key":     {list(string(private), string(goodJSON)), "private", ErrUnauthenticated},
		"empty":                         {list(), "good", ErrUnavailable},
		"not json":                      {`<html>`, "good", ErrUnavailable},
		"too large":                     {`{"keys":[],"pad":"` + strings.Repeat("x", jwksMaxBytes) + `"}`, "good", ErrUnavailable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p.Set(func(p *authtest.IDP) { p.RawJWKS = []byte(tt.jwks) })
			_, err := verifier(p, nil).Verify(context.Background(), authtest.Sign(t, jose.ES256, key, tt.kid, p.Claims()))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestOIDCProviderUnreachable(t *testing.T) {
	p := authtest.NewIDP(t)
	key := p.NewKey("k")
	o := verifier(p, nil)
	token := authtest.Sign(t, jose.ES256, key, "k", p.Claims())
	p.Close()
	if _, err := o.Verify(context.Background(), token); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestNewOIDC(t *testing.T) {
	ok := OIDCConfig{Issuer: "https://idp.example.com", Audience: "transponder"}
	tests := map[string]struct {
		change  func(*OIDCConfig)
		wantErr string
	}{
		"minimal":              {func(*OIDCConfig) {}, ""},
		"http issuer":          {func(c *OIDCConfig) { c.Issuer = "http://localhost:8081/realms/dev" }, ""},
		"no issuer":            {func(c *OIDCConfig) { c.Issuer = "" }, "issuer"},
		"relative issuer":      {func(c *OIDCConfig) { c.Issuer = "idp.example.com" }, "issuer"},
		"unparsable issuer":    {func(c *OIDCConfig) { c.Issuer = "https://idp.example.com/%zz" }, "issuer"},
		"no audience":          {func(c *OIDCConfig) { c.Audience = "" }, "audience"},
		"bad jwks url":         {func(c *OIDCConfig) { c.JWKSURL = "ftp://idp.example.com/keys" }, "jwks url"},
		"unknown mapped role":  {func(c *OIDCConfig) { c.RolesClaim, c.RoleMapping = "roles", map[string]Role{"x": "owner"} }, "unknown role"},
		"mapping with no name": {func(c *OIDCConfig) { c.RoleMapping = map[string]Role{"x": RoleAdmin} }, "roles claim"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := ok
			tt.change(&cfg)
			o, err := NewOIDC(cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if o.cfg.UserClaim != "sub" || o.cfg.ClockSkew != DefaultClockSkew {
					t.Fatalf("defaults = %+v", o.cfg)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestOIDCMapsAdmin(t *testing.T) {
	p := authtest.NewIDP(t)
	if !verifier(p, nil).MapsAdmin() {
		t.Fatal("a mapping to admin is not seen")
	}
	viewerOnly := verifier(p, func(c *OIDCConfig) { c.RoleMapping = map[string]Role{"x": RoleViewer} })
	if viewerOnly.MapsAdmin() {
		t.Fatal("a mapping to viewer counts as admin")
	}
}

func TestAuthenticateWithOIDC(t *testing.T) {
	p := authtest.NewIDP(t)
	key := p.NewKey("k")
	keys := &fakeKeys{}
	apiKey := setupKey(t, keys, "ci", nil)
	jwt := authtest.Sign(t, jose.ES256, key, "k", p.Claims())
	expired := p.Claims()
	expired["exp"] = p.Clock.Now().Add(-time.Hour).Unix()
	withOIDC := NewAuthenticator(keys, p.Clock.Now).WithOIDC(verifier(p, nil))
	keysOnly := NewAuthenticator(keys, p.Clock.Now)

	tests := map[string]struct {
		auth     *Authenticator
		header   [2]string
		wantErr  bool
		wantUser string
		wantKey  string
		wantRole Role
	}{
		"token as bearer":           {withOIDC, [2]string{"Authorization", "Bearer " + jwt}, false, "alice@example.com", "", RoleViewer},
		"token as x-api-key":        {withOIDC, [2]string{"x-api-key", jwt}, false, "alice@example.com", "", RoleViewer},
		"api key next to oidc":      {withOIDC, [2]string{"Authorization", "Bearer " + apiKey}, false, "ci", "ci", RoleMember},
		"api key as x-api-key":      {withOIDC, [2]string{"x-api-key", apiKey}, false, "ci", "ci", RoleMember},
		"expired token":             {withOIDC, [2]string{"Authorization", "Bearer " + authtest.Sign(t, jose.ES256, key, "k", expired)}, true, "", "", ""},
		"unknown api key":           {withOIDC, [2]string{"Authorization", "Bearer tp_nope"}, true, "", "", ""},
		"token with oidc not set":   {keysOnly, [2]string{"Authorization", "Bearer " + jwt}, true, "", "", ""},
		"api key with oidc not set": {keysOnly, [2]string{"Authorization", "Bearer " + apiKey}, false, "ci", "ci", RoleMember},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil)
			r.Header.Set(tt.header[0], tt.header[1])
			id, err := tt.auth.Authenticate(r)
			if tt.wantErr {
				if !errors.Is(err, ErrUnauthenticated) {
					t.Fatalf("err = %v, want ErrUnauthenticated", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if id.User != tt.wantUser || id.Key != tt.wantKey || id.Role != tt.wantRole {
				t.Fatalf("identity = %+v", id)
			}
		})
	}
}

func TestLooksLikeJWT(t *testing.T) {
	tests := map[string]bool{
		"aaa.bbb.ccc":    true,
		"tp_abc":         false,
		"tp_a.b.c":       false,
		"sk-ant-api-key": false,
		"a.b":            false,
		"a.b.c.d":        false,
		"":               false,
	}
	for token, want := range tests {
		if got := looksLikeJWT(token); got != want {
			t.Errorf("looksLikeJWT(%q) = %v, want %v", token, got, want)
		}
	}
}
