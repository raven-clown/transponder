package server_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/auth/authtest"
	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/server"
)

const messagesPing = `{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`

// setupOIDC starts a gateway that accepts API keys and tokens from a local identity provider.
func setupOIDC(t *testing.T) (*env, *authtest.IDP) {
	t.Helper()
	p := authtest.NewIDP(t)
	p.NewKey("k1")
	o, err := auth.NewOIDC(auth.OIDCConfig{Issuer: p.Issuer(), Audience: authtest.Audience, TeamsClaim: "groups", Client: p.Client()})
	if err != nil {
		t.Fatal(err)
	}
	e := setupDeps(t, echoRoutes(echo.New("echo")), func(d *server.Deps) { d.Auth = d.Auth.WithOIDC(o) })
	return e, p
}

func TestTokenCaller(t *testing.T) {
	tests := map[string]struct {
		path, header, prefix, body string
	}{
		"chat with a bearer token":   {"/v1/chat/completions", "Authorization", "Bearer ", ping},
		"messages with x-api-key":    {"/v1/messages", "x-api-key", "", messagesPing},
		"messages with a bearer":     {"/v1/messages", "Authorization", "Bearer ", messagesPing},
		"embeddings with a bearer":   {"/v1/embeddings", "Authorization", "Bearer ", `{"model":"e","input":"x"}`},
		"chat with x-api-key header": {"/v1/chat/completions", "x-api-key", "", ping},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e, p := setupOIDC(t)
			res, err := e.doPath(http.MethodPost, tt.path, map[string]string{tt.header: tt.prefix + p.Token(p.Claims())}, tt.body)
			if err != nil {
				t.Fatal(err)
			}
			if res.status != http.StatusOK {
				t.Fatalf("status = %d, body %s", res.status, res.body)
			}
			recs := e.usage(t)
			if len(recs) != 1 || recs[0].User != "alice@example.com" || recs[0].Key != "" || !slices.Equal(recs[0].Teams, []string{"platform", "ml"}) {
				t.Fatalf("records = %+v", recs)
			}
		})
	}
}

func TestTokenRefused(t *testing.T) {
	e, p := setupOIDC(t)
	expired := p.Claims()
	expired["exp"] = time.Now().Add(-time.Hour).Unix()
	elsewhere := p.Claims()
	elsewhere["aud"] = "logbook"
	tests := map[string]struct {
		path, body, token, wantType string
	}{
		"expired, openai shape":         {"/v1/chat/completions", ping, p.Token(expired), "invalid_request_error"},
		"wrong audience, openai shape":  {"/v1/chat/completions", ping, p.Token(elsewhere), "invalid_request_error"},
		"expired, anthropic shape":      {"/v1/messages", messagesPing, p.Token(expired), "authentication_error"},
		"signed by a stranger":          {"/v1/chat/completions", ping, authtest.Sign(t, "ES256", authtest.ECKey(t), "k1", p.Claims()), "invalid_request_error"},
		"token for the usage endpoint":  {"/v1/usage", "", p.Token(expired), "invalid_request_error"},
		"token for the models endpoint": {"/v1/models", "", p.Token(expired), "invalid_request_error"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			method := http.MethodPost
			if tt.body == "" {
				method = http.MethodGet
			}
			res, err := e.doPath(method, tt.path, map[string]string{"Authorization": "Bearer " + tt.token}, tt.body)
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(res.body, &body); err != nil {
				t.Fatalf("decoding %q: %v", res.body, err)
			}
			if res.status != http.StatusUnauthorized || body.Error.Type != tt.wantType {
				t.Fatalf("status = %d, body %s", res.status, res.body)
			}
		})
	}
	if n := len(e.usage(t)); n != 0 {
		t.Fatalf("records = %d, want none", n)
	}
}

func TestIdentityProviderDown(t *testing.T) {
	e, p := setupOIDC(t)
	p.Set(func(p *authtest.IDP) { p.Status = http.StatusBadGateway })
	res, err := e.do(http.MethodPost, map[string]string{"Authorization": "Bearer " + p.Token(p.Claims())}, ping)
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusServiceUnavailable || errorCode(t, res) != "auth_unavailable" || res.header.Get("Retry-After") == "" {
		t.Fatalf("status = %d, headers %v, body %s", res.status, res.header, res.body)
	}
	if res.header.Get("X-Usage-Cost-USD") != "0.000000" {
		t.Fatalf("headers %v", res.header)
	}
	// API keys do not depend on the identity provider.
	if res := e.post(t, ping); res.status != http.StatusOK {
		t.Fatalf("api key while the provider is down: status = %d, body %s", res.status, res.body)
	}
}

func TestStoreErrorWhileAuthenticating(t *testing.T) {
	e := setup(t, echo.New("echo"))
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := e.do(http.MethodPost, map[string]string{"Authorization": "Bearer " + e.key}, ping)
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, body %s", res.status, res.body)
	}
}
