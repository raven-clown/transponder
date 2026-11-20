// Command bench measures transponder under load on one machine: a fake upstream with a fixed
// latency and body size, and a load tool that reports throughput, added latency, errors and
// memory. See docs/benchmarks.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage: bench <command> [flags]

  run        start a transponder binary against the fake upstream and measure every scenario
  upstream   serve the fake upstream on its own
  load       drive one URL at a fixed rate and report latency, errors and throughput
  compare    print two reports from run -out as a before and after table
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "run":
		err = runSuite(ctx, args[1:], stdout, stderr)
	case "upstream":
		err = runUpstream(ctx, args[1:], stdout, stderr)
	case "load":
		err = runLoad(ctx, args[1:], stdout, stderr)
	case "compare":
		err = runCompare(args[1:], stdout)
	default:
		_, _ = fmt.Fprintf(stderr, "bench: unknown command %q\n%s", args[0], usage)
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "bench: %v\n", err)
		return 1
	}
	return 0
}
