// Command sim is what the demo runs next to transponder: a fake provider, a tool that puts
// a few API keys in the store, and a steady flow of calls. It is not part of the gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage: sim <command> [flags]

  upstream   answer chat and embeddings like an OpenAI-compatible provider
  seed       put the demo's API keys into a transponder SQLite store
  traffic    send a mix of calls to transponder until stopped
  probe      exit 0 when a URL answers 200, for a container health check
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "upstream":
		err = runUpstream(ctx, args[1:], stdout, stderr)
	case "seed":
		err = runSeed(ctx, args[1:], stdout, stderr)
	case "traffic":
		err = runTraffic(ctx, args[1:], stdout, stderr)
	case "probe":
		err = runProbe(ctx, args[1:], stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "sim: unknown command %q\n%s", args[0], usage)
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "sim: %v\n", err)
		return 1
	}
	return 0
}

// runProbe is the health check of the demo's containers, which have no shell or curl.
func runProbe(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("probe takes one URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fs.Arg(0), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", fs.Arg(0), resp.Status)
	}
	return nil
}
