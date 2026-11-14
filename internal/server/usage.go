package server

import (
	"encoding/json"
	"net/http"
	"time"
)

type wireRule struct {
	Type         string `json:"type"`
	Subject      string `json:"subject"`
	Period       string `json:"period"`
	Limit        *int64 `json:"limit,omitempty"`
	Used         *int64 `json:"used,omitempty"`
	Remaining    *int64 `json:"remaining,omitempty"`
	LimitUSD     string `json:"limit_usd,omitempty"`
	UsedUSD      string `json:"used_usd,omitempty"`
	RemainingUSD string `json:"remaining_usd,omitempty"`
	ResetsAt     string `json:"resets_at"`
}

// usage tells the caller what they have used of each budget and token allowance that applies to
// them. Used counts calls still in flight at what they reserved.
func (g *gateway) usage(w http.ResponseWriter, r *http.Request) {
	c := g.begin(w, r, "usage")
	if c == nil {
		return
	}
	rules := []wireRule{}
	if g.d.Limiter != nil {
		for _, s := range g.d.Limiter.Status(r.Context(), c.caller) {
			rule := wireRule{Type: s.Kind, Subject: s.Subject, Period: s.Window.String(), ResetsAt: s.Reset.Format(time.RFC3339)}
			left := max(s.Limit-s.Used, 0)
			if s.Kind == "budget" {
				rule.LimitUSD, rule.UsedUSD, rule.RemainingUSD = formatUSD(s.Limit), formatUSD(s.Used), formatUSD(left)
			} else {
				limit, used := s.Limit, s.Used
				rule.Limit, rule.Used, rule.Remaining = &limit, &used, &left
			}
			rules = append(rules, rule)
		}
	}
	body, err := json.Marshal(struct {
		Object  string     `json:"object"`
		Subject string     `json:"subject"`
		Limits  []wireRule `json:"limits"`
	}{"usage", c.caller.Subject(), rules})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "", "", "could not encode the usage")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body) //nolint:gosec // JSON we encoded, served as application/json
}
