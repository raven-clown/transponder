// Package router picks the provider that serves a requested model.
package router

import (
	"errors"
	"regexp"
	"strings"

	"github.com/raven-clown/transponder/internal/provider"
)

// ErrNoRoute means no route matches the requested model.
var ErrNoRoute = errors.New("router: no route matches the model")

// Target is one provider a route may use. An empty Model keeps the requested model.
type Target struct {
	Provider provider.Provider
	Type     string
	Model    string
}

// Route sends models matching Match, an exact name or a glob, to its targets in order.
type Route struct {
	Match   string
	Targets []Target
}

// Resolved is the target chosen for one call.
type Resolved struct {
	Route    string
	Provider provider.Provider
	Type     string
	Model    string
}

// Router holds the routes in priority order.
type Router struct {
	routes []Route
	globs  []*regexp.Regexp
}

// New returns a router over routes, first match wins. In Match, * stands for any run of
// characters, slashes included, since model names such as hf.co/user/repo have them.
func New(routes []Route) *Router {
	r := &Router{routes: routes, globs: make([]*regexp.Regexp, len(routes))}
	for i, rt := range routes {
		re := regexp.QuoteMeta(rt.Match)
		re = strings.ReplaceAll(re, `\*`, ".*")
		re = strings.ReplaceAll(re, `\?`, ".")
		r.globs[i] = regexp.MustCompile("^" + re + "$")
	}
	return r
}

// Resolve returns the first target of the first route matching model.
func (r *Router) Resolve(model string) (Resolved, error) {
	for i, rt := range r.routes {
		if len(rt.Targets) == 0 || !r.globs[i].MatchString(model) {
			continue
		}
		t := rt.Targets[0]
		up := t.Model
		if up == "" {
			up = model
		}
		return Resolved{Route: rt.Match, Provider: t.Provider, Type: t.Type, Model: up}, nil
	}
	return Resolved{}, ErrNoRoute
}
