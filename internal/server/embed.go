package server

import (
	"net/http"
	"strings"

	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/schema"
)

func (g *gateway) embeddings(w http.ResponseWriter, r *http.Request) {
	c := g.begin(w, r, "embeddings")
	if c == nil {
		return
	}
	defer c.done()
	peek, err := openai.PeekRequest(c.body)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	if !g.resolve(c, peek.Model) {
		return
	}
	if !g.admit(c, limits.Estimate{InputTokens: estimateInput(c.body), NoOutput: true}) {
		return
	}
	g.run(c, func() error {
		if n, ok := nativeTarget(c, "openai"); ok {
			return g.embedNative(c, n)
		}
		return g.embedCanonical(c)
	})
}

func (g *gateway) embedNative(c *call, n provider.Native) error {
	body, err := openai.RewriteRequest(c.body, c.resolved.Model, false)
	if err != nil {
		return badInput(&openai.RequestError{Code: "invalid_json", Message: err.Error()})
	}
	resp, err := n.Native(c.r.Context(), &provider.NativeCall{Path: "/embeddings", Body: body, Header: c.r.Header})
	if err != nil {
		return failed(err, true)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := readUpstream(resp)
	if err != nil {
		return failed(err, true)
	}
	u, ok := openai.ParseUsage(out)
	if !ok {
		u = schema.Usage{InputTokens: estimateTokens(embedText(c.body))}
		c.rec.Estimated = true
	}
	cost := g.price(c.r.Context(), c, u)
	c.rec.Status = resp.StatusCode
	g.recordCall(c.r.Context(), c)

	setUsageHeaders(c.w.Header(), u, cost)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(resp.StatusCode)
	_, _ = c.w.Write(out)
	return nil
}

func (g *gateway) embedCanonical(c *call) error {
	req, err := openai.ParseEmbedRequest(c.body)
	if err != nil {
		return badInput(err)
	}
	req.Model = c.resolved.Model
	resp, err := c.resolved.Provider.Embed(upstreamContext(c), req)
	if err != nil {
		return failed(err, false)
	}
	body, err := openai.EncodeEmbedResponse(resp)
	if err != nil {
		writeError(c.w, http.StatusInternalServerError, "api_error", "", "", "could not encode the response")
		return nil //nolint:nilerr // the 500 above is the answer
	}
	cost := g.price(c.r.Context(), c, resp.Usage)
	c.rec.Status = http.StatusOK
	g.recordCall(c.r.Context(), c)

	setUsageHeaders(c.w.Header(), resp.Usage, cost)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(body) //nolint:gosec // JSON we encoded, served as application/json
	return nil
}

func embedText(body []byte) string {
	req, err := openai.ParseEmbedRequest(body)
	if err != nil {
		return ""
	}
	return strings.Join(req.Input, "")
}
