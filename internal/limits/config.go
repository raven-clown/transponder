package limits

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Profile is a named set of limits in the shared profile format. Zero values mean no limit.
type Profile struct {
	Name               string
	RequestRate        *Rate
	Concurrency        int64
	TokenRate          *Rate
	TokensPerDay       int64
	TokensPerMonth     int64
	USDPerDay          int64 // nano-USD
	USDPerMonth        int64 // nano-USD
	MaxInputTokens     int64
	MaxOutputTokens    int64
	MaxReasoningTokens int64
}

// otherProjects own these prefixes; their keys are ignored here.
var otherProjects = []string{"tools.", "agent.", "memory."}

// ParseProfile reads one profile's limits. Keys owned by the other mothership projects are
// ignored; any other unknown key is an error.
func ParseProfile(name string, raw map[string]any) (Profile, error) {
	p := Profile{Name: name}
	fail := func(format string, a ...any) (Profile, error) {
		return Profile{}, fmt.Errorf("profile %q: %s", name, fmt.Sprintf(format, a...))
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		v := raw[key]
		var err error
		switch key {
		case "requests":
			err = fields(v, map[string]func(any) error{
				"rate":        rateField(&p.RequestRate),
				"concurrency": intField(&p.Concurrency),
			})
		case "llm.tokens":
			err = fields(v, map[string]func(any) error{
				"rate":      rateField(&p.TokenRate),
				"per_day":   intField(&p.TokensPerDay),
				"per_month": intField(&p.TokensPerMonth),
			})
		case "llm.cost_usd":
			err = fields(v, map[string]func(any) error{
				"per_day":   usdField(&p.USDPerDay),
				"per_month": usdField(&p.USDPerMonth),
			})
		case "llm.max_input_tokens":
			err = intField(&p.MaxInputTokens)(v)
		case "llm.max_output_tokens":
			err = intField(&p.MaxOutputTokens)(v)
		case "llm.max_reasoning_tokens":
			err = intField(&p.MaxReasoningTokens)(v)
		default:
			if ownedElsewhere(key) {
				continue
			}
			return fail("unknown limit %q", key)
		}
		if err != nil {
			return fail("%s: %v", key, err)
		}
	}
	return p, nil
}

func ownedElsewhere(key string) bool {
	for _, prefix := range otherProjects {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func fields(v any, known map[string]func(any) error) error {
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("must be a map of %s", strings.Join(sortedKeys(known), ", "))
	}
	for k, fv := range m {
		set, ok := known[k]
		if !ok {
			return fmt.Errorf("unknown field %q", k)
		}
		if err := set(fv); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func intField(dst *int64) func(any) error {
	return func(v any) error {
		n, ok := v.(int)
		if !ok || n < 0 {
			return fmt.Errorf("%v must be a whole number of zero or more", v)
		}
		*dst = int64(n)
		return nil
	}
}

func rateField(dst **Rate) func(any) error {
	return func(v any) error {
		s, _ := v.(string)
		r, err := ParseRate(s)
		if err != nil {
			return err
		}
		*dst = &r
		return nil
	}
}

func usdField(dst *int64) func(any) error {
	return func(v any) error {
		var f float64
		switch n := v.(type) {
		case int:
			f = float64(n)
		case float64:
			f = n
		default:
			return fmt.Errorf("%v must be an amount in USD", v)
		}
		nusd, err := toNUSD(f)
		if err != nil {
			return err
		}
		*dst = nusd
		return nil
	}
}

func toNUSD(usd float64) (int64, error) {
	if usd < 0 || math.IsNaN(usd) || math.IsInf(usd, 0) || usd > 1e9 {
		return 0, fmt.Errorf("%v must be an amount in USD of zero or more", usd)
	}
	return int64(math.Round(usd * 1e9)), nil
}

// Budget caps what one subject spends in USD per day or month.
type Budget struct {
	Subject   string // user:*, user:<id>, team:<name> or key:<name>
	Window    Window
	LimitNUSD int64
}

// ParseBudget reads one budget rule.
func ParseBudget(subject, period string, limitUSD float64) (Budget, error) {
	kind, id, ok := strings.Cut(subject, ":")
	if !ok || id == "" || (kind != "user" && kind != "team" && kind != "key") || (id == "*" && kind != "user") {
		return Budget{}, fmt.Errorf("budget subject %q must be user:*, user:<id>, team:<name> or key:<name>", subject)
	}
	b := Budget{Subject: subject}
	switch period {
	case "daily":
		b.Window = Day
	case "monthly":
		b.Window = Month
	default:
		return Budget{}, fmt.Errorf("budget for %s: period %q must be daily or monthly", subject, period)
	}
	n, err := toNUSD(limitUSD)
	if err != nil {
		return Budget{}, fmt.Errorf("budget for %s: %w", subject, err)
	}
	b.LimitNUSD = n
	return b, nil
}

// Binding gives a subject (key:, user: or team:) a profile.
type Binding struct {
	Subject, Profile string
}

// Config is everything the limiter enforces.
type Config struct {
	Profiles       []Profile
	DefaultProfile string
	Bindings       []Binding
	Budgets        []Budget
}

// Empty reports whether nothing is limited.
func (c Config) Empty() bool {
	return len(c.Budgets) == 0 && (c.DefaultProfile == "" && len(c.Bindings) == 0)
}
