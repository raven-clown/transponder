package server

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"

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
		est.CostNUSD, _ = g.d.Prices.Cost(c.resolved.Type, c.resolved.Model, schema.Usage{InputTokens: est.InputTokens, OutputTokens: est.OutputTokens})
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
	if c.ticket != nil {
		r := c.rec
		c.ticket.Settle(r.InputTokens+r.CacheReadTokens+r.CacheWriteTokens+r.OutputTokens+r.ReasoningTokens, r.CostNUSD)
	}
	g.record(ctx, c.rec)
}

// requestedMax reads the first of fields that holds a number.
func requestedMax(body []byte, fields ...string) int64 {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return 0
	}
	for _, f := range fields {
		var n int64
		if raw, ok := m[f]; ok && json.Unmarshal(raw, &n) == nil {
			return n
		}
	}
	return 0
}

// capField lowers a numeric field of the JSON object to limit, setting it when missing if
// setMissing. Bodies that are not objects come back unchanged.
func capField(m map[string]json.RawMessage, field string, limit int64, setMissing bool) {
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
		default:
			capField(m, "max_completion_tokens", limit, true)
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
		if raw, ok := m["thinking"]; ok && reasoningLimit > 0 {
			m["thinking"] = editJSON(raw, func(t map[string]json.RawMessage) {
				capField(t, "budget_tokens", reasoningLimit, false)
			})
		}
	})
}

// requestText gathers the text fields of a request body, for estimating its input tokens.
func requestText(body []byte) string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return string(body)
	}
	var b bytes.Buffer
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				if s, ok := val.(string); ok && (k == "text" || k == "content" || k == "system" || k == "input") {
					b.WriteString(s)
					continue
				}
				walk(val)
			}
		case []any:
			for _, val := range x {
				if s, ok := val.(string); ok {
					b.WriteString(s)
					continue
				}
				walk(val)
			}
		}
	}
	walk(v)
	return b.String()
}

func estimateInput(body []byte) int64 { return tokens.Estimate(requestText(body)) }
