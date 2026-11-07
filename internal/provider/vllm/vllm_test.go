package vllm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/raven-clown/transponder/internal/provider"
)

func TestDefaults(t *testing.T) {
	a := New("vllm-local", "", "", nil)
	if a.BaseURL() != "http://localhost:8000/v1" || a.Name() != "vllm-local" {
		t.Fatalf("adapter %q %q", a.Name(), a.BaseURL())
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
			resp, err := New("vllm-local", srv.URL+"/v1", tt.key, srv.Client()).Native(context.Background(),
				&provider.NativeCall{Path: "/embeddings", Body: []byte(`{}`), Header: http.Header{}})
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if path != "/v1/embeddings" || !slices.Equal(auth, tt.header) {
				t.Fatalf("path %q, authorization %v", path, auth)
			}
		})
	}
}
