package telemetry

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// scopeName names the code that made the spans, as OTLP's instrumentation scope.
const scopeName = "github.com/raven-clown/transponder"

// otlpExporter posts spans to a collector as OTLP over HTTP with JSON bodies. The JSON
// encoding is part of the OTLP specification, so every collector that takes OTLP/HTTP
// reads it, and it needs no protobuf code in the build.
type otlpExporter struct {
	url     string
	headers map[string]string
	client  *http.Client
	version string
}

// The types below are the OTLP JSON shape of an ExportTraceServiceRequest. Ids are hex and
// 64-bit integers are strings, as the specification says.
type otlpRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type otlpSpan struct {
	TraceID      string         `json:"traceId"`
	SpanID       string         `json:"spanId"`
	ParentSpanID string         `json:"parentSpanId,omitempty"`
	TraceState   string         `json:"traceState,omitempty"`
	Name         string         `json:"name"`
	Kind         SpanKind       `json:"kind"`
	Start        string         `json:"startTimeUnixNano"`
	End          string         `json:"endTimeUnixNano"`
	Attributes   []otlpKeyValue `json:"attributes,omitempty"`
	Status       *otlpStatus    `json:"status,omitempty"`
}

type otlpKeyValue struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	String *string  `json:"stringValue,omitempty"`
	Int    *string  `json:"intValue,omitempty"`
	Double *float64 `json:"doubleValue,omitempty"`
	Bool   *bool    `json:"boolValue,omitempty"`
}

type otlpStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

// otlpStatusError is STATUS_CODE_ERROR.
const otlpStatusError = 2

func stringValue(s string) otlpValue { return otlpValue{String: &s} }

func encodeAttr(a Attr) otlpKeyValue {
	kv := otlpKeyValue{Key: a.Key}
	switch a.Kind {
	case AttrInt:
		n := strconv.FormatInt(a.Int, 10)
		kv.Value.Int = &n
	case AttrFloat:
		kv.Value.Double = &a.Float
	case AttrBool:
		kv.Value.Bool = &a.Bool
	default:
		kv.Value.String = &a.Str
	}
	return kv
}

// encodeSpans renders spans as one OTLP JSON export request.
func encodeSpans(spans []SpanData, version string) ([]byte, error) {
	out := make([]otlpSpan, len(spans))
	for i, s := range spans {
		o := otlpSpan{
			TraceID:    hex.EncodeToString(s.TraceID[:]),
			SpanID:     hex.EncodeToString(s.SpanID[:]),
			TraceState: s.TraceState,
			Name:       s.Name,
			Kind:       s.Kind,
			Start:      strconv.FormatInt(s.Start.UnixNano(), 10),
			End:        strconv.FormatInt(s.End.UnixNano(), 10),
		}
		if s.ParentID != [8]byte{} {
			o.ParentSpanID = hex.EncodeToString(s.ParentID[:])
		}
		for _, a := range s.Attrs {
			o.Attributes = append(o.Attributes, encodeAttr(a))
		}
		if s.Failed {
			o.Status = &otlpStatus{Code: otlpStatusError, Message: s.Message}
		}
		out[i] = o
	}
	resource := []otlpKeyValue{{Key: "service.name", Value: stringValue("transponder")}}
	if version != "" {
		resource = append(resource, otlpKeyValue{Key: "service.version", Value: stringValue(version)})
	}
	return json.Marshal(otlpRequest{ResourceSpans: []otlpResourceSpans{{
		Resource:   otlpResource{Attributes: resource},
		ScopeSpans: []otlpScopeSpans{{Scope: otlpScope{Name: scopeName, Version: version}, Spans: out}},
	}}})
}

// Export posts one batch. Any answer but 2xx is an error; the batch is not sent again.
func (e *otlpExporter) Export(ctx context.Context, spans []SpanData) error {
	body, err := encodeSpans(spans, e.version)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// The body is read so the connection can be used again; what it says is not needed.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the collector answered %s", resp.Status)
	}
	return nil
}
