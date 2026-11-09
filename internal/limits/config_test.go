package limits

import (
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func parseYAML(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseProfile(t *testing.T) {
	p, err := ParseProfile("free", parseYAML(t, `
requests: {rate: 60/m, concurrency: 4}
llm.tokens: {rate: 20000/m, per_day: 100000, per_month: 2000000}
llm.cost_usd: {per_month: 5}
llm.max_input_tokens: 8192
llm.max_output_tokens: 2048
llm.max_reasoning_tokens: 4096
tools.calls: {rate: 30/m}
agent.runs: {rate: 10/m}
memory.items: 1000
`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "free" || p.RequestRate == nil || *p.RequestRate != (Rate{60, time.Minute}) || p.Concurrency != 4 ||
		p.TokenRate == nil || p.TokensPerDay != 100000 || p.TokensPerMonth != 2000000 ||
		p.USDPerMonth != 5_000_000_000 || p.USDPerDay != 0 ||
		p.MaxInputTokens != 8192 || p.MaxOutputTokens != 2048 || p.MaxReasoningTokens != 4096 {
		t.Fatalf("profile %+v", p)
	}
}

func TestParseProfileRejects(t *testing.T) {
	for name, body := range map[string]string{
		"unknown llm key":     "llm.temperature: 1",
		"unknown key":         "speed: 3",
		"bad rate":            "requests: {rate: fast}",
		"unknown field":       "requests: {burst: 3}",
		"negative":            "llm.max_input_tokens: -1",
		"not a number":        "llm.tokens: {per_day: lots}",
		"not a map":           "requests: 5",
		"negative cost":       "llm.cost_usd: {per_day: -2}",
		"zero concurrency ok": "requests: {concurrency: 0}",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseProfile("p", parseYAML(t, body))
			if name == "zero concurrency ok" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), `profile "p"`) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestParseBudget(t *testing.T) {
	b, err := ParseBudget("team:platform", "daily", 50)
	if err != nil || b.Window != Day || b.LimitNUSD != 50_000_000_000 {
		t.Fatalf("budget %+v %v", b, err)
	}
	for _, bad := range [][2]string{{"group:x", "daily"}, {"user:", "daily"}, {"user:a", "weekly"}, {"team:*", "daily"}} {
		if _, err := ParseBudget(bad[0], bad[1], 1); err == nil {
			t.Fatalf("budget %v accepted", bad)
		}
	}
	if _, err := ParseBudget("user:*", "monthly", -1); err == nil {
		t.Fatal("negative budget accepted")
	}
}
