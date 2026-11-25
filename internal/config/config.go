// Package config loads the bootstrap settings transponder needs before it can reach its database.
package config

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/provider/registry"
	"github.com/raven-clown/transponder/internal/router"
)

const envPrefix = "TRANSPONDER_"

// Bootstrap holds the settings read from the YAML file and the environment.
type Bootstrap struct {
	ListenAddr string `yaml:"listen_addr"`
	Store      Store  `yaml:"store"`
	SecretsKey string `yaml:"secrets_key"`
	// PreviousSecretsKey still opens provider keys sealed before secrets_key changed, until
	// transponder secrets rotate seals them again under secrets_key.
	PreviousSecretsKey string `yaml:"previous_secrets_key"`
	OTel               OTel   `yaml:"otel"`
	LogLevel           string `yaml:"log_level"`
	// UpdateCheck logs a line at start and once a day when a newer release is out.
	UpdateCheck bool `yaml:"update_check"`
	// OpsListenAddr serves /healthz, /readyz and /metrics on a port of their own, where
	// /metrics needs no key. Empty leaves them on ListenAddr only.
	OpsListenAddr string `yaml:"ops_listen_addr"`

	Auth      Auth       `yaml:"auth"`
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

// Auth says how callers sign in, next to API keys.
type Auth struct {
	OIDC OIDC `yaml:"oidc"`
}

// OIDC accepts tokens from one OpenID Connect issuer. It is off while Issuer is empty.
type OIDC struct {
	// Issuer is the iss claim of the tokens, and where the discovery document is.
	Issuer string `yaml:"issuer"`
	// Audience must be among the aud claim of a token.
	Audience string `yaml:"audience"`
	// JWKSURL is where the signing keys are, for an issuer without a discovery document.
	JWKSURL string `yaml:"jwks_url"`
	// UserClaim holds the user id. Default sub.
	UserClaim string `yaml:"user_claim"`
	// TeamsClaim holds the user's teams. Default groups.
	TeamsClaim string `yaml:"teams_claim"`
	// RolesClaim holds the values RoleMapping reads.
	RolesClaim string `yaml:"roles_claim"`
	// RoleMapping gives admin, viewer or member to a value of RolesClaim.
	RoleMapping map[string]string `yaml:"role_mapping"`
	// ClockSkew is how far a token's exp and nbf may be off. Default 1m.
	ClockSkew time.Duration `yaml:"clock_skew"`
}

// Enabled reports whether tokens from an OIDC issuer are accepted.
func (o OIDC) Enabled() bool { return o.Issuer != "" }

// validate checks the OIDC settings that can be checked without reaching the issuer.
func (o OIDC) validate() error {
	if !o.Enabled() {
		if o.Audience != "" || o.JWKSURL != "" || o.RolesClaim != "" || len(o.RoleMapping) > 0 {
			return errors.New("config: auth.oidc is set but auth.oidc.issuer is empty")
		}
		return nil
	}
	if u, err := url.Parse(o.Issuer); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("config: auth.oidc.issuer %q must be an http or https URL", o.Issuer)
	}
	if o.Audience == "" {
		return errors.New("config: auth.oidc.audience is needed, so tokens issued for another service are refused")
	}
	if o.JWKSURL != "" {
		if u, err := url.Parse(o.JWKSURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("config: auth.oidc.jwks_url %q must be an http or https URL", o.JWKSURL)
		}
	}
	if len(o.RoleMapping) > 0 && o.RolesClaim == "" {
		return errors.New("config: auth.oidc.role_mapping needs auth.oidc.roles_claim")
	}
	for value, role := range o.RoleMapping {
		switch role {
		case "admin", "viewer", "member":
		default:
			return fmt.Errorf("config: auth.oidc.role_mapping gives %q the role %q, which must be admin, viewer or member", value, role)
		}
	}
	if o.ClockSkew < 0 {
		return fmt.Errorf("config: auth.oidc.clock_skew %s must be positive", o.ClockSkew)
	}
	return nil
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

// LimitsStore says where limit counters are kept: memory in this process, or sql in the Postgres
// store so every replica shares them. Empty means sql with store.postgres and memory otherwise.
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
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&shared); err != nil && !errors.Is(err, io.EOF) {
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
	// CooldownFailures is how many failures in a row put a target last in its routes.
	CooldownFailures int `yaml:"cooldown_failures"`
	// Cooldown is how long such a target stays last.
	Cooldown time.Duration `yaml:"cooldown"`
	// ShutdownTimeout is how long open calls get to finish after a stop signal.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
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
	Match string `yaml:"match"`
	// Allow lists who may use the route: user:<id>, team:<name>, key:<name> or role:<role>.
	// Empty lets every authenticated caller in.
	Allow   []string `yaml:"allow"`
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

// OTel configures trace export. It is off while Endpoint is empty.
type OTel struct {
	// Endpoint is the base URL of an OTLP/HTTP collector, such as http://collector:4318.
	Endpoint string `yaml:"endpoint"`
	// Headers are sent with every export, for a collector that needs a key.
	Headers map[string]string `yaml:"headers"`
	// SampleRatio is the share of traces that start at the gateway that are recorded, from 0
	// to 1. Zero means all. A call that arrives with a traceparent follows its sampled flag.
	SampleRatio float64 `yaml:"sample_ratio"`
}

// validate checks the trace export settings without reaching the collector.
func (o OTel) validate() error {
	if o.Endpoint == "" {
		if len(o.Headers) > 0 || o.SampleRatio != 0 {
			return errors.New("config: otel is set but otel.endpoint is empty")
		}
		return nil
	}
	if u, err := url.Parse(o.Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// The value is not repeated: it can hold a collector's key.
		return errors.New("config: otel.endpoint must be an http or https URL of an OTLP/HTTP collector, usually port 4318; OTLP over gRPC is not supported")
	}
	if o.SampleRatio < 0 || o.SampleRatio > 1 {
		return fmt.Errorf("config: otel.sample_ratio %v must be between 0 and 1", o.SampleRatio)
	}
	return nil
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
		doc, err := expand(raw, env)
		if err != nil {
			var missing missingVarsError
			if errors.As(err, &missing) {
				return nil, err
			}
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		if doc.Kind != 0 {
			if err := doc.Decode(&tree); err != nil {
				return nil, fmt.Errorf("parsing %s: %w", path, err)
			}
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
	if c.Settings.CooldownFailures == 0 {
		c.Settings.CooldownFailures = 3
	}
	if c.Settings.Cooldown == 0 {
		c.Settings.Cooldown = 30 * time.Second
	}
	if c.Settings.ShutdownTimeout == 0 {
		c.Settings.ShutdownTimeout = 10 * time.Second
	}
	if c.Auth.OIDC.Enabled() {
		if c.Auth.OIDC.UserClaim == "" {
			c.Auth.OIDC.UserClaim = "sub"
		}
		if c.Auth.OIDC.TeamsClaim == "" {
			c.Auth.OIDC.TeamsClaim = "groups"
		}
		if c.Auth.OIDC.ClockSkew == 0 {
			c.Auth.OIDC.ClockSkew = time.Minute
		}
	}
	return c, c.validate()
}

func (c *Bootstrap) validate() error {
	if err := checkListenAddrs(c.ListenAddr, c.OpsListenAddr); err != nil {
		return err
	}
	switch c.Limits.Store {
	case "", "memory":
	case "sql":
		if c.Store.Postgres == "" {
			return errors.New("config: limits.store sql keeps the counters in Postgres, so it needs store.postgres")
		}
	case "valkey":
		return errors.New("config: limits.store valkey is not supported yet; use memory or sql")
	default:
		return fmt.Errorf("config: limits.store %q must be memory or sql", c.Limits.Store)
	}
	switch c.Limits.OnStoreError {
	case "", "local", "deny":
	default:
		return fmt.Errorf("config: limits.on_store_error %q must be local or deny", c.Limits.OnStoreError)
	}
	if err := c.Auth.OIDC.validate(); err != nil {
		return err
	}
	if err := c.OTel.validate(); err != nil {
		return err
	}
	if c.Store.SQLite != "" && c.Store.Postgres != "" {
		return errors.New("config: set only one of store.sqlite and store.postgres")
	}
	if c.PreviousSecretsKey != "" && c.SecretsKey == "" && c.Store.Postgres != "" {
		return errors.New("config: previous_secrets_key needs secrets_key, the key provider keys are sealed with from now on")
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: log_level %q must be debug, info, warn or error", c.LogLevel)
	}
	if c.Settings.AttemptTimeout < 0 {
		return fmt.Errorf("config: settings.attempt_timeout %s must be positive", c.Settings.AttemptTimeout)
	}
	if c.Settings.CooldownFailures < 0 {
		return fmt.Errorf("config: settings.cooldown_failures %d must be positive", c.Settings.CooldownFailures)
	}
	if c.Settings.Cooldown < 0 {
		return fmt.Errorf("config: settings.cooldown %s must be positive", c.Settings.Cooldown)
	}
	if c.Settings.RequestTimeout < 0 {
		return fmt.Errorf("config: settings.request_timeout %s must be positive", c.Settings.RequestTimeout)
	}
	if c.Settings.ShutdownTimeout < 0 {
		return fmt.Errorf("config: settings.shutdown_timeout %s must be positive", c.Settings.ShutdownTimeout)
	}
	if err := c.ValidateRuntime(); err != nil {
		return err
	}
	if err := checkRoutesReachable(c.Routes); err != nil {
		return err
	}
	if len(c.Providers) > 0 && len(c.Routes) == 0 {
		return errors.New("config: providers are declared but no routes send models to them")
	}
	return nil
}

// checkRoutesReachable refuses a file route that no call can reach: routes are tried in order
// and the first match wins, so a route with the same match as an earlier one, or any route
// after "*", never gets a call. Routes made through the admin API keep their own positions,
// so this is checked for the file only.
func checkRoutesReachable(routes []Route) error {
	first := make(map[string]int, len(routes))
	catchAll := -1
	for i, r := range routes {
		if catchAll >= 0 {
			return fmt.Errorf("config: routes[%d] %q can never match: routes[%d] matches every model and comes first; move routes[%d] to the end", i, r.Match, catchAll, catchAll)
		}
		if j, ok := first[r.Match]; ok {
			return fmt.Errorf("config: routes[%d] %q can never match: routes[%d] has the same match and comes first; use a different model name for each set of callers", i, r.Match, j)
		}
		first[r.Match] = i
		if r.Match == "*" {
			catchAll = i
		}
	}
	return nil
}

// checkListenAddrs checks both listen keys without opening a socket or asking DNS.
// A value the server cannot listen on stops it at start with an error that does not name
// the key, and two addresses on one port fail there with "address already in use".
func checkListenAddrs(listen, ops string) error {
	host, port, err := splitListenAddr("listen_addr", listen)
	if err != nil {
		return err
	}
	if ops == "" {
		return nil
	}
	opsHost, opsPort, err := splitListenAddr("ops_listen_addr", ops)
	if err != nil {
		return err
	}
	// Port 0 asks for a free one, so two of them cannot collide.
	if port == 0 || opsPort == 0 || port != opsPort {
		return nil
	}
	if sameHost(host, opsHost) || wildcardClash(host, opsHost) {
		return fmt.Errorf("config: listen_addr %q and ops_listen_addr %q both use port %d; they must differ, since an empty host or an unspecified address can reach the other", listen, ops, port)
	}
	return nil
}

// splitListenAddr returns the host and the port number of a listen address, the same pair
// net.Listen would take, so a value that would fail there fails here first with the key.
func splitListenAddr(key, addr string) (host string, port int, err error) {
	rawHost, rawPort, err := net.SplitHostPort(addr)
	if err != nil || rawPort == "" {
		return "", 0, fmt.Errorf("config: %s %q must be host:port, such as :8080", key, addr)
	}
	// A zone names an interface, and only an IPv6 address takes one: net.Listen reads
	// "127.0.0.1%lo" and an empty zone as a name to resolve, and refuses a second % with
	// "invalid argument".
	if bare, zone, hasZone := strings.Cut(rawHost, "%"); hasZone {
		if zone == "" || strings.Contains(zone, "%") || net.ParseIP(bare) == nil || net.ParseIP(bare).To4() != nil {
			return "", 0, fmt.Errorf("config: %s %q has a zone, which only an IPv6 address takes with an interface name", key, addr)
		}
	}
	if n, err := strconv.Atoi(rawPort); err == nil {
		if n < 0 || n > 65535 {
			return "", 0, fmt.Errorf("config: %s %q has the port %q, which must be between 0 and 65535", key, addr, rawPort)
		}
		return rawHost, n, nil
	}
	// A service name is read from the system's service list, which is local.
	n, err := net.DefaultResolver.LookupPort(context.Background(), "tcp", rawPort)
	if err != nil {
		return "", 0, fmt.Errorf("config: %s %q names the port %q, which is neither a number nor a known service", key, addr, rawPort)
	}
	return rawHost, n, nil
}

// sameHost compares two listen hosts by address, so ::1 and 0:0:0:0:0:0:0:1 are one host,
// and a name is compared without case, since a host name is not case-sensitive.
func sameHost(a, b string) bool {
	ia, za := hostParts(a)
	ib, zb := hostParts(b)
	// A name is compared without case, but a zone names an interface, which is case-sensitive.
	if za == "" && zb == "" && strings.EqualFold(a, b) {
		return true
	}
	return za == zb && ia != nil && ib != nil && ia.Equal(ib)
}

// hostParts splits a listen host into its address and its zone, since net.ParseIP does not
// read a zone, and two link-local addresses on different interfaces are different hosts.
func hostParts(host string) (net.IP, string) {
	addr, zone, _ := strings.Cut(host, "%")
	return net.ParseIP(addr), zone
}

// wildcardClash reports whether one host is a wildcard that reaches the other, so neither
// can share the other's port. Go opens a "tcp" listener on an unspecified address
// dual-stack, and net.favoriteAddrFamily treats an IPv4 wildcard as the same case as an IPv6
// one, so 0.0.0.0 and :: each reach every local address. A platform that binds the IPv4
// wildcard IPv4-only would let the pair through, and refusing it there is the safe way to be
// wrong: a config that passes and then cannot start is the bug this check exists to catch.
func wildcardClash(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	return isUnspecifiedHost(a) || isUnspecifiedHost(b)
}

// isUnspecifiedHost reports whether a listen host is an unspecified address. A name is not
// one: it reaches whatever it resolves to, which this check does not look up.
func isUnspecifiedHost(host string) bool {
	ip, _ := hostParts(host)
	return ip != nil && ip.IsUnspecified()
}

// ValidateRuntime checks the part of the config a reload swaps: providers, routes, profiles,
// bindings and budgets, wherever they came from.
func (c *Bootstrap) ValidateRuntime() error {
	if _, err := c.LimitsConfig(); err != nil {
		return err
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
		if p.BaseURL != "" {
			if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("config: provider %q: base_url must be an http or https URL", p.Name)
			}
		}
		names[p.Name] = true
	}
	for i, r := range c.Routes {
		if r.Match == "" {
			return fmt.Errorf("config: routes[%d] has no match", i)
		}
		if len(r.Targets) == 0 {
			return fmt.Errorf("config: route %q has no targets", r.Match)
		}
		if _, err := router.ParseAccess(r.Allow); err != nil {
			return fmt.Errorf("config: route %q: %w", r.Match, err)
		}
		for _, t := range r.Targets {
			if !names[t.Provider] {
				return fmt.Errorf("config: route %q uses provider %q, which is not declared", r.Match, t.Provider)
			}
		}
	}
	return nil
}

// SharedLimits reports whether limit counters are kept in Postgres, shared by every replica.
func (c *Bootstrap) SharedLimits() bool {
	return c.Limits.Store == "sql" || (c.Limits.Store == "" && c.Store.Postgres != "")
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

// missingVarsError names the variables a file uses that the environment does not set.
type missingVarsError []string

func (e missingVarsError) Error() string {
	return "config: environment variables not set: " + strings.Join(e, ", ")
}

// expand parses raw as YAML and replaces ${VAR} and $VAR in its scalars, keys
// included, from env. Each variable is first swapped for a marker made of
// letters and digits, so it parses the same in any YAML context, and only the
// markers left in scalars are filled in. Comments are never expanded, and a
// value is used as written: a # or a newline in it stays part of the value.
// A variable fills part of one scalar; it cannot add keys or list items.
func expand(raw []byte, env map[string]string) (*yaml.Node, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	prefix := "tpenv" + hex.EncodeToString(nonce[:]) + "n"
	var names []string
	marked := os.Expand(string(raw), func(name string) string {
		names = append(names, name)
		return prefix + strconv.Itoa(len(names)-1) + "x"
	})
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(marked), &doc); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return &doc, nil
	}
	marker := regexp.MustCompile(prefix + `([0-9]+)x`)
	missing := map[string]bool{}
	fill := func(m string) string {
		i, err := strconv.Atoi(m[len(prefix) : len(m)-1])
		if err != nil || i >= len(names) {
			return m
		}
		v, ok := env[names[i]]
		if !ok {
			missing[names[i]] = true
		}
		return v
	}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		switch n.Kind {
		case yaml.ScalarNode:
			if !strings.Contains(n.Value, prefix) {
				return
			}
			n.Value = marker.ReplaceAllStringFunc(n.Value, fill)
			// A plain scalar is typed again from its new value, so ${PORT} can still be a number.
			if n.Style&(yaml.TaggedStyle|yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) == 0 {
				n.Tag = ""
			}
		case yaml.AliasNode:
			// The anchored node is expanded where it is defined.
		default:
			for _, c := range n.Content {
				walk(c)
			}
		}
	}
	walk(&doc)
	if len(missing) > 0 {
		out := make(missingVarsError, 0, len(missing))
		for name := range missing {
			out = append(out, name)
		}
		sort.Strings(out)
		return nil, out
	}
	return &doc, nil
}

func applyEnv(tree map[string]any, environ []string) {
	known := leafPaths(reflect.TypeFor[Bootstrap](), "")
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || len(k) <= len(envPrefix) || !strings.EqualFold(k[:len(envPrefix)], envPrefix) {
			continue
		}
		name := strings.ToLower(k[len(envPrefix):])
		typ, isKnown := known[name]
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
		// Left as a string when it does not parse, so the error names what was set.
		var value any = v
		switch typ.Kind() {
		case reflect.Bool:
			if b, err := strconv.ParseBool(v); err == nil {
				value = b
			}
		case reflect.Float64:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				value = f
			}
		case reflect.Int, reflect.Int64:
			// A duration stays a string such as "30s".
			if typ != reflect.TypeFor[time.Duration]() {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					value = n
				}
			}
		}
		node[path[len(path)-1]] = value
	}
}

func leafPaths(t reflect.Type, prefix string) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for f := range t.Fields() {
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		// A list or a map has no single value an environment variable could carry.
		if f.Type.Kind() == reflect.Slice || f.Type.Kind() == reflect.Map {
			continue
		}
		if f.Type.Kind() == reflect.Struct {
			for p, k := range leafPaths(f.Type, prefix+tag+"__") {
				out[p] = k
			}
			continue
		}
		out[prefix+tag] = f.Type
	}
	return out
}
