package server_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/router"
)

func TestListModels(t *testing.T) {
	e := setupRoutes(t, []router.Route{
		{Match: "fast", Targets: []router.Target{{Provider: echo.New("echo"), Type: "echo", Model: "small"}}},
		{Match: "*", Targets: []router.Target{{Provider: echo.New("echo"), Type: "echo"}}},
	})
	res, err := e.doPath(http.MethodGet, "/v1/models", e.auth(), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusOK || res.header.Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, headers %v", res.status, res.header)
	}
	want := `{"object":"list","data":[{"id":"fast","object":"model","created":0,"owned_by":"transponder"},{"id":"small","object":"model","created":0,"owned_by":"transponder"}]}`
	if string(res.body) != want {
		t.Fatalf("body %s", res.body)
	}

	res, err = e.doPath(http.MethodGet, "/v1/models", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusUnauthorized || errorCode(t, res) != "invalid_api_key" {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
}

func TestListModelsEmpty(t *testing.T) {
	e := setup(t, echo.New("echo"))
	res, err := e.doPath(http.MethodGet, "/v1/models", e.auth(), "")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(res.body, &body); err != nil || body.Object != "list" || body.Data == nil || len(body.Data) != 0 {
		t.Fatalf("body %s: %v", res.body, err)
	}
}
