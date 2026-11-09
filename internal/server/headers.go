package server

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/raven-clown/transponder/internal/schema"
)

func formatUSD(nusd int64) string {
	return fmt.Sprintf("%d.%06d", nusd/1_000_000_000, (nusd%1_000_000_000)/1_000)
}

func setUsageHeaders(h http.Header, u schema.Usage, costNUSD int64) {
	h.Set("X-Usage-Input-Tokens", strconv.FormatInt(u.InputTokens, 10))
	h.Set("X-Usage-Output-Tokens", strconv.FormatInt(u.OutputTokens, 10))
	h.Set("X-Usage-Reasoning-Tokens", strconv.FormatInt(u.ReasoningTokens, 10))
	h.Set("X-Usage-Cost-USD", formatUSD(costNUSD))
}
