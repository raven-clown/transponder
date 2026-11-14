package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestLoadEnvOverrides(t *testing.T) {
	p := writeFile(t, "listen_addr: \":8081\"\nstore:\n  sqlite: a.db\n")
	c, err := Load(p, []string{"TRANSPONDER_LISTEN_ADDR=:9000", "transponder_store__sqlite=/tmp/x.db", "PATH=/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":9000" || c.Store.SQLite != "/tmp/x.db" {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		environ []string
		path    string
		want    string
	}{
		{name: "unknown file key", body: "listen_adr: x\n"},
		{name: "both stores", body: "store:\n  sqlite: a.db\n  postgres: postgres://x\n"},
		{name: "bad log level", body: "log_level: loud\n"},
		{name: "missing file", path: filepath.Join(t.TempDir(), "nope.yaml")},
		{name: "provider without name", body: "providers:\n  - type: echo\n", want: "providers[0]"},
		{name: "duplicate provider", body: "providers:\n  - {name: a, type: echo}\n  - {name: a, type: echo}\n", want: `"a"`},
		{name: "unknown provider type", body: "providers:\n  - {name: b, type: bedrock}\n", want: "bedrock"},
		{name: "negative cooldown", body: "settings:\n  cooldown: -1s\n", want: "cooldown"},
		{name: "negative cooldown failures", body: "settings:\n  cooldown_failures: -1\n", want: "cooldown_failures"},
		{name: "negative attempt timeout", body: "settings:\n  attempt_timeout: -1s\n", want: "attempt_timeout"},
		{name: "request timeout not a duration", body: "settings:\n  request_timeout: soon\n", want: "`soon`"},
		{name: "unknown type lists the known ones", body: "providers:\n  - {name: b, type: bedrock}\n", want: "known types are anthropic, cloudflare, echo, gemini"},
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
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not name %q", err, tt.want)
			}
		})
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
		"bad on_store_error":      "limits: {on_store_error: panic}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeFile(t, body), nil); err == nil {
				t.Fatal("accepted")
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
