package pricing

import (
	"testing"

	"github.com/raven-clown/transponder/internal/schema"
)

func TestCost(t *testing.T) {
	tests := []struct {
		name       string
		typ, model string
		u          schema.Usage
		want       int64
		wantPriced bool
	}{
		{"input and output", "openai", "gpt-4o-mini", schema.Usage{InputTokens: 1000, OutputTokens: 500}, 450_000, true},
		{"cache read", "openai", "gpt-5", schema.Usage{InputTokens: 1000, CacheReadTokens: 200, OutputTokens: 100}, 2_275_000, true},
		{"embeddings", "openai", "text-embedding-3-small", schema.Usage{InputTokens: 1_000_000}, 20_000_000, true},
		{"types have no free pass here", "ollama", "anything", schema.Usage{InputTokens: 10, OutputTokens: 10}, 0, false},
		{"unpriced", "openai", "unknown", schema.Usage{InputTokens: 10}, 0, false},
		{"gemini", "gemini", "gemini-2.5-flash", schema.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}, 2_800_000_000, true},
		{"reasoning at the output price", "openai", "gpt-4o-mini", schema.Usage{OutputTokens: 500, ReasoningTokens: 500}, 600_000, true},
		{"anthropic one-hour cache writes", "anthropic", "claude-sonnet-5-5", schema.Usage{CacheWriteTokens: 1000, CacheWrite1hTokens: 1000}, 4_000_000, true},
		{"anthropic with cache", "anthropic", "claude-sonnet-5-5", schema.Usage{InputTokens: 1000, CacheReadTokens: 1000, CacheWriteTokens: 1000, OutputTokens: 100}, 5_600_000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, priced := Default().Cost(tt.typ, tt.model, tt.u)
			if got != tt.want || priced != tt.wantPriced {
				t.Fatalf("Cost = %d, %v; want %d, %v", got, priced, tt.want, tt.wantPriced)
			}
		})
	}
}

func TestCostRounds(t *testing.T) {
	tb, err := Parse([]byte(`openai/half: {input: "0.0005"}
openai/less: {input: "0.0004"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := tb.Cost("openai", "half", schema.Usage{InputTokens: 1}); got != 1 {
		t.Fatalf("half a nano-USD = %d, want 1", got)
	}
	if got, _ := tb.Cost("openai", "less", schema.Usage{InputTokens: 1}); got != 0 {
		t.Fatalf("0.4 nano-USD = %d, want 0", got)
	}
}

func TestParseRejects(t *testing.T) {
	for name, body := range map[string]string{
		"negative":      "openai/m:\n  input: \"-1\"\n",
		"not a number":  "openai/m:\n  input: \"cheap\"\n",
		"unknown field": "openai/m:\n  inptu: \"1\"\n",
		"no type":       "m:\n  input: \"1\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(body)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestDefaultLoads(t *testing.T) {
	for _, m := range []string{
		"gpt-5", "gpt-5-mini", "gpt-5-nano", "gpt-5.1", "gpt-5.2", "gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano", "gpt-5.5",
		"gpt-4.1", "gpt-4.1-mini", "gpt-4o", "gpt-4o-mini", "text-embedding-3-small", "text-embedding-3-large",
	} {
		if _, priced := Default().Cost("openai", m, schema.Usage{}); !priced {
			t.Errorf("openai/%s has no price", m)
		}
	}
}
