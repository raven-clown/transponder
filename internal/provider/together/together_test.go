package together

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/raven-clown/transponder/internal/provider"
)

func TestDefaults(t *testing.T) {
	a := New("together", "", "", nil)
	if a.BaseURL() != "https://api.together.xyz/v1" || a.Name() != "together" {
		t.Fatalf("adapter %q %q", a.Name(), a.BaseURL())
	}
}

func TestForwards(t *testing.T) {
	var path string
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Values("Authorization")
	}))
	defer srv.Close()
	resp, err := New("together", srv.URL+"/v1", "secret", srv.Client()).Native(context.Background(),
		&provider.NativeCall{Path: "/embeddings", Body: []byte(`{}`), Header: http.Header{}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if path != "/v1/embeddings" || !slices.Equal(auth, []string{"Bearer secret"}) {
		t.Fatalf("path %q, authorization %v", path, auth)
	}
}
