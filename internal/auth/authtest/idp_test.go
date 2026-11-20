package authtest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

func get(t *testing.T, p *IDP, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, p.Issuer()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func TestIDPServesDiscoveryAndKeys(t *testing.T) {
	p := NewIDP(t)
	key := p.NewKey("k1")
	p.Publish("k2", &ECKey(t).PublicKey, "ES256")

	_, body := get(t, p, "/.well-known/openid-configuration")
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Issuer != p.Issuer() || doc.JWKSURI != p.Issuer()+"/keys" || p.DiscoverCount() != 1 {
		t.Fatalf("discovery = %+v, count %d", doc, p.DiscoverCount())
	}

	_, body = get(t, p, "/keys")
	p.AwaitFetch()
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		t.Fatal(err)
	}
	if len(set.Keys) != 2 || len(set.Key("k1")) != 1 || set.Key("k2")[0].Algorithm != "ES256" || p.FetchCount() != 1 {
		t.Fatalf("keys = %+v, count %d", set.Keys, p.FetchCount())
	}

	// A token from the provider verifies with the key it publishes.
	jws, err := jose.ParseSignedCompact(p.Token(p.Claims()), []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := jws.Verify(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["iss"] != p.Issuer() || claims["aud"] != Audience || claims["sub"] != "alice@example.com" || jws.Signatures[0].Protected.KeyID != "k1" {
		t.Fatalf("claims = %v", claims)
	}
}

func TestIDPOverrides(t *testing.T) {
	p := NewIDP(t)
	p.Set(func(p *IDP) {
		p.DocIssuer, p.JWKSURI = "https://elsewhere.example.com", "https://elsewhere.example.com/keys"
		p.RawJWKS = []byte(`{"keys":[]}`)
	})
	_, body := get(t, p, "/.well-known/openid-configuration")
	if !strings.Contains(string(body), "https://elsewhere.example.com/keys") {
		t.Fatalf("discovery = %s", body)
	}
	if _, body = get(t, p, "/keys"); string(body) != `{"keys":[]}` {
		t.Fatalf("keys = %s", body)
	}
	p.Set(func(p *IDP) { p.Status = http.StatusBadGateway })
	if status, _ := get(t, p, "/keys"); status != http.StatusBadGateway {
		t.Fatalf("status = %d", status)
	}
	p.DrainFetches()

	gate := make(chan struct{})
	p.Set(func(p *IDP) { p.Status, p.Gate = 0, gate })
	done := make(chan int, 1)
	go func() {
		status, _ := get(t, p, "/keys")
		done <- status
	}()
	p.AwaitFetch()
	close(gate)
	if status := <-done; status != http.StatusOK || p.FetchCount() != 3 {
		t.Fatalf("status = %d, fetches %d", status, p.FetchCount())
	}
}

func TestTokenMakesAKey(t *testing.T) {
	p := NewIDP(t)
	token := p.Token(p.Claims())
	if len(p.Keys) != 1 || strings.Count(token, ".") != 2 {
		t.Fatalf("keys = %d, token %q", len(p.Keys), token)
	}
	if again := p.Token(p.Claims()); len(p.Keys) != 1 || again == "" {
		t.Fatalf("a second token made another key: %d", len(p.Keys))
	}
}

func TestSignAndHandmade(t *testing.T) {
	key := ECKey(t)
	jws, err := jose.ParseSignedCompact(Sign(t, jose.ES256, key, "", map[string]any{"sub": "x"}), []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	if jws.Signatures[0].Protected.KeyID != "" {
		t.Fatalf("kid = %q, want none", jws.Signatures[0].Protected.KeyID)
	}
	if _, err := jws.Verify(&key.PublicKey); err != nil {
		t.Fatal(err)
	}
	forged := Handmade(t, `{"alg":"ES256","kid":"k"}`, map[string]any{"sub": "x"})
	jws, err = jose.ParseSignedCompact(forged, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jws.Verify(&key.PublicKey); err == nil {
		t.Fatal("a handmade token verified")
	}
}

func TestClock(t *testing.T) {
	p := NewIDP(t)
	start := p.Clock.Now()
	p.Clock.Advance(time.Hour)
	if got := p.Clock.Now().Sub(start); got != time.Hour {
		t.Fatalf("advanced by %s", got)
	}
	p.Close()
}
