package lmstudio

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/raven-clown/transponder/internal/provider"
)

func TestDefaults(t *testing.T) {
	a := New("lmstudio", "", "k", nil)
	if a.BaseURL() != "http://localhost:1234/v1" || a.Name() != "lmstudio" {
		t.Fatalf("adapter %q %q", a.Name(), a.BaseURL())
	}
	const custom = "http://gateway.example/v1"
	if got := New("lmstudio", custom, "k", nil).BaseURL(); got != custom {
		t.Fatalf("base URL %q", got)
	}
}

func TestForwards(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		header []string
	}{
		{"no key sends no header", "", nil},
		{"key is sent as a bearer token", "secret", []string{"Bearer secret"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			var auth []string
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				path, auth = r.URL.Path, r.Header.Values("Authorization")
			}))
			defer srv.Close()
			resp, err := New("lmstudio", srv.URL+"/v1", tt.key, srv.Client()).Native(context.Background(),
				&provider.NativeCall{Path: "/chat/completions", Body: []byte(`{}`), Header: http.Header{}})
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if path != "/v1/chat/completions" || !slices.Equal(auth, tt.header) {
				t.Fatalf("path %q, authorization %v", path, auth)
			}
		})
	}
}
