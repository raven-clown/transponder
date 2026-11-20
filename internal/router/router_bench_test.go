package router_test

import (
	"fmt"
	"testing"

	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/router"
)

// benchRouter has 20 exact routes, a few globs and a catch-all, with two targets each, which is
// more than most configs and so a fair upper bound.
func benchRouter() *router.Router {
	p := echo.New("echo")
	target := func(m string) []router.Target {
		return []router.Target{{Provider: p, Type: "openai", Model: m}, {Provider: p, Type: "anthropic", Model: m + "-fallback"}}
	}
	var routes []router.Route
	for i := range 20 {
		routes = append(routes, router.Route{Match: fmt.Sprintf("team-%d-model", i), Targets: target("gpt-4o-mini")})
	}
	routes = append(routes,
		router.Route{Match: "claude-*", Targets: target("claude-sonnet-4-5")},
		router.Route{Match: "gemini-?.?-*", Targets: target("gemini-2.5-flash")},
		router.Route{Match: "*", Targets: target("gpt-4o")},
	)
	return router.New(routes)
}

func BenchmarkResolve(b *testing.B) {
	r := benchRouter()
	for _, model := range []string{"team-0-model", "team-19-model", "claude-opus-4", "unknown-model"} {
		b.Run(model, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := r.Resolve(model); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkPlan(b *testing.B) {
	r := benchRouter()
	for _, model := range []string{"team-0-model", "team-19-model", "claude-opus-4", "unknown-model"} {
		b.Run(model, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := r.Plan(model); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
