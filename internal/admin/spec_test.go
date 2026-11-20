package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/raven-clown/transponder/api"
)

// spec is api/openapi.yaml, read for checking the handlers against it.
type spec struct {
	doc map[string]any
}

func loadSpec(t testing.TB) *spec {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(api.OpenAPI, &doc); err != nil {
		t.Fatalf("parsing openapi.yaml: %v", err)
	}
	return &spec{doc: doc}
}

func (s *spec) paths() map[string]any { return s.doc["paths"].(map[string]any) }

// operations returns "METHOD /path" for every operation in the file.
func (s *spec) operations() []string {
	var out []string
	for path, item := range s.paths() {
		for method := range item.(map[string]any) {
			if method == "parameters" {
				continue
			}
			out = append(out, strings.ToUpper(method)+" "+path)
		}
	}
	slices.Sort(out)
	return out
}

// ref follows a local $ref.
func (s *spec) ref(node map[string]any) map[string]any {
	for {
		ref, ok := node["$ref"].(string)
		if !ok {
			return node
		}
		var cur any = s.doc
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			cur = cur.(map[string]any)[part]
		}
		node = cur.(map[string]any)
	}
}

// match finds the path template of the file that path fits.
func (s *spec) match(path string) (string, bool) {
	segs := strings.Split(path, "/")
	for tmpl := range s.paths() {
		ts := strings.Split(tmpl, "/")
		if len(ts) != len(segs) {
			continue
		}
		ok := true
		for i := range ts {
			if strings.HasPrefix(ts[i], "{") {
				ok = ok && segs[i] != ""
				continue
			}
			ok = ok && ts[i] == segs[i]
		}
		if ok {
			return tmpl, true
		}
	}
	return "", false
}

// checkResponse checks that the file documents the response's status for the operation, and
// that a JSON body fits the documented schema.
func (s *spec) checkResponse(method, path string, rec *httptest.ResponseRecorder) error {
	tmpl, ok := s.match(strings.TrimPrefix(path, Prefix))
	if !ok {
		return fmt.Errorf("%s is not in openapi.yaml", path)
	}
	op, ok := s.paths()[tmpl].(map[string]any)[strings.ToLower(method)].(map[string]any)
	if !ok {
		return fmt.Errorf("%s %s is not in openapi.yaml", method, tmpl)
	}
	resp, ok := op["responses"].(map[string]any)[strconv.Itoa(rec.Code)].(map[string]any)
	if !ok {
		return fmt.Errorf("%s %s answered %d, which openapi.yaml does not list; body %s", method, tmpl, rec.Code, rec.Body)
	}
	resp = s.ref(resp)
	content, _ := resp["content"].(map[string]any)
	if content == nil {
		if rec.Body.Len() > 0 {
			return fmt.Errorf("%s %s %d: openapi.yaml lists no body, got %s", method, tmpl, rec.Code, rec.Body)
		}
		return nil
	}
	media, ok := content["application/json"].(map[string]any)
	if !ok {
		return nil
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		return fmt.Errorf("%s %s: Content-Type %q", method, tmpl, ct)
	}
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.UseNumber()
	var body any
	if err := dec.Decode(&body); err != nil {
		return fmt.Errorf("%s %s: body is not JSON: %w", method, tmpl, err)
	}
	if err := s.validate(media["schema"].(map[string]any), body, "$"); err != nil {
		return fmt.Errorf("%s %s %d: %w; body %s", method, tmpl, rec.Code, err, rec.Body)
	}
	return nil
}

// validate checks v against the subset of JSON Schema that openapi.yaml uses.
func (s *spec) validate(schema map[string]any, v any, at string) error {
	schema = s.ref(schema)
	if types := schemaTypes(schema["type"]); len(types) > 0 && !slices.ContainsFunc(types, func(t string) bool { return isType(t, v) }) {
		return fmt.Errorf("%s: %v is not of type %v", at, v, types)
	}
	if enum, ok := schema["enum"].([]any); ok && !slices.ContainsFunc(enum, func(e any) bool { return fmt.Sprint(e) == fmt.Sprint(v) }) {
		return fmt.Errorf("%s: %v is not one of %v", at, v, enum)
	}
	switch val := v.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		for _, r := range asList(schema["required"]) {
			if _, ok := val[r.(string)]; !ok {
				return fmt.Errorf("%s: required field %s is missing", at, r)
			}
		}
		for name, fv := range val {
			if ps, ok := props[name].(map[string]any); ok {
				if err := s.validate(ps, fv, at+"."+name); err != nil {
					return err
				}
				continue
			}
			switch extra := schema["additionalProperties"].(type) {
			case bool:
				if !extra {
					return fmt.Errorf("%s: field %s is not in the schema", at, name)
				}
			case map[string]any:
				if err := s.validate(extra, fv, at+"."+name); err != nil {
					return err
				}
			}
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range val {
				if err := s.validate(items, item, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func schemaTypes(t any) []string {
	switch t := t.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			out = append(out, x.(string))
		}
		return out
	}
	return nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func isType(t string, v any) bool {
	switch t {
	case "null":
		return v == nil
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		f, err := n.Float64()
		return err == nil && f == math.Trunc(f) && !strings.ContainsAny(n.String(), ".eE")
	}
	return false
}

// TestRoutesMatchOpenAPI checks that every endpoint the handler serves is in openapi.yaml and
// that every operation there is served.
func TestRoutesMatchOpenAPI(t *testing.T) {
	var served []string
	for _, rt := range (&handler{}).routes() {
		served = append(served, rt.method+" "+rt.path)
	}
	slices.Sort(served)
	if documented := loadSpec(t).operations(); !slices.Equal(served, documented) {
		t.Fatalf("served and documented endpoints differ\nserved:     %v\ndocumented: %v", served, documented)
	}
}

// TestRolesMatchOpenAPI checks the rule the file states: GET needs viewer, everything else admin.
func TestRolesMatchOpenAPI(t *testing.T) {
	for _, rt := range (&handler{}).routes() {
		want := "admin"
		if rt.method == http.MethodGet {
			want = "viewer"
		}
		if string(rt.need) != want {
			t.Errorf("%s %s needs %s, want %s", rt.method, rt.path, rt.need, want)
		}
	}
}

func TestSpecValidator(t *testing.T) {
	s := loadSpec(t)
	errSchema := map[string]any{"$ref": "#/components/schemas/Error"}
	tests := []struct {
		name string
		body string
		ok   bool
	}{
		{"valid", `{"error":{"code":"x","message":"y","field":"z"}}`, true},
		{"missing required", `{"error":{"code":"x"}}`, false},
		{"extra field", `{"error":{"code":"x","message":"y","other":1}}`, false},
		{"wrong type", `{"error":{"code":1,"message":"y"}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := json.NewDecoder(strings.NewReader(tt.body))
			dec.UseNumber()
			var v any
			if err := dec.Decode(&v); err != nil {
				t.Fatal(err)
			}
			if err := s.validate(errSchema, v, "$"); (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok %v", err, tt.ok)
			}
		})
	}
	key := map[string]any{"$ref": "#/components/schemas/Key"}
	for _, body := range []string{`{"name":"a"}`, `[]`} {
		var v any
		_ = json.Unmarshal([]byte(body), &v)
		if err := s.validate(key, v, "$"); err == nil {
			t.Errorf("%s passed as a Key", body)
		}
	}
	for _, typ := range []struct {
		t  string
		v  any
		ok bool
	}{
		{"integer", json.Number("3"), true},
		{"integer", json.Number("3.5"), false},
		{"number", json.Number("3.5"), true},
		{"null", nil, true},
		{"boolean", "true", false},
		{"unknown", 1, false},
	} {
		if got := isType(typ.t, typ.v); got != typ.ok {
			t.Errorf("isType(%s, %v) = %v", typ.t, typ.v, got)
		}
	}
}
