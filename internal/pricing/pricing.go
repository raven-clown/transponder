// Package pricing turns token counts into USD cost.
package pricing

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/raven-clown/transponder/internal/schema"
)

//go:embed prices.yaml
var embedded []byte

// Price is nano-USD per million tokens.
type Price struct {
	Input, Output, CacheRead, CacheWrite, CacheWrite1h int64
}

// Table maps "type/model" to a price.
type Table struct {
	prices map[string]Price
}

var defaultTable = mustParse(embedded)

// Default returns the price table built into the binary.
func Default() *Table { return defaultTable }

func mustParse(b []byte) *Table {
	t, err := Parse(b)
	if err != nil {
		panic(err)
	}
	return t
}

type entry struct {
	Input        string `yaml:"input"`
	Output       string `yaml:"output"`
	CacheRead    string `yaml:"cache_read"`
	CacheWrite   string `yaml:"cache_write"`
	CacheWrite1h string `yaml:"cache_write_1h"`
}

// Parse reads a price file of USD strings per million tokens.
func Parse(data []byte) (*Table, error) {
	raw := map[string]entry{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("pricing: %w", err)
	}
	t := &Table{prices: make(map[string]Price, len(raw))}
	for key, e := range raw {
		if !strings.Contains(key, "/") {
			return nil, fmt.Errorf("pricing: key %q must be type/model", key)
		}
		var p Price
		for _, f := range []struct {
			s   string
			dst *int64
		}{{e.Input, &p.Input}, {e.Output, &p.Output}, {e.CacheRead, &p.CacheRead}, {e.CacheWrite, &p.CacheWrite}, {e.CacheWrite1h, &p.CacheWrite1h}} {
			if f.s == "" {
				continue
			}
			usd, err := strconv.ParseFloat(f.s, 64)
			if err != nil || usd < 0 || math.IsInf(usd, 0) || math.IsNaN(usd) {
				return nil, fmt.Errorf("pricing: %s: %q is not a price", key, f.s)
			}
			*f.dst = int64(math.Round(usd * 1e9))
		}
		t.prices[key] = p
	}
	return t, nil
}

// Cost returns the cost of u in nano-USD. priced is false when the model has no entry.
func (t *Table) Cost(typ, model string, u schema.Usage) (nusd int64, priced bool) {
	p, ok := t.prices[typ+"/"+model]
	if !ok {
		return 0, false
	}
	oneHour := p.CacheWrite1h
	if oneHour == 0 {
		oneHour = p.CacheWrite
	}
	sum := u.InputTokens*p.Input + u.OutputTokens*p.Output + u.CacheReadTokens*p.CacheRead +
		(u.CacheWriteTokens-u.CacheWrite1hTokens)*p.CacheWrite + u.CacheWrite1hTokens*oneHour
	return (sum + 500_000) / 1_000_000, true
}
