// Package router picks the provider that serves a requested model.
package router

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/provider"
)

// ErrNoRoute means no route matches the requested model.
var ErrNoRoute = errors.New("router: no route matches the model")

// Target is one provider a route may use. An empty Model keeps the requested model.
type Target struct {
	Provider provider.Provider
	Type     string
	Free     bool
	Model    string
}

// Route sends models matching Match, an exact name or a glob, to its targets in order, for the
// callers Access lets in.
type Route struct {
	Match   string
	Access  Access
	Targets []Target
}

// Resolved is the target chosen for one call.
type Resolved struct {
	Route    string
	Provider provider.Provider
	Type     string
	Free     bool
	Model    string
	// ModelFromCaller is true when Model is whatever name the caller sent: the target sets no
	// model and the route matches by wildcard. Metrics use it to keep such names off labels.
	ModelFromCaller bool
}

// Plan is the targets to try for one call, in order, and who may make the call.
type Plan struct {
	Route   string
	Access  Access
	Targets []Resolved
}

// Defaults for the cooldown of a target that keeps failing.
const (
	DefaultCooldownFailures = 3
	DefaultCooldown         = 30 * time.Second
)

// maxHealth is how many targets the router tracks before it drops the ones not in cooldown.
const maxHealth = 1024

type health struct {
	fails int
	until time.Time
}

// healthTable is the failure record of targets, which routers built by a reload share.
type healthTable struct {
	mu sync.Mutex
	m  map[targetKey]*health
}

// Router holds the routes in priority order and remembers which targets keep failing.
type Router struct {
	routes []Route
	globs  []*regexp.Regexp

	mu       sync.Mutex
	failures int
	cooldown time.Duration
	now      func() time.Time
	health   *healthTable
}

// New returns a router over routes, first match wins. In Match, * stands for any run of
// characters, slashes included, since model names such as hf.co/user/repo have them.
func New(routes []Route) *Router {
	r := &Router{
		routes: routes, globs: make([]*regexp.Regexp, len(routes)),
		failures: DefaultCooldownFailures, cooldown: DefaultCooldown, now: time.Now,
		health: &healthTable{m: map[targetKey]*health{}},
	}
	for i, rt := range routes {
		re := regexp.QuoteMeta(rt.Match)
		re = strings.ReplaceAll(re, `\*`, ".*")
		re = strings.ReplaceAll(re, `\?`, ".")
		r.globs[i] = regexp.MustCompile("^" + re + "$")
	}
	return r
}

// SetCooldown changes how many failures in a row put a target last, and for how long.
func (r *Router) SetCooldown(failures int, d time.Duration, now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures, r.cooldown, r.now = failures, d, now
}

// KeepHealth makes r share prev's record of failing targets, so a reload keeps the cooldowns,
// and failures that calls still running on prev report reach r. Call it before r serves.
func (r *Router) KeepHealth(prev *Router) {
	if prev == nil {
		r.health = &healthTable{m: map[targetKey]*health{}}
		return
	}
	r.health = prev.health
}

func (r *Router) settings() (failures int, cooldown time.Duration, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failures, r.cooldown, r.now()
}

// Plan returns the targets of the first route matching model. Targets in cooldown come last,
// so they are only tried once the others have failed.
func (r *Router) Plan(model string) (Plan, error) {
	for i, rt := range r.routes {
		if len(rt.Targets) == 0 || !r.globs[i].MatchString(model) {
			continue
		}
		p := Plan{Route: rt.Match, Access: rt.Access}
		wildcard := strings.ContainsAny(rt.Match, "*?")
		var cooling []Resolved
		_, _, now := r.settings()
		r.health.mu.Lock()
		for _, t := range rt.Targets {
			up := t.Model
			if up == "" {
				up = model
			}
			res := Resolved{Route: rt.Match, Provider: t.Provider, Type: t.Type, Free: t.Free, Model: up, ModelFromCaller: t.Model == "" && wildcard}
			if h, ok := r.health.m[healthKey(res)]; ok && now.Before(h.until) {
				cooling = append(cooling, res)
				continue
			}
			p.Targets = append(p.Targets, res)
		}
		r.health.mu.Unlock()
		p.Targets = append(p.Targets, cooling...)
		return p, nil
	}
	return Plan{}, ErrNoRoute
}

// Resolve returns the first target of the plan for model.
func (r *Router) Resolve(model string) (Resolved, error) {
	p, err := r.Plan(model)
	if err != nil {
		return Resolved{}, err
	}
	return p.Targets[0], nil
}

// targetKey names a target by provider and upstream model, whichever route uses it.
type targetKey struct{ provider, model string }

func healthKey(t Resolved) targetKey { return targetKey{t.Provider.Name(), t.Model} }

// Report tells the router how an attempt on t went. A success clears its failures. After the
// configured number of failures in a row the target cools down; once the cooldown ends, one
// more failure starts it again.
func (r *Router) Report(t Resolved, ok bool) {
	failures, cooldown, now := r.settings()
	tab := r.health
	tab.mu.Lock()
	defer tab.mu.Unlock()
	key := healthKey(t)
	if ok {
		delete(tab.m, key)
		return
	}
	h := tab.m[key]
	if h == nil {
		// A target that keeps the caller's model has one entry per model name, so the table is
		// swept before it can grow without end. Targets in cooldown are kept.
		if len(tab.m) >= maxHealth {
			for k, e := range tab.m {
				if !now.Before(e.until) {
					delete(tab.m, k)
				}
			}
		}
		h = &health{}
		tab.m[key] = h
	}
	h.fails++
	if h.fails >= failures {
		h.until = now.Add(cooldown)
	}
}

// Models returns the names a caller can ask for: every route match without a wildcard,
// sorted and without duplicates. The models targets send upstream are left out, since a
// caller asking for one of them would not reach that route.
func (r *Router) Models() []string {
	return r.models(func(Access) bool { return true })
}

// ModelsFor returns the names of Models whose route lets id in.
func (r *Router) ModelsFor(id auth.Identity) []string {
	return r.models(func(a Access) bool { return a.Permits(id) })
}

func (r *Router) models(permits func(Access) bool) []string {
	seen := map[string]bool{}
	for _, rt := range r.routes {
		if !strings.ContainsAny(rt.Match, "*?") && permits(rt.Access) {
			seen[rt.Match] = true
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
