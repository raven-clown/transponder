package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/sse"
)

// responses serves the OpenAI Responses API as a passthrough. A target that speaks the OpenAI
// dialect gets the body as sent with only the model changed; any other target is skipped,
// since nothing translates this API yet.
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
	// A server that reads keys without regard to case would take Background for background.
	c.body = dropVariants(c.body, "background")
	if !g.resolve(c, peek.Model) {
		return
	}
	if !g.admit(c, limits.Estimate{InputTokens: estimateInput(c.body), OutputTokens: requestedMax(c.body, "max_output_tokens")}) {
		return
	}
	if out, _ := c.ticket.Caps(); out > 0 {
		c.body = capResponses(c.body, out)
	}
	// held is the answer of a target that does not hold the response this call continues.
	var held error
	g.run(c, func() error {
		n, native := nativeTarget(c, "openai")
		var err error
		switch {
		case !native:
			err = failed(provider.CannotCarry(&schema.ContentError{Reason: "the Responses API is only passed through to providers that speak the OpenAI API"}), false)
		case peek.Stream:
			err = g.responsesStream(c, n)
		default:
			err = g.responsesPlain(c, n)
		}
		var unknown *unknownResponse
		switch {
		case errors.As(err, &unknown):
			held = err
		case held != nil && errors.Is(err, provider.ErrUnsupported):
			// A target that knew the API but not the response says more than one that was skipped.
			return held
		}
		return err
	})
}

// unknownResponse is the answer of a target that does not hold the response named by
// previous_response_id. Another target of the route may hold it, so the call moves on.
type unknownResponse struct {
	req *openai.RequestError
}

func (e *unknownResponse) Error() string { return e.req.Error() }
func (e *unknownResponse) Unwrap() error { return e.req }

// responsesFailed sorts a Responses API call the provider refused. A server without the
// endpoint, and one that does not hold the response the call continues, cannot take the
// request, so the next target is tried and neither counts as an outage.
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
	if msg, ok := openai.UnknownResponse(pe.Body); ok && pe.Status >= 400 && pe.Status < 500 && !pe.Retryable {
		return badInput(&unknownResponse{req: &openai.RequestError{Param: "previous_response_id", Code: "previous_response_not_found", Message: msg}})
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
	rd := sse.NewReader(resp.Body)
	ev, err := rd.Next()
	if err == nil && !started() {
		err = errAttemptTimeout
	}
	if err == nil && openai.IsResponseStreamError(ev) {
		err = fmt.Errorf("the provider began the stream with an error: %s", upstreamMessage(ev.Data))
	}
	if err != nil {
		return failed(fmt.Errorf("stream ended before its first event: %w", err), false)
	}

	out := startSSE(c.w)
	var st openai.ResponseStream
	for {
		st.Add(ev.Data)
		out.write(sse.Frame(ev.Name, ev.Data))
		if st.Failed() {
			break
		}
		if ev, err = rd.Next(); err != nil {
			break
		}
	}
	if !st.Done() {
		if err != nil && !errors.Is(err, io.EOF) {
			g.d.Logger.WarnContext(ctx, "upstream stream broke", "provider", c.rec.Provider, "err", err)
		}
		if !st.Failed() {
			out.write(st.ErrorFrame("upstream_error", streamCut))
		}
		c.cut = true
	}
	u, ok := st.Usage()
	if !ok {
		u = schema.Usage{InputTokens: estimateInput(c.body), OutputTokens: estimateTokens(st.Text())}
		c.rec.Estimated = true
	}
	cost := g.price(ctx, c, u)
	out.finish(u, cost)
	c.rec.Status = http.StatusOK
	g.recordCall(ctx, c)
	return nil
}

// dropVariants removes the keys that differ from field only by case. A body without one comes
// back untouched.
func dropVariants(body []byte, field string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	found := false
	for k := range m {
		if k != field && strings.EqualFold(k, field) {
			delete(m, k)
			found = true
		}
	}
	if !found {
		return body
	}
	b, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return b
}

// capResponses lowers max_output_tokens of a Responses API request to the caller's cap. The
// field covers reasoning tokens too.
func capResponses(body []byte, limit int64) []byte {
	return editJSON(body, func(m map[string]json.RawMessage) {
		capField(m, "max_output_tokens", limit, true)
	})
}
