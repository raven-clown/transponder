package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/router"
)

// guardedRoutes has a route for the platform team, one for the key alice, and an open one.
func guardedRoutes(t *testing.T) []router.Route {
	t.Helper()
	access := func(allow ...string) router.Access {
		a, err := router.ParseAccess(allow)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	target := []router.Target{{Provider: echo.New("echo"), Type: "echo"}}
	return []router.Route{
		{Match: "fast", Access: access("team:platform"), Targets: target},
		{Match: "claude-*", Access: access("team:platform"), Targets: target},
		{Match: "mine", Access: access("key:alice", "role:admin"), Targets: target},
		{Match: "*", Targets: target},
	}
}

type wireError struct {
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func TestRouteAccess(t *testing.T) {
	e, p := setupOIDCRoutes(t, guardedRoutes(t))
	inPlatform := p.Token(p.Claims())
	elsewhere := p.Claims()
	elsewhere["groups"] = []string{"sales"}
	outside := p.Token(elsewhere)

	tests := map[string]struct {
		path, model, credential string
		wantStatus              int
		wantType, wantRoute     string
	}{
		"team member on the team route": {"/v1/chat/completions", "fast", inPlatform, http.StatusOK, "", ""},
		"outsider on the team route":    {"/v1/chat/completions", "fast", outside, http.StatusForbidden, "permission_error", `"fast"`},
		"key on the team route":         {"/v1/chat/completions", "fast", e.key, http.StatusForbidden, "permission_error", `"fast"`},
		"key on its own route":          {"/v1/chat/completions", "mine", e.key, http.StatusOK, "", ""},
		"token user on a key route":     {"/v1/chat/completions", "mine", inPlatform, http.StatusForbidden, "permission_error", `"mine"`},
		"outsider on an open route":     {"/v1/chat/completions", "anything", outside, http.StatusOK, "", ""},
		"anthropic shape, glob route":   {"/v1/messages", "claude-test", outside, http.StatusForbidden, "permission_error", `"claude-*"`},
		"anthropic, allowed":            {"/v1/messages", "claude-test", inPlatform, http.StatusOK, "", ""},
		"embeddings, refused":           {"/v1/embeddings", "fast", outside, http.StatusForbidden, "permission_error", `"fast"`},
		"count tokens, refused":         {"/v1/messages/count_tokens", "claude-test", e.key, http.StatusForbidden, "permission_error", `"claude-*"`},
		"no credentials is still a 401": {"/v1/chat/completions", "fast", "", http.StatusUnauthorized, "", ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var body string
			switch tt.path {
			case "/v1/embeddings":
				body = `{"model":"` + tt.model + `","input":"x"}`
			case "/v1/chat/completions":
				body = `{"model":"` + tt.model + `","messages":[{"role":"user","content":"ping"}]}`
			default:
				body = `{"model":"` + tt.model + `","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`
			}
			headers := map[string]string{}
			if tt.credential != "" {
				headers["Authorization"] = "Bearer " + tt.credential
			}
			before := len(e.usage(t))
			res, err := e.doPath(http.MethodPost, tt.path, headers, body)
			if err != nil {
				t.Fatal(err)
			}
			if res.status != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body %s", res.status, tt.wantStatus, res.body)
			}
			if tt.wantStatus != http.StatusForbidden {
				return
			}
			var we wireError
			if err := json.Unmarshal(res.body, &we); err != nil {
				t.Fatalf("decoding %q: %v", res.body, err)
			}
			if we.Error.Type != tt.wantType || we.Error.Code != "route_forbidden" || !strings.Contains(we.Error.Message, tt.wantRoute) {
				t.Fatalf("error = %+v", we.Error)
			}
			if res.header.Get("X-Usage-Cost-USD") != "0.000000" {
				t.Fatalf("headers %v", res.header)
			}
			if after := len(e.usage(t)); after != before {
				t.Fatalf("a refused call was recorded: %d records, had %d", after, before)
			}
		})
	}
}

func TestModelsListOnlyAllowedRoutes(t *testing.T) {
	e, p := setupOIDCRoutes(t, guardedRoutes(t))
	tests := map[string]struct {
		credential string
		want       []string
	}{
		"platform member": {p.Token(p.Claims()), []string{"fast"}},
		"key alice":       {e.key, []string{"mine"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			res, err := e.doPath(http.MethodGet, "/v1/models", map[string]string{"Authorization": "Bearer " + tt.credential}, "")
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(res.body, &body); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, m := range body.Data {
				got = append(got, m.ID)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("models = %v, want %v", got, tt.want)
			}
		})
	}
}
