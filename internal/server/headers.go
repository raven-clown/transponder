package server

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/raven-clown/transponder/internal/schema"
)

// nosniffValue is shared by every response, so setting the header allocates nothing. Header.Add
// appends past its capacity of one, so it never writes into this array.
var nosniffValue = []string{"nosniff"}

// noSniff stops browsers from guessing a content type, so an error body that quotes a caller's
// input is never run as HTML.
func noSniff(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["X-Content-Type-Options"] = nosniffValue
		next.ServeHTTP(w, r)
	})
}

func formatUSD(nusd int64) string {
	return fmt.Sprintf("%d.%06d", nusd/1_000_000_000, (nusd%1_000_000_000)/1_000)
}

func setUsageHeaders(h http.Header, u schema.Usage, costNUSD int64) {
	h.Set("X-Usage-Input-Tokens", strconv.FormatInt(u.InputTokens, 10))
	h.Set("X-Usage-Output-Tokens", strconv.FormatInt(u.OutputTokens, 10))
	h.Set("X-Usage-Reasoning-Tokens", strconv.FormatInt(u.ReasoningTokens, 10))
	h.Set("X-Usage-Cost-USD", formatUSD(costNUSD))
}
