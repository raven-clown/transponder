package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/raven-clown/transponder/internal/admin"
	"github.com/raven-clown/transponder/internal/config"
)

const rotateHelp = `usage: transponder secrets rotate [--config FILE]

Seals every provider key stored through the admin API again under secrets_key. Set
secrets_key to the new key and previous_secrets_key (TRANSPONDER_PREVIOUS_SECRETS_KEY) to the
old one, run this once, then remove previous_secrets_key.
`

// runSecrets runs a transponder secrets command.
func runSecrets(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "rotate" {
		_, _ = fmt.Fprint(stderr, rotateHelp)
		return 2
	}
	fs := flag.NewFlagSet("transponder secrets rotate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to the YAML config file")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	cfg, err := config.Load(*configPath, os.Environ())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "transponder: %v\n", err)
		return 1
	}
	n, err := rotateSecrets(context.Background(), cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "transponder: secrets rotate: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "transponder: sealed %d provider keys again under secrets_key; previous_secrets_key can be removed\n", n)
	return 0
}

func rotateSecrets(ctx context.Context, cfg *config.Bootstrap) (int, error) {
	if cfg.PreviousSecretsKey == "" {
		return 0, errors.New("set previous_secrets_key (or TRANSPONDER_PREVIOUS_SECRETS_KEY) to the key the provider keys are sealed under now, and secrets_key to the new one")
	}
	box, err := openSecrets(cfg)
	if err != nil {
		return 0, err
	}
	db, err := openStore(ctx, cfg.Store)
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	return admin.Reseal(ctx, db, box, time.Now)
}
