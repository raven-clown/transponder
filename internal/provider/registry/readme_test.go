package registry

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/schema"
)

const readmePath = "../../../README.md"

const providerTableHeader = "| Type | Provider | Dialect | Default base URL | Chat and streaming | Tools | Images | Embeddings | Responses API |"

// recorder answers every call with an empty JSON object and keeps the bodies it was sent, so a
// probe can see what an adapter puts on the wire without a network.
type recorder struct {
	mu     sync.Mutex
	bodies []string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	r.mu.Lock()
	r.bodies = append(r.bodies, string(body))
	r.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader("{}")),
		Request:    req,
	}, nil
}

// sent reports whether a body holding s went out, and forgets what was sent.
func (r *recorder) sent(s string) (found, any bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.bodies {
		if strings.Contains(b, s) {
			found = true
		}
	}
	any = len(r.bodies) > 0
	r.bodies = nil
	return found, any
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

const (
	probeTool  = "probe_weather_tool"
	probeURL   = "https://example.com/probe.png"
	probeImage = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
)

func userMessage(parts ...schema.Part) []schema.Message {
	return []schema.Message{{Role: schema.RoleUser, Content: append(schema.Text("hi"), parts...)}}
}

// capabilities works out a provider type's row of the README table from what its adapter does.
func capabilities(t *testing.T, s Spec) []string {
	t.Helper()
	rec := &recorder{}
	p := s.New(Settings{Name: "p", Client: &http.Client{Transport: rec}})
	ctx := context.Background()

	base := "set `base_url`"
	if b, ok := p.(interface{ BaseURL() string }); !ok {
		base = "built in"
	} else if b.BaseURL() != "" && !s.NeedsBaseURL {
		base = "`" + b.BaseURL() + "`"
	}

	unsupported := func(err error) bool { return errors.Is(err, provider.ErrUnsupported) }
	_, chatErr := p.Chat(ctx, &schema.ChatRequest{Model: "m", Messages: userMessage()})
	stream, streamErr := p.ChatStream(ctx, &schema.ChatRequest{Model: "m", Messages: userMessage(), Stream: true})
	if stream != nil {
		_ = stream.Close()
	}
	chat := !unsupported(chatErr) && !unsupported(streamErr)
	rec.sent("")

	_, _ = p.Chat(ctx, &schema.ChatRequest{Model: "m", Messages: userMessage(), Tools: []schema.Tool{{
		Name: probeTool, Parameters: json.RawMessage(`{"type":"object"}`),
	}}})
	tools, _ := rec.sent(probeTool)

	_, urlErr := p.Chat(ctx, &schema.ChatRequest{Model: "m", Messages: userMessage(schema.Part{Type: schema.PartImage, Image: &schema.Image{URL: probeURL}})})
	byURL, calledURL := rec.sent(probeURL)
	byURL = byURL && !unsupported(urlErr)
	_, _ = p.Chat(ctx, &schema.ChatRequest{Model: "m", Messages: userMessage(schema.Part{Type: schema.PartImage, Image: &schema.Image{MediaType: "image/png", Data: probeImage}})})
	inline, calledInline := rec.sent(probeImage)
	images := "no"
	switch {
	case byURL && inline:
		images = "URL and inline"
	case inline:
		images = "inline only"
	case !calledURL && !calledInline:
		images = "ignored"
	}

	_, embedErr := p.Embed(ctx, &schema.EmbedRequest{Model: "m", Input: []string{"hi"}})
	_, native := p.(provider.Native)

	return []string{base, yesNo(chat), yesNo(tools), images, yesNo(!unsupported(embedErr)), yesNo(native && p.Dialect() == "openai")}
}

// TestReadmeProviderTable keeps the README's provider table in step with the registry: every
// type is listed once, and its dialect, default base URL and what it supports are what the
// adapter does.
func TestReadmeProviderTable(t *testing.T) {
	f, err := os.Open(filepath.Clean(readmePath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows := map[string][]string{}
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == providerTableHeader:
			in = true
		case !strings.HasPrefix(line, "|"):
			in = false
		case in && !strings.HasPrefix(line, "|---"):
			cells := strings.Split(strings.Trim(line, "|"), "|")
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			typ := strings.Trim(cells[0], "`")
			if _, dup := rows[typ]; dup {
				t.Errorf("README lists %s twice", typ)
			}
			rows[typ] = cells
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatalf("README has no table with the header %s", providerTableHeader)
	}
	for typ := range rows {
		if _, ok := Lookup(typ); !ok {
			t.Errorf("README lists %s, which is not a registered provider type", typ)
		}
	}
	columns := strings.Split(strings.Trim(providerTableHeader, "| "), " | ")
	for _, typ := range Types() {
		t.Run(typ, func(t *testing.T) {
			cells, ok := rows[typ]
			if !ok {
				t.Fatalf("README's provider table has no row for %s", typ)
			}
			if len(cells) != len(columns) {
				t.Fatalf("row has %d cells, want %d", len(cells), len(columns))
			}
			spec, _ := Lookup(typ)
			p := spec.New(Settings{Name: "p", Client: http.DefaultClient})
			dialect := "`" + p.Dialect() + "`"
			if p.Dialect() == "none" {
				dialect = "none"
			}
			want := append([]string{dialect}, capabilities(t, spec)...)
			for i, w := range want {
				if got := cells[i+2]; got != w {
					t.Errorf("%s: README says %q, the adapter gives %q", columns[i+2], got, w)
				}
			}
		})
	}
}
