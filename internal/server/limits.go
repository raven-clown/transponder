package server

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/tokens"
)

// admit reserves the call against the caller's limits once the model is known, and writes the
// refusal itself when the call may not run.
func (g *gateway) admit(c *call, est limits.Estimate) bool {
	if g.d.Limiter == nil {
		return true
	}
	if !c.resolved.Free {
		est.Price = func(in, out int64) int64 {
			cost, _ := g.d.Prices.Cost(c.resolved.Type, c.resolved.Model, schema.Usage{InputTokens: in, OutputTokens: out})
			return cost
		}
	}
	t, d := g.d.Limiter.Admit(c.r.Context(), c.caller, est)
	if d != nil {
		if d.RetryAfter > 0 {
			c.w.Header().Set("Retry-After", strconv.FormatInt(int64(math.Ceil(d.RetryAfter.Seconds())), 10))
		}
		errType := "invalid_request_error"
		switch d.Status {
		case http.StatusTooManyRequests:
			errType = "rate_limit_error"
		case http.StatusPaymentRequired:
			errType = "insufficient_quota"
		}
		c.fail(c.w, d.Status, errType, d.Code, "", d.Message)
		return false
	}
	c.ticket = t
	t.SetHeaders(c.w.Header())
	return true
}

// done gives back whatever the call reserved and did not settle, and frees its slot.
func (c *call) done() {
	if c.ticket != nil {
		c.ticket.Release()
	}
}

// recordCall settles the call's reservations to its real usage and stores the usage record.
func (g *gateway) recordCall(ctx context.Context, c *call) {
	if c.ticket != nil && c.r.Context().Err() != nil && c.rec.Status >= http.StatusInternalServerError {
		// The caller left before the answer; the provider may still have done and billed the work.
		c.ticket.Keep()
	}
	if c.ticket != nil {
		r := c.rec
		c.ticket.Settle(r.InputTokens+r.CacheReadTokens+r.CacheWriteTokens+r.OutputTokens+r.ReasoningTokens, r.CostNUSD)
	}
	g.record(ctx, c.rec)
}

// requestedMax reads the largest number among fields, since providers disagree on which of
// max_tokens and max_completion_tokens wins.
func requestedMax(body []byte, fields ...string) int64 {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return 0
	}
	// Keys are matched without regard to case, as servers written in Go read them.
	var out int64
	for k, raw := range m {
		for _, f := range fields {
			var n int64
			if strings.EqualFold(k, f) && json.Unmarshal(raw, &n) == nil {
				out = max(out, n)
			}
		}
	}
	return out
}

// capField lowers a numeric field of the JSON object to limit, setting it when missing if
// setMissing. Bodies that are not objects come back unchanged.
func capField(m map[string]json.RawMessage, field string, limit int64, setMissing bool) {
	// Servers written in Go read JSON keys without regard to case, so a Max_Tokens next to
	// max_tokens could outrank the cap there.
	for k := range m {
		if k != field && strings.EqualFold(k, field) {
			delete(m, k)
		}
	}
	raw, ok := m[field]
	var n int64
	if ok && json.Unmarshal(raw, &n) == nil && n <= limit {
		return
	}
	if !ok && !setMissing {
		return
	}
	m[field] = json.RawMessage(strconv.FormatInt(limit, 10))
}

func editJSON(body []byte, edit func(map[string]json.RawMessage)) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return body
	}
	edit(m)
	b, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return b
}

// capOpenAI lowers the output limit of an OpenAI request to the caller's cap. OpenAI's own API
// takes max_completion_tokens; other servers max_tokens.
func capOpenAI(body []byte, providerType string, limit int64) []byte {
	if limit <= 0 {
		return body
	}
	return editJSON(body, func(m map[string]json.RawMessage) {
		_, hasCompletion := m["max_completion_tokens"]
		_, hasMax := m["max_tokens"]
		switch {
		case hasCompletion:
			capField(m, "max_completion_tokens", limit, true)
			capField(m, "max_tokens", limit, false)
		case hasMax || providerType != "openai":
			capField(m, "max_tokens", limit, true)
			capField(m, "max_completion_tokens", limit, false)
		default:
			capField(m, "max_completion_tokens", limit, true)
			capField(m, "max_tokens", limit, false)
		}
	})
}

// capAnthropic lowers max_tokens and the extended thinking budget of a Messages request.
func capAnthropic(body []byte, outLimit, reasoningLimit int64) []byte {
	if outLimit <= 0 && reasoningLimit <= 0 {
		return body
	}
	return editJSON(body, func(m map[string]json.RawMessage) {
		if outLimit > 0 {
			capField(m, "max_tokens", outLimit, true)
		}
		raw, ok := m["thinking"]
		if !ok {
			return
		}
		var maxTokens int64
		_ = json.Unmarshal(m["max_tokens"], &maxTokens)
		drop := false
		m["thinking"] = editJSON(raw, func(t map[string]json.RawMessage) {
			raw, has := t["budget_tokens"]
			if !has {
				return
			}
			var budget int64
			if json.Unmarshal(raw, &budget) != nil {
				drop = reasoningLimit > 0
				return
			}
			// Anthropic wants the thinking budget under max_tokens and at least 1024.
			limit := budget
			if reasoningLimit > 0 {
				limit = min(limit, reasoningLimit)
			}
			if maxTokens > 0 {
				limit = min(limit, maxTokens-1)
			}
			if limit < minThinkingBudget {
				drop = true
				return
			}
			capField(t, "budget_tokens", limit, false)
		})
		if drop {
			delete(m, "thinking")
		}
	})
}

// base64DataURI matches an inline image, audio or file payload, which is not text for the model.
var base64DataURI = regexp.MustCompile(`^data:[a-z]+/[a-zA-Z0-9.+-]+;base64,`)

// minThinkingBudget is the smallest extended thinking budget Anthropic accepts.
const minThinkingBudget = 1024

// estimateInput counts every key and string in a request body, and every number in an array,
// which is how token-array inputs arrive. Tool schemas, tool arguments and descriptions all reach
// the model, so they all count. Base64 payloads and signatures do not.
func estimateInput(body []byte) int64 {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return tokens.Estimate(string(body))
	}
	var b bytes.Buffer
	var numbers int64
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				// Only a base64 source's data is binary; a text document keeps its text there.
				if k == "data" && x["type"] == "base64" {
					continue
				}
				b.WriteString(k)
				b.WriteByte(' ')
				walk(k, val)
			}
		case []any:
			for _, val := range x {
				if _, ok := val.(float64); ok {
					numbers++
					continue
				}
				walk(key, val)
			}
		case string:
			if key == "signature" || base64DataURI.MatchString(x) {
				return
			}
			b.WriteString(x)
			b.WriteByte(' ')
		}
	}
	walk("", v)
	return tokens.Estimate(b.String()) + numbers
}
