package admin

import (
	"testing"

	"github.com/raven-clown/transponder/internal/auth"
)

func TestStatusOIDC(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		e := newEnv(t)
		body := decodeBody(t, e.do(auth.RoleViewer, "GET", "/status", nil))
		if _, ok := body["oidc"]; ok {
			t.Fatalf("oidc present with OIDC off: %v", body["oidc"])
		}
	})
	t.Run("on before the first fetch", func(t *testing.T) {
		e := newEnv(t)
		o, err := auth.NewOIDC(auth.OIDCConfig{Issuer: "https://idp.example", Audience: "transponder"})
		if err != nil {
			t.Fatal(err)
		}
		e = e.withOIDC(o)
		oidc := decodeBody(t, e.do(auth.RoleViewer, "GET", "/status", nil))["oidc"].(map[string]any)
		if oidc["issuer"] != "https://idp.example" || oidc["keys"] != float64(0) {
			t.Fatalf("oidc %v", oidc)
		}
		if _, ok := oidc["fetched_at"]; ok {
			t.Fatalf("fetched_at before the first fetch: %v", oidc)
		}
		if _, ok := oidc["last_error"]; ok {
			t.Fatalf("last_error without a fetch: %v", oidc)
		}
	})
}
