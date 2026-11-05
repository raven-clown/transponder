package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/raven-clown/transponder/internal/provider"
)

func TestDefaults(t *testing.T) {
	a := New("local", "", nil)
	if a.BaseURL() != "http://localhost:11434/v1" || a.Name() != "local" {
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
	resp, err := New("local", srv.URL+"/v1", srv.Client()).Native(context.Background(),
		&provider.NativeCall{Path: "/embeddings", Body: []byte(`{}`), Header: http.Header{}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if path != "/v1/embeddings" || len(auth) != 0 {
		t.Fatalf("path %q, authorization %v", path, auth)
	}
}
