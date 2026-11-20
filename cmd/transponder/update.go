package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/update"
)

// updateChecker reads the release list of this repository. Tests point it at
// a local server.
var updateChecker = &update.Checker{Repo: "raven-clown/transponder", Client: &http.Client{Timeout: 15 * time.Second}}

// updateInterval is how often update_check looks again after the first check.
var updateInterval = 24 * time.Hour

// runCheckUpdate prints what the newer releases change. A config that fails to
// load still allows the check, without marking the changes that touch it.
func runCheckUpdate(configPath string, stdout, stderr io.Writer) int {
	var used map[string]bool
	if cfg, err := config.Load(configPath, os.Environ()); err == nil {
		used = usedFeatures(cfg)
	} else {
		_, _ = fmt.Fprintf(stderr, "transponder: %v, so no change is matched to your config\n", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rep, err := updateChecker.Check(ctx, version)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "transponder: checking for updates: %v\n", err)
		return 1
	}
	rep.Render(stdout, "transponder", used)
	return 0
}

// usedFeatures lists the Affects keys that match a config, so a change to a
// provider type the config uses is shown as one that affects it.
func usedFeatures(cfg *config.Bootstrap) map[string]bool {
	used := map[string]bool{}
	for _, p := range cfg.Providers {
		used["provider:"+p.Type] = true
	}
	if len(cfg.Providers) == 0 {
		used["provider:echo"] = true
	}
	return used
}

// watchUpdates checks once at start and then every interval, and logs one
// line when a newer release is out. Failures are logged at debug level, since
// a gateway without internet access is normal.
func watchUpdates(ctx context.Context, logger *slog.Logger, c *update.Checker, used map[string]bool, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		logUpdate(ctx, logger, c, used)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func logUpdate(ctx context.Context, logger *slog.Logger, c *update.Checker, used map[string]bool) {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rep, err := c.Check(checkCtx, version)
	if err != nil {
		logger.Debug("update check failed", "error", err.Error())
		return
	}
	if rep.UpToDate() || rep.Dev {
		return
	}
	affects := 0
	for _, m := range rep.Releases {
		for _, ch := range m.Changes {
			for _, a := range ch.Affects {
				if used[a] {
					affects++
					break
				}
			}
		}
	}
	latest := rep.Releases[len(rep.Releases)-1]
	logger.Info("update available",
		"current", version, "latest", rep.Latest, "releases_behind", len(rep.Releases),
		"urgency", rep.Level(), "changes_affecting_config", affects, "notes", latest.URL,
		"details", "run transponder --check-update")
}
