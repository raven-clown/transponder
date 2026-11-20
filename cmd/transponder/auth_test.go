package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth/authtest"
	"github.com/raven-clown/transponder/internal/config"
)

func TestBuildAuth(t *testing.T) {
	tests := map[string]struct {
		oidc      config.OIDC
		wantOIDC  bool
		wantAdmin bool
		wantErr   string
	}{
		"keys only": {},
		"oidc": {
			oidc:     config.OIDC{Issuer: "https://idp.example.com", Audience: "transponder", UserClaim: "sub", TeamsClaim: "groups"},
			wantOIDC: true,
		},
		"oidc with an admin mapping": {
			oidc:     config.OIDC{Issuer: "https://idp.example.com", Audience: "transponder", RolesClaim: "roles", RoleMapping: map[string]string{"ops": "admin"}},
			wantOIDC: true, wantAdmin: true,
		},
		"unknown role": {
			oidc:    config.OIDC{Issuer: "https://idp.example.com", Audience: "transponder", RolesClaim: "roles", RoleMapping: map[string]string{"ops": "owner"}},
			wantErr: "role_mapping",
		},
		"no audience": {
			oidc:    config.OIDC{Issuer: "https://idp.example.com"},
			wantErr: "audience",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			authn, oidc, err := buildAuth(&config.Bootstrap{Auth: config.Auth{OIDC: tt.oidc}}, nil, http.DefaultClient)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if authn == nil || (oidc != nil) != tt.wantOIDC || (oidc != nil && oidc.MapsAdmin() != tt.wantAdmin) {
				t.Fatalf("authenticator %v, oidc %v", authn, oidc)
			}
		})
	}
}

// lockedBuffer is a buffer serve can write to while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func get(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestServeWithOIDC(t *testing.T) {
	tests := map[string]struct {
		mapping      map[string]string
		wantAdminKey bool
	}{
		// Nobody can sign in as an admin, so the first start still prints a key.
		"no admin mapping": {map[string]string{"tp-auditors": "viewer"}, true},
		"admin mapping":    {map[string]string{"tp-auditors": "admin"}, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := authtest.NewIDP(t)
			ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Bootstrap{
				ListenAddr: ln.Addr().String(),
				Store:      config.Store{SQLite: filepath.Join(t.TempDir(), "t.db")},
				LogLevel:   "info",
				Auth: config.Auth{OIDC: config.OIDC{
					Issuer: p.Issuer(), Audience: authtest.Audience, UserClaim: "sub", TeamsClaim: "groups",
					RolesClaim: "roles", RoleMapping: tt.mapping, ClockSkew: time.Minute,
				}},
			}
			ctx, cancel := context.WithCancel(context.Background())
			var stderr lockedBuffer
			done := make(chan error, 1)
			go func() { done <- serve(ctx, cfg, ln, io.Discard, &stderr) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Errorf("serve returned %v", err)
				}
			}()
			base := "http://" + ln.Addr().String()

			// The listener is open before serve starts, so the first request waits for it.
			token := p.Token(p.Claims())
			if status, body := get(t, base+"/v1/models", token); status != http.StatusOK {
				t.Fatalf("models with a token: status %d, body %s", status, body)
			}
			if status, body := get(t, base+"/readyz", ""); status != http.StatusOK {
				t.Fatalf("readyz: status %d, body %s", status, body)
			}
			if status, _ := get(t, base+"/v1/models", "tp_not-a-key"); status != http.StatusUnauthorized {
				t.Fatalf("models with a bad key: status %d", status)
			}
			if got := strings.Contains(stderr.String(), "tp_"); got != tt.wantAdminKey {
				t.Fatalf("admin key printed = %v, want %v (stderr %q)", got, tt.wantAdminKey, stderr.String())
			}
		})
	}
}

func TestReadyzWithIdentityProviderDown(t *testing.T) {
	p := authtest.NewIDP(t)
	p.Set(func(p *authtest.IDP) { p.Status = http.StatusBadGateway })
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Bootstrap{
		ListenAddr: ln.Addr().String(),
		Store:      config.Store{SQLite: filepath.Join(t.TempDir(), "t.db")},
		LogLevel:   "info",
		Auth:       config.Auth{OIDC: config.OIDC{Issuer: p.Issuer(), Audience: authtest.Audience}},
	}
	key, stop := startServe(t, cfg, ln)
	defer stop()
	base := "http://" + ln.Addr().String()
	status, body := get(t, base+"/readyz", "")
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "oidc") {
		t.Fatalf("readyz: status %d, body %s", status, body)
	}
	// API keys work without the identity provider.
	if status, body := get(t, base+"/v1/models", key); status != http.StatusOK {
		t.Fatalf("models with an API key: status %d, body %s", status, body)
	}
}

func TestBuildRouterAccess(t *testing.T) {
	cfg := &config.Bootstrap{
		Providers: []config.Provider{{Name: "e", Type: "echo"}},
		Routes: []config.Route{
			{Match: "fast", Allow: []string{"team:platform"}, Targets: []config.Target{{Provider: "e"}}},
			{Match: "*", Targets: []config.Target{{Provider: "e"}}},
		},
	}
	r, err := buildRouter(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.Plan("fast")
	if err != nil {
		t.Fatal(err)
	}
	if p.Access.Open() || strings.Join(p.Access.Entries(), ",") != "team:platform" {
		t.Fatalf("access = %v", p.Access.Entries())
	}
	cfg.Routes[0].Allow = []string{"role:owner"}
	if _, err := buildRouter(cfg, nil); err == nil || !strings.Contains(err.Error(), `route "fast"`) {
		t.Fatalf("err = %v", err)
	}
}
