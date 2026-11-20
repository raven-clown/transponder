package cloudflare

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/raven-clown/transponder/internal/provider"
)

func TestNativeForwardsToGateway(t *testing.T) {
	var gotPath, gotAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuthorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	adapter := New("cf", upstream.URL+"/v1/account/gateway/compat", "token", upstream.Client())
	resp, err := adapter.Native(context.Background(), &provider.NativeCall{Path: "/chat/completions", Body: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotPath != "/v1/account/gateway/compat/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuthorization != "Bearer token" {
		t.Fatalf("authorization = %q", gotAuthorization)
	}
}
