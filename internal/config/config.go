// Package config loads the bootstrap settings transponder needs before it can reach its database.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/provider/registry"
)

const envPrefix = "TRANSPONDER_"

// Bootstrap holds the settings read from the YAML file and the environment.
type Bootstrap struct {
	ListenAddr string `yaml:"listen_addr"`
	Store      Store  `yaml:"store"`
	SecretsKey string `yaml:"secrets_key"`
	OTel       OTel   `yaml:"otel"`
	LogLevel   string `yaml:"log_level"`
	// UpdateCheck logs a line at start and once a day when a newer release is out.
	UpdateCheck bool `yaml:"update_check"`

	Providers []Provider `yaml:"providers"`
	Routes    []Route    `yaml:"routes"`
	Settings  Settings   `yaml:"settings"`

	Profiles        []ProfileConfig `yaml:"profiles"`
	ProfilesFile    string          `yaml:"profiles_file"`
	DefaultProfile  string          `yaml:"default_profile"`
	ProfileBindings []BindingConfig `yaml:"profile_bindings"`
	Budgets         []BudgetConfig  `yaml:"budgets"`
	Limits          LimitsStore     `yaml:"limits"`
}

// ProfileConfig is a named set of limits in the shared mothership format.
type ProfileConfig struct {
	Name     string         `yaml:"name"`
	Priority int            `yaml:"priority"`
	Limits   map[string]any `yaml:"limits"`
}

// BindingConfig gives a key:, user: or team: subject a profile.
type BindingConfig struct {
	Subject string `yaml:"subject"`
	Profile string `yaml:"profile"`
}

// BudgetConfig caps what a subject spends in USD per day or month.
type BudgetConfig struct {
	Subject  string  `yaml:"subject"`
	Period   string  `yaml:"period"`
	LimitUSD float64 `yaml:"limit_usd"`
}

// LimitsStore says where limit counters are kept.
type LimitsStore struct {
	Store        string `yaml:"store"`
	OnStoreError string `yaml:"on_store_error"`
}

// sharedProfiles is what transponder reads from profiles_file.
type sharedProfiles struct {
	Profiles        []ProfileConfig `yaml:"profiles"`
	DefaultProfile  string          `yaml:"default_profile"`
	ProfileBindings []BindingConfig `yaml:"profile_bindings"`
}

// LimitsConfig turns the profiles, bindings and budgets into what the limiter enforces.
func (c *Bootstrap) LimitsConfig() (limits.Config, error) {
	var out limits.Config
	names := map[string]bool{}
	for i, pc := range c.Profiles {
		if pc.Name == "" {
			return out, fmt.Errorf("config: profiles[%d] has no name", i)
		}
		if names[pc.Name] {
			return out, fmt.Errorf("config: profile %q is declared twice", pc.Name)
		}
		names[pc.Name] = true
		p, err := limits.ParseProfile(pc.Name, pc.Limits)
		if err != nil {
			return out, fmt.Errorf("config: %w", err)
		}
		out.Profiles = append(out.Profiles, p)
	}
	if c.DefaultProfile != "" && !names[c.DefaultProfile] {
		return out, fmt.Errorf("config: default_profile %q is not a declared profile", c.DefaultProfile)
	}
	out.DefaultProfile = c.DefaultProfile
	for _, b := range c.ProfileBindings {
		kind, id, _ := strings.Cut(b.Subject, ":")
		if id == "" || (kind != "key" && kind != "user" && kind != "team") {
			return out, fmt.Errorf("config: profile binding subject %q must be key:<name>, user:<id> or team:<name>", b.Subject)
		}
		if !names[b.Profile] {
			return out, fmt.Errorf("config: profile binding for %s names %q, which is not a declared profile", b.Subject, b.Profile)
		}
		out.Bindings = append(out.Bindings, limits.Binding{Subject: b.Subject, Profile: b.Profile})
	}
	for _, bc := range c.Budgets {
		b, err := limits.ParseBudget(bc.Subject, bc.Period, bc.LimitUSD)
		if err != nil {
			return out, fmt.Errorf("config: %w", err)
		}
		out.Budgets = append(out.Budgets, b)
	}
	return out, nil
}

// loadProfilesFile adds the profiles, default and bindings of the shared profiles file.
func (c *Bootstrap) loadProfilesFile() error {
	raw, err := os.ReadFile(filepath.Clean(c.ProfilesFile))
	if err != nil {
		return fmt.Errorf("config: reading profiles_file: %w", err)
	}
	var shared sharedProfiles
	if err := yaml.Unmarshal(raw, &shared); err != nil {
		return fmt.Errorf("config: parsing %s: %w", c.ProfilesFile, err)
	}
	c.Profiles = append(c.Profiles, shared.Profiles...)
	if c.DefaultProfile == "" {
		c.DefaultProfile = shared.DefaultProfile
	}
	c.ProfileBindings = append(c.ProfileBindings, shared.ProfileBindings...)
	return nil
}

// Settings tune how calls to providers are made.
type Settings struct {
	// AttemptTimeout is how long an upstream may take to answer with headers.
	AttemptTimeout time.Duration `yaml:"attempt_timeout"`
	// RequestTimeout bounds a whole call, streaming included.
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

// Provider is one backend declared in the file.
type Provider struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"`
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
}

// Route sends the models matching Match to its targets.
type Route struct {
	Match   string   `yaml:"match"`
	Targets []Target `yaml:"targets"`
}

// Target is one provider a route may use. An empty Model keeps the requested model.
type Target struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// Store selects the database. Exactly one of the fields is set.
type Store struct {
	SQLite   string `yaml:"sqlite"`
	Postgres string `yaml:"postgres"`
}

// OTel configures trace export.
type OTel struct {
	Endpoint string `yaml:"endpoint"`
}

// Load reads the file at path, which may be empty, and applies TRANSPONDER_ overrides from environ.
// Variables that do not name a config key are ignored, since platforms such as Kubernetes inject
// TRANSPONDER_PORT and similar for a service of the same name.
func Load(path string, environ []string) (*Bootstrap, error) {
	env := envMap(environ)
	tree := map[string]any{}
	if path != "" {
		raw, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return nil, fmt.Errorf("reading config: %w", err)
		}
		expanded, err := expand(string(raw), env)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal([]byte(expanded), &tree); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		if tree == nil {
			tree = map[string]any{}
		}
	}
	applyEnv(tree, environ)

	c := &Bootstrap{ListenAddr: ":8080", LogLevel: "info"}
	merged, err := yaml.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("encoding config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(merged))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.Store.SQLite == "" && c.Store.Postgres == "" {
		c.Store.SQLite = "./transponder.db"
	}
	if c.ProfilesFile != "" {
		if err := c.loadProfilesFile(); err != nil {
			return nil, err
		}
	}
	if c.Settings.AttemptTimeout == 0 {
		c.Settings.AttemptTimeout = time.Minute
	}
	if c.Settings.RequestTimeout == 0 {
		c.Settings.RequestTimeout = 10 * time.Minute
	}
	return c, c.validate()
}

func (c *Bootstrap) validate() error {
	switch c.Limits.Store {
	case "", "memory":
	case "sql", "valkey":
		return fmt.Errorf("config: limits.store %q is not supported yet; use memory", c.Limits.Store)
	default:
		return fmt.Errorf("config: limits.store %q must be memory", c.Limits.Store)
	}
	switch c.Limits.OnStoreError {
	case "", "local", "deny":
	default:
		return fmt.Errorf("config: limits.on_store_error %q must be local or deny", c.Limits.OnStoreError)
	}
	if _, err := c.LimitsConfig(); err != nil {
		return err
	}
	if c.Store.SQLite != "" && c.Store.Postgres != "" {
		return errors.New("config: set only one of store.sqlite and store.postgres")
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: log_level %q must be debug, info, warn or error", c.LogLevel)
	}
	if c.Settings.AttemptTimeout < 0 {
		return fmt.Errorf("config: settings.attempt_timeout %s must be positive", c.Settings.AttemptTimeout)
	}
	if c.Settings.RequestTimeout < 0 {
		return fmt.Errorf("config: settings.request_timeout %s must be positive", c.Settings.RequestTimeout)
	}
	names := map[string]bool{}
	for i, p := range c.Providers {
		switch {
		case p.Name == "":
			return fmt.Errorf("config: providers[%d] has no name", i)
		case names[p.Name]:
			return fmt.Errorf("config: provider %q is declared twice", p.Name)
		}
		spec, ok := registry.Lookup(p.Type)
		if !ok {
			return fmt.Errorf("config: provider %q has unknown type %q; known types are %s", p.Name, p.Type, strings.Join(registry.Types(), ", "))
		}
		if spec.NeedsBaseURL && p.BaseURL == "" {
			return fmt.Errorf("config: provider %q of type %s needs base_url", p.Name, p.Type)
		}
		names[p.Name] = true
	}
	if len(c.Providers) > 0 && len(c.Routes) == 0 {
		return errors.New("config: providers are declared but no routes send models to them")
	}
	for i, r := range c.Routes {
		if r.Match == "" {
			return fmt.Errorf("config: routes[%d] has no match", i)
		}
		if len(r.Targets) == 0 {
			return fmt.Errorf("config: route %q has no targets", r.Match)
		}
		for _, t := range r.Targets {
			if !names[t.Provider] {
				return fmt.Errorf("config: route %q uses provider %q, which is not declared", r.Match, t.Provider)
			}
		}
	}
	return nil
}

func envMap(environ []string) map[string]string {
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

func expand(s string, env map[string]string) (string, error) {
	var missing []string
	out := os.Expand(s, func(name string) string {
		v, ok := env[name]
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("config: environment variables not set: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func applyEnv(tree map[string]any, environ []string) {
	known := leafPaths(reflect.TypeFor[Bootstrap](), "")
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || len(k) <= len(envPrefix) || !strings.EqualFold(k[:len(envPrefix)], envPrefix) {
			continue
		}
		name := strings.ToLower(k[len(envPrefix):])
		kind, isKnown := known[name]
		if !isKnown {
			continue
		}
		path := strings.Split(name, "__")
		node := tree
		for _, part := range path[:len(path)-1] {
			child, isMap := node[part].(map[string]any)
			if !isMap {
				child = map[string]any{}
				node[part] = child
			}
			node = child
		}
		var value any = v
		if kind == reflect.Bool {
			if b, err := strconv.ParseBool(v); err == nil {
				value = b
			}
		}
		node[path[len(path)-1]] = value
	}
}

func leafPaths(t reflect.Type, prefix string) map[string]reflect.Kind {
	out := map[string]reflect.Kind{}
	for f := range t.Fields() {
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		if f.Type.Kind() == reflect.Slice {
			continue
		}
		if f.Type.Kind() == reflect.Struct {
			for p, k := range leafPaths(f.Type, prefix+tag+"__") {
				out[p] = k
			}
			continue
		}
		out[prefix+tag] = f.Type.Kind()
	}
	return out
}
