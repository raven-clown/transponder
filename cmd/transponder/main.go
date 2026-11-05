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
	"syscall"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/pricing"
	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/server"
	"github.com/raven-clown/transponder/internal/store/sqlite"
)

var version = "dev"

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

	srv := &http.Server{
		Handler: server.New(server.Deps{
			Auth:   auth.NewAuthenticator(db, time.Now),
			Router: router.New([]router.Route{{Match: "*", Targets: []router.Target{{Provider: echo.New("echo"), Type: "echo"}}}}),
			Prices: pricing.Default(),
			Usage:  db,
			Logger: logger,
			Now:    time.Now,
		}),
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
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
