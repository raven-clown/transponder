package config

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transponder.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || c.Store.SQLite != "./transponder.db" || c.LogLevel != "info" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.Settings.AttemptTimeout != time.Minute || c.Settings.RequestTimeout != 10*time.Minute {
		t.Fatalf("unexpected timeout defaults: %+v", c.Settings)
	}
}

func TestLoadSettings(t *testing.T) {
	c, err := Load(writeFile(t, "settings:\n  attempt_timeout: 5s\n  request_timeout: 1m\n"), []string{"TRANSPONDER_SETTINGS__ATTEMPT_TIMEOUT=7s"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Settings.AttemptTimeout != 7*time.Second || c.Settings.RequestTimeout != time.Minute {
		t.Fatalf("settings %+v", c.Settings)
	}
	c, err = Load(writeFile(t, "settings:\n  cooldown_failures: 5\n  cooldown: 2m\n"), nil)
	if err != nil || c.Settings.CooldownFailures != 5 || c.Settings.Cooldown != 2*time.Minute {
		t.Fatalf("cooldown settings %+v, %v", c.Settings, err)
	}
}

func TestLoadFile(t *testing.T) {
	p := writeFile(t, `
listen_addr: "127.0.0.1:9090"
store:
  sqlite: /data/t.db
secrets_key: abc
otel:
  endpoint: http://otel:4318
log_level: debug
`)
	c, err := Load(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Bootstrap{
		ListenAddr: "127.0.0.1:9090",
		Store:      Store{SQLite: "/data/t.db"},
		SecretsKey: "abc",
		OTel:       OTel{Endpoint: "http://otel:4318"},
		LogLevel:   "debug",
		Settings:   Settings{AttemptTimeout: time.Minute, RequestTimeout: 10 * time.Minute, CooldownFailures: 3, Cooldown: 30 * time.Second},
	}
	if !reflect.DeepEqual(*c, want) {
		t.Fatalf("got %+v, want %+v", *c, want)
	}
}

func TestLoadExpandsVars(t *testing.T) {
	c, err := Load(writeFile(t, "secrets_key: ${K}\n"), []string{"K=abc"})
	if err != nil {
		t.Fatal(err)
	}
	if c.SecretsKey != "abc" {
		t.Fatalf("SecretsKey = %q", c.SecretsKey)
	}
	_, err = Load(writeFile(t, "secrets_key: ${MISSING}\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("err = %v, want it to name MISSING", err)
	}
}

func TestLoadSkipsVarsInCommentLines(t *testing.T) {
	body := `providers:
  - name: echo
    type: echo
    # api_key: ${UNSET_KEY}
routes:
  - match: "*"
    targets:
      - provider: echo
`
	c, err := Load(writeFile(t, body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Providers) != 1 || c.Providers[0].Name != "echo" || c.Providers[0].APIKey != "" {
		t.Fatalf("providers = %+v", c.Providers)
	}
}

func TestLoadExpandsHashInsideBlockScalar(t *testing.T) {
	c, err := Load(writeFile(t, "secrets_key: |-\n  #${KEY}\n"), []string{"KEY=abc"})
	if err != nil {
		t.Fatal(err)
	}
	if c.SecretsKey != "#abc" {
		t.Fatalf("SecretsKey = %q", c.SecretsKey)
	}
	_, err = Load(writeFile(t, "secrets_key: |-\n  #${NEED}\n# ${SKIP}\n"), nil)
	if err == nil || err.Error() != "config: environment variables not set: NEED" {
		t.Fatalf("err = %v", err)
	}
}

func TestExpandCommentLines(t *testing.T) {
	env := map[string]string{"HOST": "h"}
	tests := []struct {
		name, in, want, missing string
	}{
		{name: "indented comment is copied", in: "  # key: ${UNSET}\nb: ${HOST}", want: "  # key: ${UNSET}\nb: h"},
		{name: "tab before the hash", in: "a: 1\t# ${UNSET}", want: "a: 1"},
		{name: "a hash after a value is not a comment line", in: "url: http://${HOST}/#${HOST}", want: "url: http://h/#h"},
		{name: "a missing variable outside comments still fails", in: "# ${UNSET}\nkey: ${ALSO_UNSET}", missing: "ALSO_UNSET"},
		{name: "a hash line inside a literal block is expanded", in: "key: |\n  # ${HOST}\n  v: ${HOST}", want: "key: |\n  # h\n  v: h"},
		{name: "a hash line inside a folded block is expanded", in: "key: >-\n  # ${HOST}\n", want: "key: >-\n  # h\n"},
		{name: "a comment after the block is copied", in: "key: |\n  ${HOST}\n# ${UNSET}\n", want: "key: |\n  h\n# ${UNSET}\n"},
		{name: "a comment on the block header is copied", in: "key: | # ${UNSET}\n  # ${HOST}\n", want: "key: | # ${UNSET}\n  # h\n"},
		{name: "a comment glued to the indicator is copied", in: "key: |# ${UNSET}\n  #${HOST}\n", want: "key: |# ${UNSET}\n  #h\n"},
		{name: "an unset variable inside a block still fails", in: "# ${SKIP}\nkey: |\n  # ${UNSET}\n", missing: "UNSET"},
		{name: "a pipe inside a plain value is not a block", in: "note: use | to pipe ${HOST}\n# ${UNSET}\n", want: "note: use | to pipe h\n# ${UNSET}\n"},
		{name: "a blank line does not end the block", in: "key: |\n\n  # ${HOST}\n# ${UNSET}", want: "key: |\n\n  # h\n# ${UNSET}"},
		{name: "content is indented further than its header", in: "outer:\n  key: |\n    # ${HOST}\n  # ${UNSET}\n  other: ${HOST}", want: "outer:\n  key: |\n    # h\n  # ${UNSET}\n  other: h"},
		{name: "a sequence block expands a hash line", in: "- |\n  # ${HOST}\n- ${HOST}", want: "- |\n  # h\n- h"},
		{name: "a sibling of a sequence key is outside the block", in: "- name: |\n    # ${HOST}\n  # ${UNSET}\n  age: ${HOST}", want: "- name: |\n    # h\n  # ${UNSET}\n  age: h"},
		{name: "a carriage return is preserved", in: "# ${UNSET}\r\nkey: ${HOST}\r\n", want: "# ${UNSET}\r\nkey: h\r\n"},
		{name: "a key is expanded", in: "${HOST}: 1", want: "h: 1"},
		{name: "two missing variables are named once each, sorted", in: "a: ${B}${A}\nb: ${A}", missing: "A, B"},
		{name: "a merge key keeps the expanded anchor", in: "base: &b {x: '${HOST}'}\nuse:\n  <<: *b\n", want: "base: {x: h}\nuse: {x: h}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := expand([]byte(tt.in), env)
			if tt.missing != "" {
				if err == nil || err.Error() != "config: environment variables not set: "+tt.missing {
					t.Fatalf("err = %v, want only %s named", err, tt.missing)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := doc.Decode(&got); err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal([]byte(tt.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("expand = %#v, want %#v", got, want)
			}
		})
	}
}

// FuzzExpand checks that expansion never panics, and that a file without a $ parses as it would
// without expansion.
func FuzzExpand(f *testing.F) {
	for _, s := range []string{
		"secrets_key: ${K}\n",
		"key: |\n  # ${K}\n# ${U}\n",
		"a: [${K}, $K]\nb: {c: \"${K}\"}\n",
		"x: &a ${K}\ny: *a\nz:\n  <<: {m: 1}\n",
		"- ? ${K}\n  : >-\n    #${K}\n",
		"plain: 1\n",
	} {
		f.Add(s)
	}
	env := map[string]string{"K": "v #: 'x\"\n"}
	f.Fuzz(func(t *testing.T, in string) {
		doc, err := expand([]byte(in), env)
		if strings.Contains(in, "$") || err != nil {
			return
		}
		var got, want any
		gotErr := doc.Decode(&got)
		wantErr := yaml.Unmarshal([]byte(in), &want)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("decode errors differ: %v and %v", gotErr, wantErr)
		}
		if gotErr == nil && !reflect.DeepEqual(got, want) {
			t.Fatalf("expand changed a file without variables: %#v, want %#v", got, want)
		}
	})
}

func TestLoadEnvOverrides(t *testing.T) {
	p := writeFile(t, "listen_addr: \":8081\"\nstore:\n  sqlite: a.db\n")
	c, err := Load(p, []string{"TRANSPONDER_LISTEN_ADDR=:9000", "transponder_store__sqlite=/tmp/x.db", "PATH=/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":9000" || c.Store.SQLite != "/tmp/x.db" {
		t.Fatalf("overrides not applied: %+v", c)
	}
	c, err = Load(writeFile(t, "previous_secrets_key: from-file\n"), []string{"TRANSPONDER_PREVIOUS_SECRETS_KEY=from-env"})
	if err != nil || c.PreviousSecretsKey != "from-env" {
		t.Fatalf("previous_secrets_key %q, %v", c.PreviousSecretsKey, err)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		environ []string
		path    string
		want    string
		hidden  string
	}{
		{name: "unknown file key", body: "listen_adr: x\n"},
		{name: "both stores", body: "store:\n  sqlite: a.db\n  postgres: postgres://x\n"},
		{name: "previous secrets key alone with Postgres", body: "store:\n  postgres: postgres://x\nprevious_secrets_key: abc\n", want: "previous_secrets_key needs secrets_key"},
		{name: "bad log level", body: "log_level: loud\n"},
		{name: "listen address without a port", body: "listen_addr: \"8080\"\n", want: `listen_addr "8080" must be host:port`},
		{name: "ops listen address not host:port", body: "ops_listen_addr: nope\n", want: `ops_listen_addr "nope" must be host:port`},
		{name: "ops listen address uses the same port", body: "listen_addr: \":8080\"\nops_listen_addr: \":8080\"\n", want: "must differ"},
		{name: "ops listen address on every interface", body: "listen_addr: \"0.0.0.0:8080\"\nops_listen_addr: \"127.0.0.1:8080\"\n", want: "must differ"},
		{name: "ops listen address with a padded port", body: "listen_addr: \":8080\"\nops_listen_addr: \":08080\"\n", want: "must differ"},
		{name: "listen address from the environment without a port", environ: []string{"TRANSPONDER_LISTEN_ADDR=8080"}, want: `listen_addr "8080" must be host:port`},
		{name: "listen address without a port number", body: "listen_addr: \":\"\n", want: `listen_addr ":" must be host:port`},
		{name: "listen address with an unknown service name", body: "listen_addr: \":nope\"\n", want: "neither a number nor a known service"},
		{name: "listen address port out of range", body: "listen_addr: \":70000\"\n", want: "between 0 and 65535"},
		{name: "ops listen address with the same IPv6 host written twice", body: "listen_addr: \"[::1]:8080\"\nops_listen_addr: \"[0:0:0:0:0:0:0:1]:8080\"\n", want: "must differ"},
		{name: "ops listen address with the same name in another case", body: "listen_addr: \"LocalHost:8080\"\nops_listen_addr: \"localhost:8080\"\n", want: "must differ"},
		{name: "ops listen address on an IPv6 wildcard", body: "listen_addr: \"0.0.0.0:8080\"\nops_listen_addr: \"[::]:8080\"\n", want: "must differ"},
		{name: "ops listen address on an IPv6 loopback behind an IPv4 wildcard", body: "listen_addr: \"0.0.0.0:8080\"\nops_listen_addr: \"[::1]:8080\"\n", want: "must differ"},
		{name: "ops listen address on an IPv4 loopback behind an IPv6 wildcard", body: "listen_addr: \"[::]:8080\"\nops_listen_addr: \"127.0.0.1:8080\"\n", want: "must differ"},
		{name: "listen address with a zone on an IPv4 address", body: "listen_addr: \"127.0.0.1%lo:8080\"\n", want: "only an IPv6 address takes"},
		{name: "listen address with a second percent sign", body: "listen_addr: \"[fe80::1%eth0%x]:8080\"\n", want: "only an IPv6 address takes"},
		{name: "listen address with an empty zone", body: "listen_addr: \"[fe80::1%]:8080\"\n", want: "only an IPv6 address takes"},
		{name: "ops listen address with the same link-local address", body: "listen_addr: \"[fe80::1%eth0]:8080\"\nops_listen_addr: \"[fe80:0:0:0:0:0:0:1%eth0]:8080\"\n", want: "must differ"},
		{name: "missing file", path: filepath.Join(t.TempDir(), "nope.yaml")},
		{name: "provider without name", body: "providers:\n  - type: echo\n", want: "providers[0]"},
		{name: "duplicate provider", body: "providers:\n  - {name: a, type: echo}\n  - {name: a, type: echo}\n", want: `"a"`},
		{name: "unknown provider type", body: "providers:\n  - {name: b, type: bedrock}\n", want: "bedrock"},
		{name: "route with the same name as an earlier one", body: "providers:\n  - {name: e, type: echo}\nroutes:\n  - {match: gpt-4o, allow: ['team:a'], targets: [{provider: e}]}\n  - {match: gpt-4o, allow: ['team:b'], targets: [{provider: e}]}\n", want: `routes[1] "gpt-4o" can never match: routes[0] has the same match and comes first`},
		{name: "route with the same glob as an earlier one", body: "providers:\n  - {name: e, type: echo}\nroutes:\n  - {match: 'claude-*', targets: [{provider: e}]}\n  - {match: gpt-4o, targets: [{provider: e}]}\n  - {match: 'claude-*', targets: [{provider: e}]}\n", want: `routes[2] "claude-*" can never match: routes[0] has the same match and comes first`},
		{name: "route after a catch-all", body: "providers:\n  - {name: e, type: echo}\nroutes:\n  - {match: '*', targets: [{provider: e}]}\n  - {match: gpt-4o, targets: [{provider: e}]}\n", want: `routes[1] "gpt-4o" can never match: routes[0] matches every model and comes first`},
		{name: "second catch-all", body: "providers:\n  - {name: e, type: echo}\nroutes:\n  - {match: '*', targets: [{provider: e}]}\n  - {match: '*', targets: [{provider: e}]}\n", want: `routes[1] "*" can never match: routes[0] matches every model`},
		{name: "provider base URL without scheme", body: "providers:\n  - {name: no-scheme, type: openai, base_url: api.openai.com/v1}\nroutes:\n  - {match: '*', targets: [{provider: no-scheme}]}\n", want: `provider "no-scheme": base_url must be an http or https URL`},
		{name: "provider base URL with unsupported scheme", body: "providers:\n  - {name: ftp-provider, type: openai, base_url: ftp://example.com}\nroutes:\n  - {match: '*', targets: [{provider: ftp-provider}]}\n", want: `provider "ftp-provider": base_url must be an http or https URL`},
		{name: "provider base URL without host", body: "providers:\n  - {name: no-host, type: openai, base_url: 'https://'}\nroutes:\n  - {match: '*', targets: [{provider: no-host}]}\n", want: `provider "no-host": base_url must be an http or https URL`},
		{name: "provider base URL error hides password", body: "providers:\n  - {name: private-provider, type: openai, base_url: 'https://user:secret@'}\nroutes:\n  - {match: '*', targets: [{provider: private-provider}]}\n", want: `provider "private-provider": base_url must be an http or https URL`, hidden: "secret"},
		{name: "negative cooldown", body: "settings:\n  cooldown: -1s\n", want: "cooldown"},
		{name: "negative cooldown failures", body: "settings:\n  cooldown_failures: -1\n", want: "cooldown_failures"},
		{name: "negative attempt timeout", body: "settings:\n  attempt_timeout: -1s\n", want: "attempt_timeout"},
		{name: "request timeout not a duration", body: "settings:\n  request_timeout: soon\n", want: "`soon`"},
		{name: "unknown type lists the known ones", body: "providers:\n  - {name: b, type: bedrock}\n", want: "known types are anthropic, cerebras, cloudflare, echo, gemini"},
		{name: "cloudflare without base URL", body: "providers:\n  - {name: cf, type: cloudflare}\nroutes:\n  - {match: '*', targets: [{provider: cf}]}\n", want: `provider "cf" of type cloudflare needs base_url`},
		{name: "route without match", body: "providers:\n  - {name: a, type: echo}\nroutes:\n  - targets: [{provider: a}]\n", want: "routes[0]"},
		{name: "providers without routes", body: "providers:\n  - {name: a, type: openai}\n", want: "routes"},
		{name: "route without targets", body: "routes:\n  - match: x\n", want: `"x"`},
		{name: "target unknown provider", body: "providers:\n  - {name: a, type: echo}\nroutes:\n  - match: x\n    targets: [{provider: z}]\n", want: `"z"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.path
			if p == "" {
				p = writeFile(t, tt.body)
			}
			_, err := Load(p, tt.environ)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tt.hidden != "" && strings.Contains(err.Error(), tt.hidden) {
				t.Fatal("error exposed a sensitive value")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not name %q", err, tt.want)
			}
		})
	}
}

func TestLoadAcceptsACatchAllRouteLast(t *testing.T) {
	body := "providers:\n  - {name: e, type: echo}\nroutes:\n  - {match: gpt-4o, targets: [{provider: e}]}\n  - {match: 'claude-*', targets: [{provider: e}]}\n  - {match: '*', targets: [{provider: e}]}\n"
	if _, err := Load(writeFile(t, body), nil); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoadAcceptsListenAddrs(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "different ports on every interface", body: "listen_addr: \":8080\"\nops_listen_addr: \":9000\"\n"},
		{name: "same port on two named hosts", body: "listen_addr: \"127.0.0.1:8080\"\nops_listen_addr: \"127.0.0.2:8080\"\n"},
		{name: "two free ports cannot collide", body: "listen_addr: \":0\"\nops_listen_addr: \":0\"\n"},
		{name: "one link-local address on two interfaces", body: "listen_addr: \"[fe80::1%eth0]:8080\"\nops_listen_addr: \"[fe80::1%eth1]:8080\"\n"},
		{name: "two zones that differ only in case", body: "listen_addr: \"[fe80::1%eth0]:8080\"\nops_listen_addr: \"[fe80::1%ETH0]:8080\"\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Load(writeFile(t, tt.body), nil); err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
	}
}

func TestLoadRefusesAServicePortOnTheNumberItNames(t *testing.T) {
	if _, err := net.DefaultResolver.LookupPort(t.Context(), "tcp", "http"); err != nil {
		t.Skip("this host has no service list to look the name up in")
	}
	_, err := Load(writeFile(t, "listen_addr: \":http\"\nops_listen_addr: \":80\"\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("error %v does not report the shared port", err)
	}
}

func TestLoadVercel(t *testing.T) {
	p := writeFile(t, `providers:
  - name: vercel
    type: vercel
    api_key: ${VERCEL_API_KEY}
  - name: custom
    type: vercel
    api_key: ${VERCEL_API_KEY}
    base_url: https://gateway.example/v1
routes:
  - match: "anthropic/*"
    targets:
      - provider: vercel
  - match: "custom/*"
    targets:
      - provider: custom
`)
	c, err := Load(p, []string{"VERCEL_API_KEY=k"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Provider{
		{Name: "vercel", Type: "vercel", APIKey: "k"},
		{Name: "custom", Type: "vercel", BaseURL: "https://gateway.example/v1", APIKey: "k"},
	}
	if !reflect.DeepEqual(c.Providers, want) {
		t.Fatalf("providers = %+v", c.Providers)
	}
}

func TestLoadLmstudio(t *testing.T) {
	p := writeFile(t, `providers:
  - name: lmstudio
    type: lmstudio
    api_key: ${LMSTUDIO_API_KEY}
  - name: custom
    type: lmstudio
    api_key: ${LMSTUDIO_API_KEY}
    base_url: http://gateway.example/v1
routes:
  - match: "lmstudio-*"
    targets:
      - provider: lmstudio
  - match: "custom/*"
    targets:
      - provider: custom
`)
	c, err := Load(p, []string{"LMSTUDIO_API_KEY=k"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Provider{
		{Name: "lmstudio", Type: "lmstudio", APIKey: "k"},
		{Name: "custom", Type: "lmstudio", BaseURL: "http://gateway.example/v1", APIKey: "k"},
	}
	if !reflect.DeepEqual(c.Providers, want) {
		t.Fatalf("providers = %+v", c.Providers)
	}
}

func TestLoadMistral(t *testing.T) {
	p := writeFile(t, `providers:
  - name: mistral
    type: mistral
    api_key: ${MISTRAL_API_KEY}
  - name: custom
    type: mistral
    api_key: ${MISTRAL_API_KEY}
    base_url: https://gateway.example/v1
routes:
  - match: "mistral-*"
    targets:
      - provider: mistral
  - match: "custom/*"
    targets:
      - provider: custom
`)
	c, err := Load(p, []string{"MISTRAL_API_KEY=k"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Provider{
		{Name: "mistral", Type: "mistral", APIKey: "k"},
		{Name: "custom", Type: "mistral", BaseURL: "https://gateway.example/v1", APIKey: "k"},
	}
	if !reflect.DeepEqual(c.Providers, want) {
		t.Fatalf("providers = %+v", c.Providers)
	}
}

func TestLoadNvidia(t *testing.T) {
	p := writeFile(t, `providers:
  - name: nvidia
    type: nvidia
    api_key: ${NVIDIA_API_KEY}
  - name: custom
    type: nvidia
    api_key: ${NVIDIA_API_KEY}
    base_url: https://gateway.example/v1
routes:
  - match: "nvidia-*"
    targets:
      - provider: nvidia
  - match: "custom/*"
    targets:
      - provider: custom
`)
	c, err := Load(p, []string{"NVIDIA_API_KEY=k"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Provider{
		{Name: "nvidia", Type: "nvidia", APIKey: "k"},
		{Name: "custom", Type: "nvidia", BaseURL: "https://gateway.example/v1", APIKey: "k"},
	}
	if !reflect.DeepEqual(c.Providers, want) {
		t.Fatalf("providers = %+v", c.Providers)
	}
}

func TestLoadGroq(t *testing.T) {
	p := writeFile(t, `providers:
  - name: groq
    type: groq
    api_key: ${GROQ_API_KEY}
  - name: custom
    type: groq
    api_key: ${GROQ_API_KEY}
    base_url: https://gateway.example/openai/v1
routes:
  - match: "groq-*"
    targets:
      - provider: groq
  - match: "custom/*"
    targets:
      - provider: custom
`)
	c, err := Load(p, []string{"GROQ_API_KEY=k"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Provider{
		{Name: "groq", Type: "groq", APIKey: "k"},
		{Name: "custom", Type: "groq", BaseURL: "https://gateway.example/openai/v1", APIKey: "k"},
	}
	if !reflect.DeepEqual(c.Providers, want) {
		t.Fatalf("providers = %+v", c.Providers)
	}
}

func TestLoadCerebras(t *testing.T) {
	p := writeFile(t, `providers:
  - name: cerebras
    type: cerebras
    api_key: ${CEREBRAS_API_KEY}
  - name: custom
    type: cerebras
    api_key: ${CEREBRAS_API_KEY}
    base_url: https://gateway.example/v1
routes:
  - match: "cerebras-*"
    targets:
      - provider: cerebras
  - match: "custom/*"
    targets:
      - provider: custom
`)
	c, err := Load(p, []string{"CEREBRAS_API_KEY=k"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Provider{
		{Name: "cerebras", Type: "cerebras", APIKey: "k"},
		{Name: "custom", Type: "cerebras", BaseURL: "https://gateway.example/v1", APIKey: "k"},
	}
	if !reflect.DeepEqual(c.Providers, want) {
		t.Fatalf("providers = %+v", c.Providers)
	}
}

func TestLoadSiliconflow(t *testing.T) {
	p := writeFile(t, `providers:
  - name: siliconflow
    type: siliconflow
    api_key: ${SILICONFLOW_API_KEY}
  - name: custom
    type: siliconflow
    api_key: ${SILICONFLOW_API_KEY}
    base_url: https://gateway.example/v1
routes:
  - match: "siliconflow-*"
    targets:
      - provider: siliconflow
  - match: "custom/*"
    targets:
      - provider: custom
`)
	c, err := Load(p, []string{"SILICONFLOW_API_KEY=k"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Provider{
		{Name: "siliconflow", Type: "siliconflow", APIKey: "k"},
		{Name: "custom", Type: "siliconflow", BaseURL: "https://gateway.example/v1", APIKey: "k"},
	}
	if !reflect.DeepEqual(c.Providers, want) {
		t.Fatalf("providers = %+v", c.Providers)
	}
}

func TestLoadXAI(t *testing.T) {
	p := writeFile(t, `providers:
  - name: xai
    type: xai
    api_key: ${XAI_API_KEY}
  - name: us
    type: xai
    api_key: ${XAI_API_KEY}
    base_url: https://us.api.x.ai/v1
routes:
  - match: "grok-*"
    targets:
      - provider: xai
  - match: "us-*"
    targets:
      - provider: us
`)
	c, err := Load(p, []string{"XAI_API_KEY=xai-test"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Provider{
		{Name: "xai", Type: "xai", APIKey: "xai-test"},
		{Name: "us", Type: "xai", BaseURL: "https://us.api.x.ai/v1", APIKey: "xai-test"},
	}
	if !reflect.DeepEqual(c.Providers, want) {
		t.Fatalf("providers = %+v", c.Providers)
	}
}

func TestLoadGitHubProvider(t *testing.T) {
	p := writeFile(t, `providers:
  - name: github
    type: github
    api_key: ghp_test
routes:
  - match: "*"
    targets:
      - provider: github
`)
	c, err := Load(p, nil)
	if err != nil {
		t.Fatalf("type github should load: %v", err)
	}
	if want := (Provider{Name: "github", Type: "github", APIKey: "ghp_test"}); !reflect.DeepEqual(c.Providers[0], want) {
		t.Fatalf("provider = %+v", c.Providers[0])
	}
}

func TestLoadProvidersAndRoutes(t *testing.T) {
	p := writeFile(t, `providers:
  - name: openai
    type: openai
    api_key: ${OPENAI_KEY}
  - name: local
    type: ollama
routes:
  - match: "gpt-*"
    targets:
      - provider: openai
  - match: local
    targets:
      - provider: local
        model: llama3.2
`)
	c, err := Load(p, []string{"OPENAI_KEY=sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	wantProviders := []Provider{{Name: "openai", Type: "openai", APIKey: "sk-test"}, {Name: "local", Type: "ollama"}}
	wantRoutes := []Route{
		{Match: "gpt-*", Targets: []Target{{Provider: "openai"}}},
		{Match: "local", Targets: []Target{{Provider: "local", Model: "llama3.2"}}},
	}
	if !reflect.DeepEqual(c.Providers, wantProviders) {
		t.Fatalf("providers = %+v", c.Providers)
	}
	if !reflect.DeepEqual(c.Routes, wantRoutes) {
		t.Fatalf("routes = %+v", c.Routes)
	}
}

func TestLoadIgnoresUnknownEnv(t *testing.T) {
	environ := []string{
		"TRANSPONDER_PORT=tcp://10.0.0.1:8080",
		"TRANSPONDER_SERVICE_HOST=10.0.0.1",
		"TRANSPONDER_PORT_8080_TCP_ADDR=10.0.0.1",
		"TRANSPONDER_STORE=oops",
		"TRANSPONDER_STORE__NOPE=1",
		"TRANSPONDER_LOG_LEVEL=warn",
		"TRANSPONDER_PROVIDERS=x",
		"TRANSPONDER_ROUTES=y",
	}
	c, err := Load("", environ)
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "warn" || c.Store.SQLite != "./transponder.db" {
		t.Fatalf("config = %+v", c)
	}
}

func TestLoadUpdateCheck(t *testing.T) {
	c, err := Load(writeFile(t, "update_check: true\n"), nil)
	if err != nil || !c.UpdateCheck {
		t.Fatalf("from file: %+v %v", c, err)
	}
	c, err = Load("", []string{"TRANSPONDER_UPDATE_CHECK=true"})
	if err != nil || !c.UpdateCheck {
		t.Fatalf("from env: %v", err)
	}
	c, err = Load("", nil)
	if err != nil || c.UpdateCheck {
		t.Fatalf("default must be off: %+v %v", c, err)
	}
}

func TestLoadIntEnvOverride(t *testing.T) {
	c, err := Load("", []string{"TRANSPONDER_SETTINGS__COOLDOWN_FAILURES=5"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Settings.CooldownFailures != 5 {
		t.Fatalf("cooldown_failures = %d", c.Settings.CooldownFailures)
	}
	if _, err := Load("", []string{"TRANSPONDER_SETTINGS__COOLDOWN_FAILURES=many"}); err == nil || !strings.Contains(err.Error(), "many") {
		t.Fatalf("err = %v, want it to name the value", err)
	}
}

func TestLoadExpandsEdgeCases(t *testing.T) {
	env := []string{"HOST=h", "SECRET=a #b: c", "QUOTED=\"x' *y", "LINES=one\ntwo", "N=7", "ON=true", "USD=2.5"}
	tests := []struct {
		name string
		body string
		want func(*Bootstrap) bool
	}{
		{
			name: "a comment indented less than block content is not content",
			body: "secrets_key: |-\n    a${HOST}\n  # ${UNSET}\nlog_level: info\n",
			want: func(c *Bootstrap) bool { return c.SecretsKey == "ah" },
		},
		{
			name: "a quoted scalar over lines with a hash line is expanded",
			body: "secrets_key: \"a\n  #${HOST}\"\n",
			want: func(c *Bootstrap) bool { return c.SecretsKey == "a #h" },
		},
		{
			name: "a block header with an anchor is a block",
			body: "secrets_key: &k |-\n  #${HOST}\n",
			want: func(c *Bootstrap) bool { return c.SecretsKey == "#h" },
		},
		{
			name: "a block header with a tag is a block",
			body: "secrets_key: !!str |-\n  #${HOST}\n",
			want: func(c *Bootstrap) bool { return c.SecretsKey == "#h" },
		},
		{
			name: "a comment after a value is not expanded",
			body: "secrets_key: ${HOST} # was ${UNSET}\n",
			want: func(c *Bootstrap) bool { return c.SecretsKey == "h" },
		},
		{
			name: "a value with a hash and a colon is kept whole",
			body: "secrets_key: ${SECRET}\n",
			want: func(c *Bootstrap) bool { return c.SecretsKey == "a #b: c" },
		},
		{
			name: "a value with quotes and a star is kept whole",
			body: "secrets_key: ${QUOTED}\n",
			want: func(c *Bootstrap) bool { return c.SecretsKey == "\"x' *y" },
		},
		{
			name: "a value over two lines is kept whole",
			body: "secrets_key: ${LINES}\nlog_level: info\n",
			want: func(c *Bootstrap) bool { return c.SecretsKey == "one\ntwo" },
		},
		{
			name: "a variable inside a flow list",
			body: "providers: [{name: e, type: echo}]\nroutes:\n  - {match: '*', allow: [user:${HOST}], targets: [{provider: e}]}\n",
			want: func(c *Bootstrap) bool {
				return len(c.Routes) == 1 && reflect.DeepEqual(c.Routes[0].Allow, []string{"user:h"})
			},
		},
		{
			name: "plain values keep their type",
			body: "update_check: ${ON}\nsettings:\n  cooldown_failures: ${N}\nbudgets:\n  - {subject: 'user:*', period: daily, limit_usd: ${USD}}\n",
			want: func(c *Bootstrap) bool {
				return c.UpdateCheck && c.Settings.CooldownFailures == 7 && len(c.Budgets) == 1 && c.Budgets[0].LimitUSD == 2.5
			},
		},
		{
			name: "a quoted value stays a string",
			body: "secrets_key: \"${N}\"\n",
			want: func(c *Bootstrap) bool { return c.SecretsKey == "7" },
		},
		{
			name: "an alias repeats the expanded value",
			body: "secrets_key: &k ${HOST}x\nprovider_note: *k\n",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(writeFile(t, tt.body), env)
			if tt.want == nil {
				if err == nil || strings.Contains(err.Error(), "environment") {
					t.Fatalf("err = %v, want the unknown key refused", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tt.want(c) {
				t.Fatalf("config = %+v", c)
			}
		})
	}
}

func TestLoadRejectsBadBoolEnv(t *testing.T) {
	if _, err := Load("", []string{"TRANSPONDER_UPDATE_CHECK=maybe"}); err == nil {
		t.Fatal("expected an error for a value that is not true or false")
	}
}

const limitsYAML = `
default_profile: free
profiles:
  - name: free
    priority: 1
    limits:
      requests: {rate: 60/m, concurrency: 4}
      llm.tokens: {per_day: 100000}
      tools.calls: {rate: 30/m}
  - name: pro
    limits:
      llm.tokens: {per_day: 2000000}
profile_bindings:
  - {subject: "user:alice", profile: pro}
budgets:
  - {subject: "user:*", period: monthly, limit_usd: 20}
  - {subject: "team:platform", period: daily, limit_usd: 50.5}
limits:
  store: memory
  on_store_error: local
`

func TestLoadLimits(t *testing.T) {
	c, err := Load(writeFile(t, limitsYAML), nil)
	if err != nil {
		t.Fatal(err)
	}
	lc, err := c.LimitsConfig()
	if err != nil {
		t.Fatal(err)
	}
	if lc.DefaultProfile != "free" || len(lc.Profiles) != 2 || lc.Profiles[0].Concurrency != 4 || lc.Profiles[1].TokensPerDay != 2000000 ||
		len(lc.Bindings) != 1 || lc.Bindings[0].Profile != "pro" || len(lc.Budgets) != 2 || lc.Budgets[1].LimitNUSD != 50_500_000_000 {
		t.Fatalf("limits %+v", lc)
	}
	empty, err := (&Bootstrap{}).LimitsConfig()
	if err != nil || !empty.Empty() {
		t.Fatalf("no limits configured: %+v %v", empty, err)
	}
}

func TestLoadProfilesFile(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "profiles.yaml")
	if err := os.WriteFile(shared, []byte("profiles:\n  - name: team\n    limits:\n      requests: {rate: 10/s}\n      memory.items: 5\ndefault_profile: team\nprofile_bindings:\n  - {subject: \"key:ci\", profile: team}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(writeFile(t, "profiles_file: "+filepath.ToSlash(shared)+"\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	lc, err := c.LimitsConfig()
	if err != nil || lc.DefaultProfile != "team" || len(lc.Profiles) != 1 || lc.Profiles[0].RequestRate == nil || len(lc.Bindings) != 1 {
		t.Fatalf("limits %+v %v", lc, err)
	}
	if _, err := Load(writeFile(t, "profiles_file: "+filepath.ToSlash(filepath.Join(t.TempDir(), "missing.yaml"))+"\n"), nil); err == nil {
		t.Fatal("missing profiles file accepted")
	}
}

func TestLoadLimitsRejects(t *testing.T) {
	for name, body := range map[string]string{
		"unknown llm key":         "profiles:\n  - name: p\n    limits:\n      llm.speed: 1\n",
		"unknown default":         "default_profile: gold\n",
		"binding to unknown":      "profiles:\n  - name: p\n    limits: {}\nprofile_bindings:\n  - {subject: \"user:a\", profile: q}\n",
		"binding bad subject":     "profiles:\n  - name: p\n    limits: {}\nprofile_bindings:\n  - {subject: \"org:a\", profile: p}\n",
		"duplicate profile":       "profiles:\n  - name: p\n    limits: {}\n  - name: p\n    limits: {}\n",
		"profile without name":    "profiles:\n  - limits: {}\n",
		"bad budget":              "budgets:\n  - {subject: \"user:*\", period: weekly, limit_usd: 1}\n",
		"store not supported yet": "limits: {store: valkey}\n",
		"sql without postgres":    "limits: {store: sql}\n",
		"unknown store":           "limits: {store: redis}\n",
		"bad on_store_error":      "limits: {on_store_error: panic}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeFile(t, body), nil); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestOpsListenAddr(t *testing.T) {
	c, err := Load("", nil)
	if err != nil || c.OpsListenAddr != "" {
		t.Fatalf("default ops_listen_addr = %q, err %v; want none", c.OpsListenAddr, err)
	}
	p := writeFile(t, "ops_listen_addr: \"127.0.0.1:9464\"\n")
	if c, err = Load(p, nil); err != nil || c.OpsListenAddr != "127.0.0.1:9464" {
		t.Fatalf("from the file: %q, err %v", c.OpsListenAddr, err)
	}
	if c, err = Load(p, []string{"TRANSPONDER_OPS_LISTEN_ADDR=:9000"}); err != nil || c.OpsListenAddr != ":9000" {
		t.Fatalf("from the environment: %q, err %v", c.OpsListenAddr, err)
	}
}

func TestOTel(t *testing.T) {
	c, err := Load("", nil)
	if err != nil || c.OTel.Endpoint != "" {
		t.Fatalf("tracing is on by default: %+v, err %v", c.OTel, err)
	}
	p := writeFile(t, "otel:\n  endpoint: https://otlp.example.com/otlp\n  headers:\n    Authorization: Bearer ${OTEL_KEY}\n  sample_ratio: 0.1\n")
	c, err = Load(p, []string{"OTEL_KEY=k1"})
	if err != nil {
		t.Fatal(err)
	}
	want := OTel{Endpoint: "https://otlp.example.com/otlp", Headers: map[string]string{"Authorization": "Bearer k1"}, SampleRatio: 0.1}
	if !reflect.DeepEqual(c.OTel, want) {
		t.Fatalf("otel = %+v, want %+v", c.OTel, want)
	}
	c, err = Load("", []string{"TRANSPONDER_OTEL__ENDPOINT=http://collector:4318", "TRANSPONDER_OTEL__SAMPLE_RATIO=0.25"})
	if err != nil || c.OTel.Endpoint != "http://collector:4318" || c.OTel.SampleRatio != 0.25 {
		t.Fatalf("from the environment: %+v, err %v", c.OTel, err)
	}

	for name, tt := range map[string]struct {
		body string
		env  []string
		want string
	}{
		"grpc address":             {body: "otel: {endpoint: \"collector:4317\"}\n", want: "otel.endpoint"},
		"another scheme":           {body: "otel: {endpoint: \"grpc://collector:4317\"}\n", want: "otel.endpoint"},
		"ratio above 1":            {body: "otel: {endpoint: \"http://c:4318\", sample_ratio: 2}\n", want: "otel.sample_ratio"},
		"negative ratio":           {body: "otel: {endpoint: \"http://c:4318\", sample_ratio: -1}\n", want: "otel.sample_ratio"},
		"headers with no endpoint": {body: "otel: {headers: {a: b}}\n", want: "otel.endpoint is empty"},
		"ratio with no endpoint":   {body: "otel: {sample_ratio: 0.5}\n", want: "otel.endpoint is empty"},
		"ratio that is no number":  {env: []string{"TRANSPONDER_OTEL__ENDPOINT=http://c:4318", "TRANSPONDER_OTEL__SAMPLE_RATIO=half"}, want: "half"},
	} {
		t.Run(name, func(t *testing.T) {
			path := ""
			if tt.body != "" {
				path = writeFile(t, tt.body)
			}
			if _, err := Load(path, tt.env); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to name %q", err, tt.want)
			}
		})
	}
}

func TestSharedLimits(t *testing.T) {
	tests := []struct {
		name, body string
		want       bool
	}{
		{"sqlite", "", false},
		{"postgres", "store: {postgres: postgres://db/t}\n", true},
		{"postgres with sql", "store: {postgres: postgres://db/t}\nlimits: {store: sql}\n", true},
		{"postgres with memory", "store: {postgres: postgres://db/t}\nlimits: {store: memory}\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(writeFile(t, tt.body), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := c.SharedLimits(); got != tt.want {
				t.Fatalf("SharedLimits = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadProfilesFileRejectsUnknownKeys(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "profiles.yaml")
	if err := os.WriteFile(shared, []byte("profiles: []\nprofile_binding:\n  - {subject: \"key:ci\", profile: team}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(writeFile(t, "profiles_file: "+filepath.ToSlash(shared)+"\n"), nil); err == nil || !strings.Contains(err.Error(), "profile_binding") {
		t.Fatalf("a misspelt key in the profiles file was accepted: %v", err)
	}
}
