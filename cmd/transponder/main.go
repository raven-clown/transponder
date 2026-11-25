// Command transponder is an LLM gateway with fallback, per-user cost tracking and budgets.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/raven-clown/transponder/internal/admin"
	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/pricing"
	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/provider/registry"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/runtime"
	"github.com/raven-clown/transponder/internal/secrets"
	"github.com/raven-clown/transponder/internal/server"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/postgres"
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
	if len(args) > 0 && args[0] == "secrets" {
		return runSecrets(args[1:], stdout, stderr)
	}
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
	started := time.Now()
	logger, err := telemetry.NewLogger(stdout, cfg.LogLevel)
	if err != nil {
		return err
	}

	db, err := openStore(ctx, cfg.Store)
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

	box, err := openSecrets(cfg)
	if err != nil {
		return err
	}
	if err := admin.SyncFile(ctx, db, cfg, box, time.Now, logger); err != nil {
		return fmt.Errorf("storing the providers, routes, budgets and profiles of the config file: %w", err)
	}

	client := providerClient()
	limiters := &limiterSource{build: func(lc limits.Config) (*limits.Limiter, func(), error) {
		return buildLimiterFor(ctx, cfg, lc, db, logger, time.Now)
	}}
	defer limiters.close()
	reloader, err := runtime.New(ctx, db, func(ctx context.Context, _ int64) (live, error) {
		return loadStored(ctx, cfg, db, box, client, limiters)
	}, logger, time.Now)
	if err != nil {
		return err
	}
	metrics := telemetry.NewMetrics(version)
	reloader.OnReload = metrics.ConfigReloaded
	reloadCtx, stopReloads := context.WithCancel(ctx)
	reloadsDone := make(chan struct{})
	go func() {
		defer close(reloadsDone)
		reloader.Run(reloadCtx, runtime.DefaultInterval)
	}()
	// The loop reads the store, so it stops before the deferred Close.
	defer func() {
		stopReloads()
		<-reloadsDone
	}()
	stopOps, err := serveOps(ctx, cfg.OpsListenAddr, server.Ops(ready, metrics), logger)
	if err != nil {
		return err
	}
	defer stopOps()
	tracer, flushSpans, err := startTracing(cfg.OTel, metrics, logger)
	if err != nil {
		return err
	}
	// Runs once the open requests have ended, so their spans are in the last export.
	defer flushSpans()
	gw := server.New(server.Deps{
		Auth: authn,
		Live: func() (*router.Router, *limits.Limiter) {
			c := reloader.Current().Value
			return c.router, c.limiter
		},
		Prices:      pricing.Default(),
		Usage:       db,
		Logger:      logger,
		Now:         time.Now,
		LogRequests: cfg.LogRequests,

		AttemptTimeout: cfg.Settings.AttemptTimeout,
		RequestTimeout: cfg.Settings.RequestTimeout,
		Ready:          ready,
		Metrics:        metrics,
		Tracer:         tracer,
		Admin: admin.New(admin.Deps{
			Auth:      authn,
			Store:     db,
			Reload:    reloader,
			Logger:    logger,
			Now:       time.Now,
			Version:   version,
			StoreKind: storeKind(cfg.Store),
			Started:   started,
			File:      cfg,
			Secrets:   box,
			Router: func() *router.Router {
				return reloader.Current().Value.router
			},
		}),
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

// database is what serve needs from a store, whichever database holds it.
type database interface {
	admin.Store
	Close() error
}

func storeKind(cfg config.Store) string {
	if cfg.Postgres != "" {
		return "postgres"
	}
	return "sqlite"
}

// openStore opens the database the config names: Postgres when store.postgres is set, which
// several replicas can share, and otherwise the SQLite file.
func openStore(ctx context.Context, cfg config.Store) (database, error) {
	if cfg.Postgres != "" {
		return postgres.Open(ctx, cfg.Postgres)
	}
	return sqlite.Open(ctx, cfg.SQLite)
}

// live is the runtime config a reload swaps: what a call reads when it starts.
type live struct {
	router  *router.Router
	limiter *limits.Limiter
}

// loadLive loads and validates the whole runtime config. The limiter keeps base's counters, so a
// reload changes the rules and never resets what callers have used.
//
// The providers, routes and limits come from the file for now. The admin API will keep them in
// the store, and read them here, so every replica picks up its writes.
func loadLive(cfg *config.Bootstrap, client *http.Client, limiters *limiterSource) (live, error) {
	rt, err := buildRouter(cfg, client)
	if err != nil {
		return live{}, err
	}
	lc, err := cfg.LimitsConfig()
	if err != nil {
		return live{}, err
	}
	rt.KeepHealth(limiters.router)
	l := live{router: rt}
	if l.limiter, err = limiters.limiter(lc); err != nil {
		return live{}, err
	}
	limiters.router = rt
	return l, nil
}

// loadStored loads the runtime config from the store, where the file's items and the admin
// API's writes are, and builds it.
func loadStored(ctx context.Context, cfg *config.Bootstrap, db admin.RuntimeStore, box *secrets.Box, client *http.Client, limiters *limiterSource) (live, error) {
	rc, err := admin.Runtime(ctx, db, box, cfg)
	if err != nil {
		return live{}, err
	}
	return loadLive(rc, client, limiters)
}

// limiterSource keeps the limiter's counters. They are made the first time a config limits
// anything, so a replica that started with no limits can take budgets added through the
// admin API, and a reload keeps them, so it never resets what callers have used.
type limiterSource struct {
	build func(lc limits.Config) (*limits.Limiter, func(), error)
	base  *limits.Limiter
	stop  func()
	// router is the last router loaded, whose record of failing targets the next one keeps,
	// so a reload does not end the cooldowns.
	router *router.Router
}

// limiter returns a limiter for lc on the kept counters, or nil when nothing is limited and
// there are no counters yet. Loads are serialised by the reloader, so it takes no lock.
func (s *limiterSource) limiter(lc limits.Config) (*limits.Limiter, error) {
	if s.base == nil {
		if lc.Empty() || s.build == nil {
			return nil, nil
		}
		base, stop, err := s.build(lc)
		if err != nil {
			return nil, err
		}
		s.base, s.stop = base, stop
	}
	return s.base.WithConfig(lc), nil
}

func (s *limiterSource) close() {
	if s.stop != nil {
		s.stop()
	}
}

// openSecrets returns what seals provider keys: secrets_key when it is set, and with SQLite a
// key kept in transponder.key next to the database otherwise. With Postgres and no
// secrets_key it returns nil, and provider keys cannot be stored through the API. With
// previous_secrets_key, values sealed under that key still open.
func openSecrets(cfg *config.Bootstrap) (*secrets.Box, error) {
	box, err := currentSecrets(cfg)
	if err != nil || cfg.PreviousSecretsKey == "" {
		return box, err
	}
	key, err := secrets.ParseKey(cfg.PreviousSecretsKey)
	if err != nil {
		return nil, fmt.Errorf("previous_secrets_key: %w", err)
	}
	prev, err := secrets.New(key)
	if err != nil {
		return nil, err
	}
	return box.WithPrevious(prev), nil
}

func currentSecrets(cfg *config.Bootstrap) (*secrets.Box, error) {
	if cfg.SecretsKey != "" {
		key, err := secrets.ParseKey(cfg.SecretsKey)
		if err != nil {
			return nil, err
		}
		return secrets.New(key)
	}
	if cfg.Store.Postgres != "" {
		return nil, nil
	}
	return secrets.LoadOrCreate(filepath.Join(filepath.Dir(cfg.Store.SQLite), "transponder.key"))
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

// buildRouter turns the providers and routes of the file into a router. With no providers and
// no routes, every model goes to a built-in echo provider so a bare start still answers. With
// providers but no routes, as after the last route is deleted, no model has a route.
func buildRouter(cfg *config.Bootstrap, client *http.Client) (*router.Router, error) {
	if len(cfg.Routes) == 0 && len(cfg.Providers) == 0 {
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
		access, err := router.ParseAccess(r.Allow)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", r.Match, err)
		}
		rt := router.Route{Match: r.Match, Access: access}
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

// buildLimiter returns the limiter for the profiles and budgets in cfg, and a function that
// stops it. It returns nil when nothing is limited.
//
// With shared limits the counters live in Postgres, and this month's usage fills them the first
// time any replica starts there. Otherwise they live in this process and are filled from the
// usage table at every start, so a restart does not reset anyone's spend.
func buildLimiter(ctx context.Context, cfg *config.Bootstrap, db database, logger *slog.Logger, now func() time.Time) (*limits.Limiter, func(), error) {
	lc, err := cfg.LimitsConfig()
	if err != nil || lc.Empty() {
		return nil, func() {}, err
	}
	return buildLimiterFor(ctx, cfg, lc, db, logger, now)
}

// buildLimiterFor is buildLimiter for the limits lc, wherever they came from.
func buildLimiterFor(ctx context.Context, cfg *config.Bootstrap, lc limits.Config, db database, logger *slog.Logger, now func() time.Time) (*limits.Limiter, func(), error) {
	seeded := func() (*limits.MemoryStore, error) {
		mem := limits.NewMemoryStore()
		l := limits.New(lc, mem, now)
		err := db.UsageSince(ctx, store.DefaultOrg, limits.Month.Start(now()), func(r store.UsageRecord) error {
			tokens := r.InputTokens + r.CacheReadTokens + r.CacheWriteTokens + r.OutputTokens + r.ReasoningTokens
			l.Seed(ctx, limits.Caller{User: r.User, Teams: r.Teams, Key: r.Key}, r.Time, tokens, r.CostNUSD)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("reading this month's usage for the limits: %w", err)
		}
		return mem, nil
	}
	pg, isPostgres := db.(*postgres.DB)
	if !cfg.SharedLimits() || !isPostgres {
		mem, err := seeded()
		if err != nil {
			return nil, nil, err
		}
		return limits.New(lc, mem, now), func() {}, nil
	}
	counters, err := pg.Counters(ctx, logger)
	if err != nil {
		return nil, nil, err
	}
	if _, err := counters.SeedOnce(ctx, seeded); err != nil {
		_ = counters.Close()
		return nil, nil, fmt.Errorf("filling the shared limit counters: %w", err)
	}
	shared := &limits.Failover{
		Shared:   counters,
		Local:    limits.NewMemoryStore(),
		Deny:     cfg.Limits.OnStoreError == "deny",
		Replicas: counters.Replicas,
		OnChange: func(err error) {
			if err != nil {
				logger.Warn("the shared limit counters are unreachable", "err", err, "on_store_error", cmp.Or(cfg.Limits.OnStoreError, "local"))
				return
			}
			logger.Info("the shared limit counters answer again")
		},
	}
	return limits.New(lc, shared, now), func() { _ = counters.Close() }, nil
}
