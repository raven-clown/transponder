package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/schema"
)

// whoami records the caller its Chat call was made for.
type whoami struct {
	*echo.Echo
	seen chan auth.Identity
}

func (p whoami) Chat(ctx context.Context, req *schema.ChatRequest) (*schema.ChatResponse, error) {
	id, _ := auth.FromContext(ctx)
	p.seen <- id
	return p.Echo.Chat(ctx, req)
}

func TestCallCarriesIdentity(t *testing.T) {
	p := whoami{Echo: echo.New("echo"), seen: make(chan auth.Identity, 1)}
	e := setup(t, p)
	if res := e.post(t, ping); res.status != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.status, res.body)
	}
	id := <-p.seen
	if id.User != "alice" || id.Key != "alice" || id.Role != auth.RoleMember {
		t.Fatalf("identity = %+v", id)
	}
}
