package limits

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxAmount caps a reservation, so a request asking for an absurd max_tokens is refused by the
// limits rather than overflowing them.
const maxAmount = int64(1) << 40

// Caller is who a call is counted against.
type Caller struct {
	User  string
	Teams []string
	Key   string
}

// subject is the caller's own counter: the user when there is one, otherwise the key.
func (c Caller) subject() string {
	if c.User != "" {
		return "user:" + c.User
	}
	return "key:" + c.Key
}

// DefaultOutputReservation is reserved for the output of a call that sets no output limit, when
// no profile cap applies either.
const DefaultOutputReservation = 4096

// Estimate is what a call is expected to use, reserved before it runs.
type Estimate struct {
	InputTokens  int64
	OutputTokens int64 // the output limit the call asks for, per choice; zero when it sets none
	Choices      int64 // completions asked for; zero means one
	NoOutput     bool  // embeddings and other calls that produce no tokens
	CostNUSD     int64
	// Price, when set, prices the estimate once the output cap is applied, and replaces CostNUSD.
	Price func(inputTokens, outputTokens int64) int64
}

// Denied says why a call may not run.
type Denied struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
	Subject    string
	Reset      time.Time
}

// Limiter decides whether a call may run and keeps the counts.
type Limiter struct {
	store    Store
	cfg      Config
	profiles map[string]Profile
	now      func() time.Time
}

// New returns a limiter for cfg.
func New(cfg Config, store Store, now func() time.Time) *Limiter {
	l := &Limiter{store: store, cfg: cfg, profiles: map[string]Profile{}, now: now}
	for _, p := range cfg.Profiles {
		l.profiles[p.Name] = p
	}
	return l
}

// Profile returns the caller's profile: a binding on the key, then the user, then a team,
// then the default.
func (l *Limiter) Profile(c Caller) (Profile, bool) {
	find := func(subject string) (Profile, bool) {
		for _, b := range l.cfg.Bindings {
			if b.Subject == subject {
				p, ok := l.profiles[b.Profile]
				return p, ok
			}
		}
		return Profile{}, false
	}
	if c.Key != "" {
		if p, ok := find("key:" + c.Key); ok {
			return p, true
		}
	}
	if c.User != "" {
		if p, ok := find("user:" + c.User); ok {
			return p, true
		}
	}
	for _, t := range c.Teams {
		if p, ok := find("team:" + t); ok {
			return p, true
		}
	}
	p, ok := l.profiles[l.cfg.DefaultProfile]
	return p, ok
}

type reservation struct {
	key    string
	window Window
	amount int64
	usd    bool
}

type rateTaken struct {
	name   string
	key    string
	rate   Rate
	amount int64
	bucket Bucket
	settle bool
}

// Ticket is an admitted call. Settle it with the real usage, and always Release it.
type Ticket struct {
	l            *Limiter
	at           time.Time
	release      func()
	allowances   []reservation
	rates        []rateTaken
	maxOut       int64
	maxReasoning int64
	mu           sync.Mutex
	settled      bool
}

// Admit reserves the estimate against every limit that applies, or says why the call may not
// run. A denied call keeps nothing reserved.
func (l *Limiter) Admit(ctx context.Context, c Caller, est Estimate) (*Ticket, *Denied) {
	now := l.now()
	t := &Ticket{l: l, at: now, release: func() {}}
	p, hasProfile := l.Profile(c)
	subj := c.subject()
	est.InputTokens = clamp(est.InputTokens)
	est.OutputTokens = clamp(est.OutputTokens)
	est.CostNUSD = clamp(est.CostNUSD)

	if hasProfile {
		t.maxOut, t.maxReasoning = p.MaxOutputTokens, p.MaxReasoningTokens
		if p.MaxInputTokens > 0 && est.InputTokens > p.MaxInputTokens {
			return nil, &Denied{Status: http.StatusBadRequest, Code: "input_too_large", Subject: subj,
				Message: fmt.Sprintf("the prompt is about %d tokens, over the limit of %d", est.InputTokens, p.MaxInputTokens)}
		}
		if p.MaxOutputTokens > 0 && (est.OutputTokens == 0 || est.OutputTokens > p.MaxOutputTokens) {
			est.OutputTokens = p.MaxOutputTokens
		}
	}
	if est.OutputTokens == 0 && !est.NoOutput {
		est.OutputTokens = DefaultOutputReservation
	}
	if n := max(est.Choices, 1); est.OutputTokens > maxAmount/n {
		est.OutputTokens = maxAmount
	} else {
		est.OutputTokens *= n
	}
	if est.Price != nil {
		est.CostNUSD = clamp(est.Price(est.InputTokens, est.OutputTokens))
	}
	if hasProfile {
		if p.Concurrency > 0 {
			ok, release, _ := l.store.Acquire(ctx, "conc|"+subj, p.Concurrency)
			if !ok {
				return nil, &Denied{Status: http.StatusTooManyRequests, Code: "concurrency_limited", Subject: subj,
					Message: fmt.Sprintf("%s already has %d requests running; retry when one ends", subj, p.Concurrency)}
			}
			t.release = release
		}
		if p.RequestRate != nil {
			if d := t.take(ctx, "requests", "req|"+subj, 1, *p.RequestRate, false, subj); d != nil {
				return nil, d
			}
		}
	}
	tokens := clamp(est.InputTokens + est.OutputTokens)
	checks := l.checks(c, p, hasProfile)
	for _, ch := range checks {
		amount := tokens
		if ch.usd {
			amount = est.CostNUSD
		}
		r, _ := l.store.Add(ctx, ch.key, amount, ch.limit, ch.window, now)
		if !r.OK {
			t.Release()
			if ch.usd {
				return nil, &Denied{Status: http.StatusPaymentRequired, Code: "budget_exceeded", Subject: ch.subject, Reset: r.Reset,
					Message: fmt.Sprintf("the budget for %s is spent: %.2f USD %s, resets %s", ch.subject, float64(ch.limit)/1e9, ch.window, r.Reset.Format(time.RFC3339))}
			}
			return nil, &Denied{Status: http.StatusPaymentRequired, Code: "quota_exceeded", Subject: ch.subject, Reset: r.Reset,
				Message: fmt.Sprintf("the %s token allowance of %d for %s is spent, resets %s", ch.window, ch.limit, ch.subject, r.Reset.Format(time.RFC3339))}
		}
		t.allowances = append(t.allowances, reservation{ch.key, ch.window, amount, ch.usd})
	}
	if hasProfile && p.TokenRate != nil {
		if d := t.take(ctx, "tokens", "tokr|"+subj, min(tokens, p.TokenRate.N), *p.TokenRate, true, subj); d != nil {
			return nil, d
		}
	}
	return t, nil
}

func (t *Ticket) take(ctx context.Context, name, key string, amount int64, r Rate, settle bool, subj string) *Denied {
	b, _ := t.l.store.Take(ctx, key, amount, r, t.at)
	if !b.OK {
		t.Release()
		return &Denied{Status: http.StatusTooManyRequests, Code: "rate_limited", Subject: subj, RetryAfter: b.RetryAfter,
			Message: fmt.Sprintf("%s is over its %s rate of %s; retry in %s", subj, name, r, b.RetryAfter.Round(time.Second))}
	}
	t.rates = append(t.rates, rateTaken{name, key, r, amount, b, settle})
	return nil
}

func (l *Limiter) budgetsFor(c Caller) []Budget {
	var out []Budget
	exactUser := map[Window]bool{}
	for _, b := range l.cfg.Budgets {
		if c.User != "" && b.Subject == "user:"+c.User {
			exactUser[b.Window] = true
		}
	}
	for _, b := range l.cfg.Budgets {
		switch {
		case b.Subject == "user:*":
			// An exact user rule replaces user:* for its own period only, so raising one user's
			// monthly budget does not drop their daily one.
			if c.User != "" && !exactUser[b.Window] {
				out = append(out, b)
			}
		case c.User != "" && b.Subject == "user:"+c.User,
			c.Key != "" && b.Subject == "key:"+c.Key:
			out = append(out, b)
		default:
			for _, team := range c.Teams {
				if b.Subject == "team:"+team {
					out = append(out, b)
				}
			}
		}
	}
	return out
}

func clamp(n int64) int64 { return min(max(n, 0), maxAmount) }

// Caps returns the output and reasoning token caps of the caller's profile, zero when none or
// when there is no ticket.
func (t *Ticket) Caps() (maxOutput, maxReasoning int64) {
	if t == nil {
		return 0, 0
	}
	return t.maxOut, t.maxReasoning
}

// Settle replaces the reservations with what the call used. Only the first call counts.
func (t *Ticket) Settle(tokens, costNUSD int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.settled {
		return
	}
	t.settled = true
	ctx := context.Background()
	tokens, costNUSD = clamp(tokens), clamp(costNUSD)
	for _, a := range t.allowances {
		used := tokens
		if a.usd {
			used = costNUSD
		}
		_ = t.l.store.Adjust(ctx, a.key, used-a.amount, a.window, t.at)
	}
	for _, r := range t.rates {
		if r.settle {
			_ = t.l.store.Settle(ctx, r.key, tokens-r.amount, r.rate, t.l.now())
		}
	}
}

// Keep marks the reservation as spent as it stands, for a call whose provider did the work but
// whose real usage is unknown, such as one the caller abandoned before the answer.
func (t *Ticket) Keep() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.settled = true
}

// Release frees the concurrency slot and gives back any reservation that was not settled. It is
// safe to call more than once and after Settle.
func (t *Ticket) Release() {
	t.Settle(0, 0)
	t.release()
}

// SetHeaders adds the IETF RateLimit-Policy and RateLimit headers for the rates the call was
// counted against.
func (t *Ticket) SetHeaders(h http.Header) {
	var policies, states []string
	for _, r := range t.rates {
		policies = append(policies, fmt.Sprintf("%q;q=%d;w=%d", r.name, r.rate.N, int64(r.rate.Per/time.Second)))
		states = append(states, fmt.Sprintf("%q;r=%d;t=%d", r.name, r.bucket.Remaining, int64(math.Ceil(r.bucket.Reset.Seconds()))))
	}
	if len(policies) > 0 {
		h.Set("RateLimit-Policy", strings.Join(policies, ", "))
		h.Set("RateLimit", strings.Join(states, ", "))
	}
}

// Seed adds usage recorded before a restart to the counters of the windows that are still
// open, so a restart does not hand out a fresh allowance.
func (l *Limiter) Seed(ctx context.Context, c Caller, at time.Time, tokens, costNUSD int64) {
	now := l.now()
	keys := []string{"usd|" + c.subject()}
	if c.Key != "" && c.User != "" {
		keys = append(keys, "usd|key:"+c.Key)
	}
	for _, team := range c.Teams {
		keys = append(keys, "usd|team:"+team)
	}
	for _, w := range []Window{Day, Month} {
		if !w.Start(at).Equal(w.Start(now)) {
			continue
		}
		_ = l.store.Adjust(ctx, "tok|"+c.subject(), clamp(tokens), w, at)
		for _, k := range keys {
			_ = l.store.Adjust(ctx, k, clamp(costNUSD), w, at)
		}
	}
}

// check is one counter a call is held against.
type check struct {
	key     string
	window  Window
	limit   int64
	usd     bool
	subject string
}

// checks lists the token allowances and budgets that apply to c. A profile's llm.cost_usd and a
// budget on the same subject and period share a counter, and the lower limit holds.
func (l *Limiter) checks(c Caller, p Profile, hasProfile bool) []check {
	subj := c.subject()
	var out []check
	add := func(key string, w Window, limit int64, usd bool, subject string) {
		if limit <= 0 {
			return
		}
		for i, ch := range out {
			if ch.key == key && ch.window == w {
				if limit < ch.limit {
					out[i].limit, out[i].subject = limit, subject
				}
				return
			}
		}
		out = append(out, check{key, w, limit, usd, subject})
	}
	if hasProfile {
		add("tok|"+subj, Day, p.TokensPerDay, false, subj)
		add("tok|"+subj, Month, p.TokensPerMonth, false, subj)
		add("usd|"+subj, Day, p.USDPerDay, true, subj)
		add("usd|"+subj, Month, p.USDPerMonth, true, subj)
	}
	for _, b := range l.budgetsFor(c) {
		subject := b.Subject
		if subject == "user:*" {
			subject = "user:" + c.User
		}
		add("usd|"+subject, b.Window, b.LimitNUSD, true, subject)
	}
	return out
}

// Subject names the caller's own counter: user:<id>, or key:<name> for a caller with no user.
func (c Caller) Subject() string { return c.subject() }

// RuleStatus is how much of one allowance or budget is used. Limit and Used are tokens for
// Kind "tokens" and nano-USD for Kind "budget". Used includes calls still in flight.
type RuleStatus struct {
	Kind    string
	Subject string
	Window  Window
	Limit   int64
	Used    int64
	Reset   time.Time
}

// Status reports every token allowance and budget that applies to c.
func (l *Limiter) Status(ctx context.Context, c Caller) []RuleStatus {
	now := l.now()
	p, hasProfile := l.Profile(c)
	var out []RuleStatus
	for _, ch := range l.checks(c, p, hasProfile) {
		used, _ := l.store.Used(ctx, ch.key, ch.window, now)
		kind := "tokens"
		if ch.usd {
			kind = "budget"
		}
		out = append(out, RuleStatus{Kind: kind, Subject: ch.subject, Window: ch.window, Limit: ch.limit, Used: used, Reset: ch.window.Reset(now)})
	}
	return out
}
