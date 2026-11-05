// Command transponder is an LLM gateway with fallback, per-user cost tracking and budgets.
package main

import (
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/pricing"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/provider/ollama"
	"github.com/raven-clown/transponder/internal/provider/openai"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/server"
	"github.com/raven-clown/transponder/internal/store/sqlite"
)

var version = "dev"

// shutdownTimeout is how long open requests get to finish after a stop signal.
var shutdownTimeout = 10 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("transponder", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	configPath := fs.String("config", "", "path to the YAML config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintf(stdout, "transponder %s\n", version)
		return 0
	}
	cfg, err := config.Load(*configPath, os.Environ())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "transponder: %v\n", err)
		return 1
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
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToUpper(cfg.LogLevel))); err != nil {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: level}))

	db, err := sqlite.Open(ctx, cfg.Store.SQLite)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := auth.EnsureAdmin(ctx, db, time.Now, stderr); err != nil {
		return err
	}

	rt, err := buildRouter(cfg, &http.Client{Timeout: 10 * time.Minute})
	if err != nil {
		return err
	}
	gw := server.New(server.Deps{
		Auth:   auth.NewAuthenticator(db, time.Now),
		Router: rt,
		Prices: pricing.Default(),
		Usage:  db,
		Logger: logger,
		Now:    time.Now,
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

// buildRouter turns the providers and routes of the file into a router. With no routes,
// every model goes to a built-in echo provider so a bare start still answers.
func buildRouter(cfg *config.Bootstrap, client *http.Client) (*router.Router, error) {
	if len(cfg.Routes) == 0 {
		return router.New([]router.Route{{Match: "*", Targets: []router.Target{{Provider: echo.New("echo"), Type: "echo"}}}}), nil
	}
	providers := map[string]router.Target{}
	for _, p := range cfg.Providers {
		var prov provider.Provider
		switch p.Type {
		case "echo":
			prov = echo.New(p.Name)
		case "openai":
			prov = openai.New(openai.Options{Name: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey, Client: client})
		case "ollama":
			prov = ollama.New(p.Name, p.BaseURL, client)
		default:
			return nil, fmt.Errorf("provider %q has unknown type %q", p.Name, p.Type)
		}
		providers[p.Name] = router.Target{Provider: prov, Type: p.Type}
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
	return router.New(routes), nil
}
