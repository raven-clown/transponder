package telemetry

import (
	"bufio"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// maxLabels is the most labels a metric can have. Label values are the map key of a series,
// so a fixed size keeps a lookup free of allocations.
const maxLabels = 5

// maxSeries caps the series of one metric. Labels are bounded by the config already; the cap
// holds even if a label turns out not to be, by counting the overflow in one series.
const maxSeries = 5000

// overflow is the label value of the series that takes what does not fit under maxSeries.
const overflow = "other"

const (
	kindCounter   = "counter"
	kindGauge     = "gauge"
	kindHistogram = "histogram"
)

// Registry holds metrics and writes them in the Prometheus text format.
type Registry struct {
	mu       sync.Mutex
	families []*Vec
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Vec is one metric with a series per set of label values.
type Vec struct {
	name, help, kind string
	labels           []string
	buckets          []float64

	mu     sync.RWMutex
	series map[[maxLabels]string]*series
}

type series struct {
	labels string // rendered once, as a="b",c="d"
	value  atomic.Uint64

	// A histogram keeps a count per bucket, not cumulative, and the sum in value.
	counts []atomic.Uint64
}

// Counter registers a counter. It panics on a name used twice or on too many labels, since
// both are mistakes in the code that registers it.
func (r *Registry) Counter(name, help string, labels ...string) *Vec {
	return r.register(&Vec{name: name, help: help, kind: kindCounter, labels: labels})
}

// Gauge registers a gauge.
func (r *Registry) Gauge(name, help string, labels ...string) *Vec {
	return r.register(&Vec{name: name, help: help, kind: kindGauge, labels: labels})
}

// Histogram registers a histogram with the given upper bounds, in ascending order.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Vec {
	return r.register(&Vec{name: name, help: help, kind: kindHistogram, labels: labels, buckets: buckets})
}

func (r *Registry) register(v *Vec) *Vec {
	if len(v.labels) > maxLabels {
		panic("telemetry: metric " + v.name + " has more than " + strconv.Itoa(maxLabels) + " labels")
	}
	v.series = map[[maxLabels]string]*series{}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.families {
		if f.name == v.name {
			panic("telemetry: metric " + v.name + " is registered twice")
		}
	}
	r.families = append(r.families, v)
	return v
}

// Add adds n to the series with the given label values, in the order the labels were declared.
func (v *Vec) Add(n float64, values ...string) {
	addFloat(&v.get(values).value, n)
}

// Set sets a gauge.
func (v *Vec) Set(n float64, values ...string) {
	v.get(values).value.Store(math.Float64bits(n))
}

// Observe adds one observation to a histogram.
func (v *Vec) Observe(n float64, values ...string) {
	s := v.get(values)
	// The last slot is +Inf.
	s.counts[sort.SearchFloat64s(v.buckets, n)].Add(1)
	addFloat(&s.value, n)
}

func addFloat(a *atomic.Uint64, n float64) {
	for {
		old := a.Load()
		if a.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+n)) {
			return
		}
	}
}

func (v *Vec) get(values []string) *series {
	var key [maxLabels]string
	copy(key[:], values)
	v.mu.RLock()
	s := v.series[key]
	v.mu.RUnlock()
	if s != nil {
		return s
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if s = v.series[key]; s != nil {
		return s
	}
	if len(v.series) >= maxSeries {
		for i := range v.labels {
			key[i] = overflow
		}
		if s = v.series[key]; s != nil {
			return s
		}
	}
	s = &series{labels: renderLabels(v.labels, key[:len(v.labels)])}
	if v.kind == kindHistogram {
		s.counts = make([]atomic.Uint64, len(v.buckets)+1)
	}
	v.series[key] = s
	return s
}

func renderLabels(names, values []string) string {
	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(name)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(values[i]))
		b.WriteByte('"')
	}
	return b.String()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }

// WriteTo writes every metric in the Prometheus text exposition format, version 0.0.4, with
// metrics in the order they were registered and series sorted by their labels.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	families := append([]*Vec(nil), r.families...)
	r.mu.Unlock()
	cw := &countingWriter{w: w}
	bw := bufio.NewWriter(cw)
	for _, v := range families {
		v.write(bw)
	}
	err := bw.Flush()
	return cw.n, err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (v *Vec) write(w *bufio.Writer) {
	v.mu.RLock()
	all := make([]*series, 0, len(v.series))
	for _, s := range v.series {
		all = append(all, s)
	}
	v.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool { return all[i].labels < all[j].labels })

	_, _ = w.WriteString("# HELP " + v.name + " " + v.help + "\n")
	_, _ = w.WriteString("# TYPE " + v.name + " " + v.kind + "\n")
	for _, s := range all {
		if v.kind != kindHistogram {
			writeSample(w, v.name, s.labels, "", math.Float64frombits(s.value.Load()))
			continue
		}
		var cumulative uint64
		for i, bound := range v.buckets {
			cumulative += s.counts[i].Load()
			writeSample(w, v.name+"_bucket", s.labels, `le="`+formatFloat(bound)+`"`, float64(cumulative))
		}
		cumulative += s.counts[len(v.buckets)].Load()
		writeSample(w, v.name+"_bucket", s.labels, `le="+Inf"`, float64(cumulative))
		writeSample(w, v.name+"_sum", s.labels, "", math.Float64frombits(s.value.Load()))
		// The count is the +Inf bucket, so the two always agree within one scrape.
		writeSample(w, v.name+"_count", s.labels, "", float64(cumulative))
	}
}

func writeSample(w *bufio.Writer, name, labels, extra string, value float64) {
	_, _ = w.WriteString(name)
	if labels != "" || extra != "" {
		_ = w.WriteByte('{')
		_, _ = w.WriteString(labels)
		if labels != "" && extra != "" {
			_ = w.WriteByte(',')
		}
		_, _ = w.WriteString(extra)
		_ = w.WriteByte('}')
	}
	_ = w.WriteByte(' ')
	_, _ = w.WriteString(formatFloat(value))
	_ = w.WriteByte('\n')
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
