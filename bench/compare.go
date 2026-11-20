package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

func readReport(path string) (report, error) {
	var rep report
	b, err := os.ReadFile(path) //nolint:gosec // a report the caller named
	if err != nil {
		return rep, err
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		return rep, fmt.Errorf("%s: %w", path, err)
	}
	return rep, nil
}

// change is how b differs from a, in percent.
func change(a, b float64) string {
	if a == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%+.0f%%", (b-a)/a*100)
}

// runMerge combines reports of separate runs of one binary into one, each number the median,
// so runs of two binaries can take turns and share the machine's drift.
func runMerge(args []string, stdout io.Writer) error {
	if len(args) < 3 {
		return errors.New("merge takes the file to write and at least two reports")
	}
	var out report
	runs := map[string][]row{}
	var order []string
	for _, path := range args[1:] {
		rep, err := readReport(path)
		if err != nil {
			return err
		}
		if out.Rows == nil {
			out = rep
			out.Count = 0
		}
		out.Count += rep.Count
		for _, r := range rep.Rows {
			if _, ok := runs[r.Scenario]; !ok {
				order = append(order, r.Scenario)
			}
			runs[r.Scenario] = append(runs[r.Scenario], r)
		}
	}
	out.Rows = nil
	for _, name := range order {
		out.Rows = append(out.Rows, median(runs[name]))
	}
	writeTable(stdout, out)
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(args[0], append(b, '\n'), 0o600)
}

// runCompare prints two reports side by side, the first as before and the second as after.
func runCompare(args []string, stdout io.Writer) error {
	if len(args) != 2 {
		return errors.New("compare takes two report files, before and after")
	}
	before, err := readReport(args[0])
	if err != nil {
		return err
	}
	after, err := readReport(args[1])
	if err != nil {
		return err
	}
	old := map[string]row{}
	for _, r := range before.Rows {
		old[r.Scenario] = r
	}
	a, b := before.Label, after.Label
	_, _ = fmt.Fprintf(stdout, "| Scenario | req/s %s | req/s %s | p50 added %s | p50 added %s | p99 added %s | p99 added %s | Peak memory %s | Peak memory %s |\n", a, b, a, b, a, b, a, b)
	_, _ = fmt.Fprintln(stdout, "|---|---:|---:|---:|---:|---:|---:|---:|---:|")
	for _, n := range after.Rows {
		o, ok := old[n.Scenario]
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(stdout, "| %s | %.0f | %.0f (%s) | %.2f ms | %.2f ms | %.2f ms | %.2f ms | %.0f MiB | %.0f MiB |\n",
			n.Scenario, o.Throughput, n.Throughput, change(o.Throughput, n.Throughput), o.P50, n.P50, o.P99, n.P99, o.PeakMiB, n.PeakMiB)
	}
	return nil
}
