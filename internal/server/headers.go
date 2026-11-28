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

// formatUSD writes nano-USD as dollars with six decimals, such as 0.002500.
func formatUSD(nusd int64) string {
	if nusd < 0 {
		// Never a real cost; kept as it always read.
		return fmt.Sprintf("%d.%06d", nusd/1_000_000_000, (nusd%1_000_000_000)/1_000)
	}
	var buf [32]byte
	b := strconv.AppendInt(buf[:0], nusd/1_000_000_000, 10)
	b = append(b, '.')
	micro := (nusd % 1_000_000_000) / 1_000
	for div := int64(100_000); div > 0; div /= 10 {
		b = append(b, byte('0'+micro/div%10))
	}
	return string(b)
}

// setUsageHeaders writes the usage headers by their names in Go's canonical form, which skips
// the check Header.Set makes on every call. X-Usage-Cost-USD is X-Usage-Cost-Usd in that form.
func setUsageHeaders(h http.Header, u schema.Usage, costNUSD int64) {
	h["X-Usage-Input-Tokens"] = []string{strconv.FormatInt(u.InputTokens, 10)}
	h["X-Usage-Output-Tokens"] = []string{strconv.FormatInt(u.OutputTokens, 10)}
	h["X-Usage-Reasoning-Tokens"] = []string{strconv.FormatInt(u.ReasoningTokens, 10)}
	h["X-Usage-Cost-Usd"] = []string{formatUSD(costNUSD)}
}
