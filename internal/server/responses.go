package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/sse"
)

// responses serves the OpenAI Responses API as a passthrough. A target that speaks the OpenAI
// dialect gets the body as sent with the model changed and store off; any other target is
// skipped, since nothing translates this API yet.
//
// The gateway keeps no state for this API. Every caller of a route shares the provider account
// behind it, so a response stored there could be continued by anyone who learned its id. Calls
// are therefore never stored, and a call that continues stored state is refused.
func (g *gateway) responses(w http.ResponseWriter, r *http.Request) {
	c := g.begin(w, r, "responses")
	if c == nil {
		return
	}
	defer c.done()
	peek, err := openai.PeekResponses(c.body)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	if peek.Background {
		// The answer and its usage would arrive on GET /v1/responses/{id}, which is not served,
		// so the call would be billed without being counted.
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unsupported", "background", "background responses are not supported: the gateway cannot fetch the result or its usage later")
		return
	}
	refs := openai.ReadResponsesRefs(c.body)
	if field := refs.Stateful(); field != "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unsupported", field,
			"responses stored by the provider cannot be continued through transponder, which stores none: send the whole conversation in input instead of "+statefulName(field))
		return
	}
	c.body = stateless(c.body)
	if !g.resolve(c, peek.Model) {
		return
	}
	if !g.countable(c, refs) {
		return
	}
	if !g.admit(c, func() limits.Estimate {
		return limits.Estimate{InputTokens: estimateInput(c.body), OutputTokens: requestedMax(c.body, "max_output_tokens")}
	}) {
		return
	}
	if out, _ := c.ticket.Caps(); out > 0 {
		c.body = capResponses(c.body, out)
	}
	g.run(c, func() error {
		n, native := nativeTarget(c, "openai")
		switch {
		case !native:
			return failed(provider.CannotCarry(&schema.ContentError{Reason: "the Responses API is only passed through to providers that speak the OpenAI API"}), false)
		case peek.Stream:
			return g.responsesStream(c, n)
		default:
			return g.responsesPlain(c, n)
		}
	})
}

func statefulName(field string) string {
	if field == "input" {
		return "item_reference items"
	}
	return field
}

// countable refuses content whose size the gateway cannot read before the call when the caller
// has llm.max_input_tokens, since that limit would not see it. It writes the refusal itself.
func (g *gateway) countable(c *call, refs openai.ResponsesRefs) bool {
	if c.limiter == nil || (!refs.Prompt && refs.Files == 0) {
		return true
	}
	p, ok := c.limiter.Profile(c.caller)
	if !ok || p.MaxInputTokens <= 0 {
		return true
	}
	param, what := "prompt", "a stored prompt"
	if !refs.Prompt {
		param, what = "input", "an input_file part"
	}
	c.fail(c.w, http.StatusBadRequest, "invalid_request_error", "input_not_countable", param,
		fmt.Sprintf("this caller has an input limit of %d tokens, and %s cannot be counted before the call: send its text in input instead", p.MaxInputTokens, what))
	return false
}

// responsesFailed sorts a Responses API call the provider refused. A server without the
// endpoint cannot take the request, so the next target is tried and it does not count as an
// outage.
func responsesFailed(err error) error {
	var pe *provider.Error
	if !errors.As(err, &pe) {
		return failed(err, true)
	}
	switch pe.Status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		reason := fmt.Sprintf("its /responses endpoint answered %d: %s", pe.Status, upstreamMessage(pe.Body))
		return failed(provider.CannotCarry(&schema.ContentError{Reason: reason}), false)
	}
	return failed(err, true)
}

func (g *gateway) responsesPlain(c *call, n provider.Native) error {
	body, err := openai.RewriteRequest(c.body, c.resolved.Model, false)
	if err != nil {
		return badInput(&openai.RequestError{Code: "invalid_json", Message: err.Error()})
	}
	resp, err := n.Native(c.r.Context(), &provider.NativeCall{Path: "/responses", Body: body, Header: c.r.Header})
	if err != nil {
		return responsesFailed(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := readUpstream(resp)
	if err != nil {
		return failed(err, true)
	}
	u, ok := openai.ParseResponseUsage(out)
	if !ok {
		u = schema.Usage{InputTokens: estimateInput(c.body), OutputTokens: estimateTokens(openai.ResponseText(out))}
		c.rec.Estimated = true
	}
	cost := g.price(c.r.Context(), c, u)
	c.rec.Status = resp.StatusCode
	g.recordCall(c.r.Context(), c)

	setUsageHeaders(c.w.Header(), u, cost)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(resp.StatusCode)
	_, _ = c.w.Write(out) //nolint:gosec // the provider's JSON, served as application/json
	return nil
}

// responsesStream copies a Responses API stream frame by frame, event names included, and reads
// the usage from the event that ends the response.
func (g *gateway) responsesStream(c *call, n provider.Native) error {
	body, err := openai.RewriteRequest(c.body, c.resolved.Model, false)
	if err != nil {
		return badInput(&openai.RequestError{Code: "invalid_json", Message: err.Error()})
	}
	ctx, started, cancel := g.attempt(c.r.Context())
	defer cancel()
	resp, err := n.Native(ctx, &provider.NativeCall{Path: "/responses", Body: body, Header: c.r.Header})
	if err != nil {
		return responsesFailed(err)
	}
	defer func() { _ = resp.Body.Close() }()
	next, err := open(clocked(sse.NewReader(resp.Body).Next, started), openai.OpeningResponseEvent, func(ev sse.Event) error {
		if openai.IsResponseStreamError(ev) {
			return fmt.Errorf("the provider began the stream with an error: %s", upstreamMessage(ev.Data))
		}
		return nil
	})
	if err != nil {
		return failed(fmt.Errorf("stream ended before its first output: %w", err), false)
	}

	out := startSSE(c.w)
	var st openai.ResponseStream
	for {
		var ev sse.Event
		if ev, err = next(); err != nil {
			break
		}
		st.Add(ev.Data)
		out.write(sse.Frame(ev.Name, ev.Data))
		if st.Failed() {
			break
		}
	}
	if !st.Done() && g.streamBroke(ctx, c, err) && !st.Failed() {
		out.write(st.ErrorFrame("upstream_error", streamCut))
	}
	u, ok := st.Usage()
	if !ok {
		u = schema.Usage{InputTokens: estimateInput(c.body), OutputTokens: estimateTokens(st.Text())}
		c.rec.Estimated = true
	}
	cost := g.price(ctx, c, u)
	out.finish(u, cost)
	c.rec.Status = streamStatus(c)
	g.recordCall(ctx, c)
	return nil
}

// stateless sets store to false, which OpenAI takes as true when it is missing. Keys that match
// store or background only without regard to case are dropped, since a server written in Go
// would read them as those fields.
func stateless(body []byte) []byte {
	return editJSON(body, func(m map[string]json.RawMessage) {
		for k := range m {
			if strings.EqualFold(k, "store") || (k != "background" && strings.EqualFold(k, "background")) {
				delete(m, k)
			}
		}
		m["store"] = json.RawMessage("false")
	})
}

// capResponses lowers max_output_tokens of a Responses API request to the caller's cap. The
// field covers reasoning tokens too.
func capResponses(body []byte, limit int64) []byte {
	return editJSON(body, func(m map[string]json.RawMessage) {
		capField(m, "max_output_tokens", limit, true)
	})
}
