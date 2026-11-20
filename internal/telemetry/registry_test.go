package telemetry

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

func expose(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	n, err := r.WriteTo(&b)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(b.Len()) {
		t.Fatalf("WriteTo reported %d bytes, wrote %d", n, b.Len())
	}
	return b.String()
}

func TestRegistryExposition(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("t_requests_total", "Requests.", "route", "code")
	g := r.Gauge("t_inflight", "In flight.")
	h := r.Histogram("t_seconds", "Seconds.", []float64{0.1, 1}, "route")

	c.Add(1, "b", "200")
	c.Add(2, "a", "200")
	c.Add(0.5, "a", "200")
	g.Add(3)
	g.Add(-1)
	h.Observe(0.05, "a")
	h.Observe(0.1, "a") // on the bound counts in that bucket
	h.Observe(0.5, "a")
	h.Observe(7, "a")

	want := `# HELP t_requests_total Requests.
# TYPE t_requests_total counter
t_requests_total{route="a",code="200"} 2.5
t_requests_total{route="b",code="200"} 1
# HELP t_inflight In flight.
# TYPE t_inflight gauge
t_inflight 2
# HELP t_seconds Seconds.
# TYPE t_seconds histogram
t_seconds_bucket{route="a",le="0.1"} 2
t_seconds_bucket{route="a",le="1"} 3
t_seconds_bucket{route="a",le="+Inf"} 4
t_seconds_sum{route="a"} 7.65
t_seconds_count{route="a"} 4
`
	if got := expose(t, r); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRegistrySetAndEmptyHistogram(t *testing.T) {
	r := NewRegistry()
	r.Gauge("t_info", "Info.", "version").Set(1, "v1")
	r.Histogram("t_empty_seconds", "Nothing observed.", []float64{1})
	want := `# HELP t_info Info.
# TYPE t_info gauge
t_info{version="v1"} 1
# HELP t_empty_seconds Nothing observed.
# TYPE t_empty_seconds histogram
`
	if got := expose(t, r); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRegistryEscapesLabelValues(t *testing.T) {
	r := NewRegistry()
	r.Counter("t_total", "T.", "v").Add(1, "a\"b\\c\nd")
	if got, want := expose(t, r), `t_total{v="a\"b\\c\nd"} 1`+"\n"; !strings.HasSuffix(got, want) {
		t.Fatalf("got %q, want it to end with %q", got, want)
	}
}

func TestRegistryCapsSeries(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("t_total", "T.", "a", "b")
	for i := range maxSeries + 50 {
		c.Add(1, strconv.Itoa(i), "x")
	}
	// One past the cap is the series that takes the overflow.
	if n := len(c.series); n != maxSeries+1 {
		t.Fatalf("%d series, want %d", n, maxSeries+1)
	}
	if got := expose(t, r); !strings.Contains(got, `t_total{a="other",b="other"} 50`+"\n") {
		t.Fatal("the overflow series does not hold the 50 values past the cap")
	}
	// A series that existed before the cap keeps counting on its own.
	c.Add(1, "0", "x")
	if got := expose(t, r); !strings.Contains(got, `t_total{a="0",b="x"} 2`+"\n") {
		t.Fatal("an existing series stopped counting after the cap")
	}
}

func TestRegistryRejectsMistakes(t *testing.T) {
	tests := []struct {
		name     string
		register func(r *Registry)
	}{
		{"name twice", func(r *Registry) { r.Counter("t_total", "T."); r.Gauge("t_total", "T.") }},
		{"too many labels", func(r *Registry) { r.Counter("t_total", "T.", "a", "b", "c", "d", "e", "f") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("no panic")
				}
			}()
			tt.register(NewRegistry())
		})
	}
}

func TestRegistryConcurrentUse(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("t_total", "T.", "worker")
	h := r.Histogram("t_seconds", "S.", []float64{1})
	const workers, each = 8, 500
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for range each {
				c.Add(1, strconv.Itoa(w%2))
				h.Observe(0.5)
				_, _ = r.WriteTo(&strings.Builder{})
			}
		})
	}
	wg.Wait()
	got := expose(t, r)
	for _, line := range []string{
		`t_total{worker="0"} 2000`,
		`t_total{worker="1"} 2000`,
		`t_seconds_count 4000`,
		`t_seconds_sum 2000`,
	} {
		if !strings.Contains(got, line+"\n") {
			t.Fatalf("missing %q in:\n%s", line, got)
		}
	}
}
