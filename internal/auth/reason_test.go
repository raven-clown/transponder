package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/raven-clown/transponder/internal/auth/authtest"
	"github.com/raven-clown/transponder/internal/store"
)

func TestRefusalReasons(t *testing.T) {
	p := authtest.NewIDP(t)
	key := p.NewKey("k")
	keys := &fakeKeys{}
	disabled := setupKey(t, keys, "off", func(k *store.APIKey) { k.Disabled = true })
	expired := setupKey(t, keys, "old", func(k *store.APIKey) { k.ExpiresAt = testNow.Add(-time.Minute) })
	a := NewAuthenticator(keys, p.Clock.Now).WithOIDC(verifier(p, nil))
	claim := func(change func(map[string]any)) string {
		c := p.Claims()
		change(c)
		return authtest.Sign(t, jose.ES256, key, "k", c)
	}
	tests := map[string]struct {
		header [2]string
		want   Reason
	}{
		"no credentials":       {[2]string{}, ReasonMissing},
		"unknown api key":      {[2]string{"Authorization", "Bearer tp_nope"}, ReasonUnknownKey},
		"disabled api key":     {[2]string{"x-api-key", disabled}, ReasonDisabledKey},
		"expired api key":      {[2]string{"x-api-key", expired}, ReasonExpiredKey},
		"token with bad sig":   {[2]string{"Authorization", "Bearer " + authtest.Sign(t, jose.ES256, authtest.ECKey(t), "k", p.Claims())}, ReasonBadToken},
		"token from other iss": {[2]string{"Authorization", "Bearer " + claim(func(c map[string]any) { c["iss"] = "https://elsewhere.example.com" })}, ReasonWrongIssuer},
		"token for other aud":  {[2]string{"Authorization", "Bearer " + claim(func(c map[string]any) { c["aud"] = "someone-else" })}, ReasonWrongAudience},
		"expired token":        {[2]string{"Authorization", "Bearer " + claim(func(c map[string]any) { c["exp"] = p.Clock.Now().Add(-time.Hour).Unix() })}, ReasonExpiredToken},
		"token without user":   {[2]string{"Authorization", "Bearer " + claim(func(c map[string]any) { delete(c, "sub") })}, ReasonBadToken},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil)
			if tt.header[0] != "" {
				r.Header.Set(tt.header[0], tt.header[1])
			}
			_, err := a.Authenticate(r)
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
			if got, ok := ReasonOf(err); !ok || got != tt.want {
				t.Fatalf("reason = %q (%v), want %q", got, ok, tt.want)
			}
		})
	}
}

func TestReasonsAreFixed(t *testing.T) {
	seen := map[Reason]bool{}
	for _, r := range Reasons {
		if seen[r] {
			t.Fatalf("reason %q listed twice", r)
		}
		seen[r] = true
	}
	if len(seen) != 8 {
		t.Fatalf("%d reasons, want the eight the metric names", len(seen))
	}
}
