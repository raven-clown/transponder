package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/schema"
)

// admit reserves the call against the caller's limits once the model is known, and writes the
// refusal itself when the call may not run. The estimate is only worked out when something
// limits the call, since it reads the whole body.
func (g *gateway) admit(c *call, estimate func() limits.Estimate) bool {
	if c.limiter == nil {
		return true
	}
	est := estimate()
	// Any target of the route may end up serving the call, so the reservation is priced at the
	// dearest one. A free first target must not let calls through that a paid fallback would bill.
	est.Price = func(in, out int64) int64 {
		var dearest int64
		for _, t := range c.plan.Targets {
			if t.Free {
				continue
			}
			cost, _ := g.d.Prices.Cost(t.Type, t.Model, schema.Usage{InputTokens: in, OutputTokens: out})
			dearest = max(dearest, cost)
		}
		return dearest
	}
	t, d := c.limiter.Admit(c.r.Context(), c.caller, est)
	if d != nil {
		g.d.Metrics.Limited(d.Code)
		if d.RetryAfter > 0 {
			c.w.Header().Set("Retry-After", strconv.FormatInt(int64(math.Ceil(d.RetryAfter.Seconds())), 10))
		}
		errType := "invalid_request_error"
		switch d.Status {
		case http.StatusTooManyRequests:
			errType = "rate_limit_error"
		case http.StatusPaymentRequired:
			errType = "insufficient_quota"
		case http.StatusServiceUnavailable:
			errType = "server_error"
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
	beforeAnswer := c.rec.Status >= http.StatusInternalServerError || (c.rec.Status == statusCallerGone && !c.cut)
	if c.ticket != nil && c.r.Context().Err() != nil && beforeAnswer {
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
	return maxField(topFields(body), fields...)
}

// topFields is the body's top-level object, or nil when the body is not one. Reading it once
// serves every maxField a call needs.
func topFields(body []byte) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	return m
}

// maxField is requestedMax on fields already read.
func maxField(m map[string]json.RawMessage, fields ...string) int64 {
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

// minThinkingBudget is the smallest extended thinking budget Anthropic accepts.
const minThinkingBudget = 1024
