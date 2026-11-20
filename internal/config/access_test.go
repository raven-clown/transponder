package config

import (
	"reflect"
	"strings"
	"testing"
)

const accessProviders = `
providers:
  - {name: e, type: echo}
routes:
`

func TestLoadRouteAllow(t *testing.T) {
	c, err := Load(writeFile(t, accessProviders+`
  - match: fast
    allow: ["team:platform", "user:alice@example.com", "key:ci", "role:admin"]
    targets: [{provider: e}]
  - match: "*"
    targets: [{provider: e}]
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"team:platform", "user:alice@example.com", "key:ci", "role:admin"}
	if !reflect.DeepEqual(c.Routes[0].Allow, want) || c.Routes[1].Allow != nil {
		t.Fatalf("routes = %+v", c.Routes)
	}
}

func TestLoadRouteAllowRejects(t *testing.T) {
	tests := map[string]struct {
		allow string
		want  string
	}{
		"unknown kind": {`["group:platform"]`, `route "fast": allow entry "group:platform"`},
		"no name":      {`["team:"]`, `allow entry "team:"`},
		"bare name":    {`["alice"]`, `allow entry "alice"`},
		"unknown role": {`["role:owner"]`, `unknown role "owner"`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeFile(t, accessProviders+"  - match: fast\n    allow: "+tt.allow+"\n    targets: [{provider: e}]\n"), nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %s", err, tt.want)
			}
		})
	}
}
