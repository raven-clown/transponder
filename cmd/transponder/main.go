// Command transponder is an LLM gateway with fallback, per-user cost tracking and budgets.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/pricing"
	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/provider/registry"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/server"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/sqlite"
	"github.com/raven-clown/transponder/internal/telemetry"
)

var version = "dev"

// readBuildInfo is swapped in tests, since test binaries carry no VCS stamp.
var readBuildInfo = debug.ReadBuildInfo

// shutdownTimeout is how long open requests get to finish after a stop signal.
var shutdownTimeout = 10 * time.Second

// versionString describes the build: the version, and the commit and time when
// the toolchain recorded them.
func versionString(version string, info *debug.BuildInfo, ok bool) string {
	out := "transponder " + version
	if !ok || info == nil {
		return out
	}
	settings := make(map[string]string, len(info.Settings))
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	rev, when := settings["vcs.revision"], settings["vcs.time"]
	if rev == "" || when == "" {
		return out
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	detail := rev + ", " + when
	if settings["vcs.modified"] == "true" {
		detail += ", modified"
	}
	return out + " (" + detail + ")"
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("transponder", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	configPath := fs.String("config", "", "path to the YAML config file")
	configTest := fs.Bool("config-test", false, "check the configuration and exit without starting")
	checkUpdate := fs.Bool("check-update", false, "list newer releases, how urgent they are and which changes touch this config, then exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *checkUpdate {
		return runCheckUpdate(*configPath, stdout, stderr)
	}
	if *showVersion {
		info, ok := readBuildInfo()
		_, _ = fmt.Fprintln(stdout, versionString(version, info, ok))
		return 0
	}
	cfg, err := config.Load(*configPath, os.Environ())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "transponder: %v\n", err)
		return 1
	}
	if *configTest {
		if _, err := buildRouter(cfg, nil); err != nil {
			_, _ = fmt.Fprintf(stderr, "transponder: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, "transponder: config ok")
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go stopOnContextDone(ctx, stop)
	defer stop()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "transponder: %v\n", err)
		return 1
	}
	if err := serve(ctx, cfg, ln, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "transponder: %v\n", err)
		return 1
	}
	return 0
}

func stopOnContextDone(ctx context.Context, stop context.CancelFunc) {
	<-ctx.Done()
	stop()
}

func serve(ctx context.Context, cfg *config.Bootstrap, ln net.Listener, stdout, stderr io.Writer) error {
	if cfg.Store.Postgres != "" {
		return errors.New("postgres store is not supported yet")
	}
	logger, err := telemetry.NewLogger(stdout, cfg.LogLevel)
	if err != nil {
		return err
	}

	db, err := sqlite.Open(ctx, cfg.Store.SQLite)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	authn, oidc, err := buildAuth(cfg, db, &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()})
	if err != nil {
		return err
	}
	ready := map[string]server.Pinger{"store": db}
	if oidc != nil {
		ready["oidc"] = oidc
	}
	// Someone who signs in as an admin can make keys, so no key is printed for them.
	if oidc == nil || !oidc.MapsAdmin() {
		if err := auth.EnsureAdmin(ctx, db, time.Now, stderr); err != nil {
			return err
		}
	}

	rt, err := buildRouter(cfg, &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()})
	if err != nil {
		return err
	}
	limiter, err := buildLimiter(ctx, cfg, db, time.Now)
	if err != nil {
		return err
	}
	gw := server.New(server.Deps{
		Auth:   authn,
		Router: rt,
		Prices: pricing.Default(),
		Usage:  db,
		Logger: logger,
		Now:    time.Now,

		AttemptTimeout: cfg.Settings.AttemptTimeout,
		RequestTimeout: cfg.Settings.RequestTimeout,
		Ready:          ready,
		Limiter:        limiter,
	})
	// Requests run on their own context so a drain that runs out of time can end them,
	// and the handlers are counted so their usage is written before the store closes.
	reqCtx, endRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer endRequests()
	var inflight sync.WaitGroup
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inflight.Add(1)
			defer inflight.Done()
			gw.ServeHTTP(w, r)
		}),
		BaseContext:       func(net.Listener) context.Context { return reqCtx },
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	logger.Info("listening", "addr", ln.Addr().String(), "version", version)
	if cfg.UpdateCheck {
		go watchUpdates(ctx, logger, updateChecker, usedFeatures(cfg), updateInterval)
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("open requests did not finish in time, ending them", "timeout", shutdownTimeout.String())
		endRequests()
		inflight.Wait()
		_ = srv.Close()
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// buildAuth returns what checks callers: API keys from keys, and tokens from the OIDC issuer
// when one is configured. The verifier is nil when it is not.
func buildAuth(cfg *config.Bootstrap, keys store.KeyStore, client *http.Client) (*auth.Authenticator, *auth.OIDC, error) {
	authn := auth.NewAuthenticator(keys, time.Now)
	oc := cfg.Auth.OIDC
	if !oc.Enabled() {
		return authn, nil, nil
	}
	mapping := make(map[string]auth.Role, len(oc.RoleMapping))
	for value, name := range oc.RoleMapping {
		role, err := auth.ParseRole(name)
		if err != nil {
			return nil, nil, fmt.Errorf("auth.oidc.role_mapping for %q: %w", value, err)
		}
		mapping[value] = role
	}
	oidc, err := auth.NewOIDC(auth.OIDCConfig{
		Issuer:      oc.Issuer,
		Audience:    oc.Audience,
		JWKSURL:     oc.JWKSURL,
		UserClaim:   oc.UserClaim,
		TeamsClaim:  oc.TeamsClaim,
		RolesClaim:  oc.RolesClaim,
		RoleMapping: mapping,
		ClockSkew:   oc.ClockSkew,
		Client:      client,
	})
	if err != nil {
		return nil, nil, err
	}
	return authn.WithOIDC(oidc), oidc, nil
}

// buildRouter turns the providers and routes of the file into a router. With no routes,
// every model goes to a built-in echo provider so a bare start still answers.
func buildRouter(cfg *config.Bootstrap, client *http.Client) (*router.Router, error) {
	if len(cfg.Routes) == 0 {
		return router.New([]router.Route{{Match: "*", Targets: []router.Target{{Provider: echo.New("echo"), Type: "echo", Free: true}}}}), nil
	}
	providers := map[string]router.Target{}
	for _, p := range cfg.Providers {
		spec, ok := registry.Lookup(p.Type)
		if !ok {
			return nil, fmt.Errorf("provider %q has unknown type %q", p.Name, p.Type)
		}
		prov := spec.New(registry.Settings{Name: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey, Client: client})
		providers[p.Name] = router.Target{Provider: prov, Type: p.Type, Free: spec.Free}
	}
	routes := make([]router.Route, 0, len(cfg.Routes))
	for _, r := range cfg.Routes {
		rt := router.Route{Match: r.Match}
		for _, t := range r.Targets {
			target, ok := providers[t.Provider]
			if !ok {
				return nil, fmt.Errorf("route %q uses provider %q, which is not declared", r.Match, t.Provider)
			}
			target.Model = t.Model
			rt.Targets = append(rt.Targets, target)
		}
		routes = append(routes, rt)
	}
	r := router.New(routes)
	if cfg.Settings.CooldownFailures > 0 && cfg.Settings.Cooldown > 0 {
		r.SetCooldown(cfg.Settings.CooldownFailures, cfg.Settings.Cooldown, time.Now)
	}
	return r, nil
}

// buildLimiter returns the limiter for the profiles and budgets in cfg, with this month's usage
// added back so a restart does not reset anyone's spend. It returns nil when nothing is limited.
func buildLimiter(ctx context.Context, cfg *config.Bootstrap, usage store.UsageStore, now func() time.Time) (*limits.Limiter, error) {
	lc, err := cfg.LimitsConfig()
	if err != nil || lc.Empty() {
		return nil, err
	}
	l := limits.New(lc, limits.NewMemoryStore(), now)
	err = usage.UsageSince(ctx, store.DefaultOrg, limits.Month.Start(now()), func(r store.UsageRecord) error {
		tokens := r.InputTokens + r.CacheReadTokens + r.CacheWriteTokens + r.OutputTokens + r.ReasoningTokens
		l.Seed(ctx, limits.Caller{User: r.User, Teams: r.Teams, Key: r.Key}, r.Time, tokens, r.CostNUSD)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading this month's usage for the limits: %w", err)
	}
	return l, nil
}
