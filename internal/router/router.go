// Package router picks the provider that serves a requested model.
package router

import (
	"errors"
	"path"

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
}

// New returns a router over routes, first match wins.
func New(routes []Route) *Router {
	return &Router{routes: routes}
}

// Resolve returns the first target of the first route matching model.
func (r *Router) Resolve(model string) (Resolved, error) {
	for _, rt := range r.routes {
		if len(rt.Targets) == 0 || !matches(rt.Match, model) {
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

func matches(pattern, model string) bool {
	if pattern == model {
		return true
	}
	ok, err := path.Match(pattern, model)
	return err == nil && ok
}
