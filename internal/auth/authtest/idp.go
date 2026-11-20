// Package authtest is a local OpenID Connect provider for tests: a discovery document, a key
// set a test can change, and tokens signed in the test. It uses no outside network.
package authtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// Audience is the audience of the tokens Claims describes.
const Audience = "transponder"

// Clock is a time source a test moves by hand.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// Now returns the time the clock shows.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// IDP is a local identity provider. Change the exported fields through Set.
type IDP struct {
	t   testing.TB
	srv *httptest.Server
	// Clock starts at the real time, so tokens also pass a verifier on the real clock.
	Clock *Clock

	mu        sync.Mutex
	Keys      []jose.JSONWebKey
	RawJWKS   []byte        // served instead of Keys when set
	Status    int           // served instead of 200 when set
	Gate      chan struct{} // a key fetch waits for it when set
	JWKSURI   string        // advertised instead of the real one when set
	DocIssuer string        // advertised instead of the real one when set
	signer    *ecdsa.PrivateKey
	signerKid string
	fetches   int
	discovers int
	fetched   chan struct{} // gets a value for every key fetch
}

// NewIDP starts a provider with no keys. It stops when the test ends.
func NewIDP(t testing.TB) *IDP {
	t.Helper()
	p := &IDP{t: t, Clock: &Clock{t: time.Now()}, fetched: make(chan struct{}, 1024)}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		p.discovers++
		doc := map[string]string{"issuer": p.srv.URL, "jwks_uri": p.srv.URL + "/keys"}
		if p.DocIssuer != "" {
			doc["issuer"] = p.DocIssuer
		}
		if p.JWKSURI != "" {
			doc["jwks_uri"] = p.JWKSURI
		}
		p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		p.fetches++
		gate, status, raw := p.Gate, p.Status, p.RawJWKS
		if raw == nil {
			raw, _ = json.Marshal(jose.JSONWebKeySet{Keys: p.Keys})
		}
		p.mu.Unlock()
		select {
		case p.fetched <- struct{}{}:
		default:
		}
		if gate != nil {
			<-gate
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write(raw)
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// Issuer is the issuer URL, as the iss claim and the config carry it.
func (p *IDP) Issuer() string { return p.srv.URL }

// Client is an HTTP client that reaches the provider.
func (p *IDP) Client() *http.Client { return p.srv.Client() }

// Close stops the provider before the test ends.
func (p *IDP) Close() { p.srv.Close() }

// Set changes the provider under its lock.
func (p *IDP) Set(change func(*IDP)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	change(p)
}

// FetchCount is how many times the key set was fetched.
func (p *IDP) FetchCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fetches
}

// DiscoverCount is how many times the discovery document was fetched.
func (p *IDP) DiscoverCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.discovers
}

// AwaitFetch waits for the next key fetch to reach the provider.
func (p *IDP) AwaitFetch() {
	p.t.Helper()
	select {
	case <-p.fetched:
	case <-time.After(10 * time.Second):
		p.t.Fatal("no key fetch arrived")
	}
}

// DrainFetches forgets the fetches seen so far.
func (p *IDP) DrainFetches() {
	for {
		select {
		case <-p.fetched:
		default:
			return
		}
	}
}

// NewKey makes a P-256 key and publishes its public half under kid. Token signs with the
// first key made this way.
func (p *IDP) NewKey(kid string) *ecdsa.PrivateKey {
	p.t.Helper()
	priv := ECKey(p.t)
	p.Set(func(p *IDP) {
		p.Keys = append(p.Keys, jose.JSONWebKey{Key: &priv.PublicKey, KeyID: kid, Use: "sig"})
		if p.signer == nil {
			p.signer, p.signerKid = priv, kid
		}
	})
	return priv
}

// Publish adds a public key to the key set. A non-empty alg limits the key to that signature.
func (p *IDP) Publish(kid string, pub any, alg string) {
	p.Set(func(p *IDP) {
		p.Keys = append(p.Keys, jose.JSONWebKey{Key: pub, KeyID: kid, Algorithm: alg, Use: "sig"})
	})
}

// Claims returns the claims of a token that passes every check: user alice@example.com,
// groups platform and ml, and the roles value tp-auditors.
func (p *IDP) Claims() map[string]any {
	now := p.Clock.Now()
	return map[string]any{
		"iss":    p.srv.URL,
		"aud":    Audience,
		"sub":    "alice@example.com",
		"iat":    now.Unix(),
		"exp":    now.Add(5 * time.Minute).Unix(),
		"groups": []string{"platform", "ml"},
		"roles":  []string{"tp-auditors"},
	}
}

// Token returns a token over claims, signed with the provider's first key. It makes that key
// when the provider has none.
func (p *IDP) Token(claims map[string]any) string {
	p.t.Helper()
	p.mu.Lock()
	key, kid := p.signer, p.signerKid
	p.mu.Unlock()
	if key == nil {
		kid = "test-key"
		key = p.NewKey(kid)
	}
	return Sign(p.t, jose.ES256, key, kid, claims)
}

// ECKey makes a P-256 key.
func ECKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

// Sign returns a token over claims. An empty kid leaves the key id out of the header.
func Sign(t testing.TB, alg jose.SignatureAlgorithm, key any, kid string, claims map[string]any) string {
	t.Helper()
	signingKey := jose.SigningKey{Algorithm: alg, Key: key}
	if kid != "" {
		signingKey.Key = jose.JSONWebKey{Key: key, KeyID: kid}
	}
	signer, err := jose.NewSigner(signingKey, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	token, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// Handmade returns a token with the given header and a signature nobody made.
func Handmade(t testing.TB, header string, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(header)) + "." + enc.EncodeToString(payload) + "." + enc.EncodeToString([]byte("signature"))
}
