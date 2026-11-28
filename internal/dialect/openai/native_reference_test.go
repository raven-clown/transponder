package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The functions below are PeekRequest, PeekResponses and RewriteRequest as first written, on a
// decoded map. The versions that scan the body must agree with them on every body.

func referenceField(m map[string]json.RawMessage, key string, dst any) error {
	raw, ok := m[key]
	if !ok || isNull(raw) {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return &RequestError{Param: key, Code: "invalid_json", Message: key + " has the wrong type"}
	}
	return nil
}

func referencePeek(body []byte) (Peek, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return Peek{}, invalidJSON("the request body is not a JSON object")
	}
	var p Peek
	if err := referenceField(m, "model", &p.Model); err != nil {
		return Peek{}, err
	}
	if err := referenceField(m, "stream", &p.Stream); err != nil {
		return Peek{}, err
	}
	if raw, ok := m["stream_options"]; ok && !isNull(raw) {
		var opts map[string]json.RawMessage
		if err := json.Unmarshal(raw, &opts); err != nil {
			return Peek{}, invalidJSON("stream_options must be an object")
		}
		if err := referenceField(opts, "include_usage", &p.IncludeUsage); err != nil {
			return Peek{}, err
		}
	}
	if p.Model == "" {
		return Peek{}, &RequestError{Param: "model", Code: "missing_field", Message: "model is required"}
	}
	return p, nil
}

func referencePeekResponses(body []byte) (ResponsesPeek, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return ResponsesPeek{}, invalidJSON("the request body is not a JSON object")
	}
	var p ResponsesPeek
	if err := referenceField(m, "model", &p.Model); err != nil {
		return ResponsesPeek{}, err
	}
	if err := referenceField(m, "stream", &p.Stream); err != nil {
		return ResponsesPeek{}, err
	}
	if err := referenceField(m, "background", &p.Background); err != nil {
		return ResponsesPeek{}, err
	}
	if p.Model == "" {
		return ResponsesPeek{}, &RequestError{Param: "model", Code: "missing_field", Message: "model is required"}
	}
	return p, nil
}

// decoded is a body as encoding/json reads it, so two bodies that differ only in key order,
// spacing or escapes compare equal.
// decoded reads b with numbers kept as text, since a number past float64's range is still
// valid JSON that the comparison has to see.
func decoded(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("not JSON: %v: %s", err, b)
	}
	return v
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	var re *RequestError
	if errors.As(err, &re) {
		return re.Param + "|" + re.Code + "|" + re.Message
	}
	return "error"
}

func checkAgainstReference(t *testing.T, body []byte) {
	t.Helper()
	got, gerr := PeekRequest(body)
	want, werr := referencePeek(body)
	if got != want || errText(gerr) != errText(werr) {
		t.Fatalf("PeekRequest(%q) = %+v, %v; reference %+v, %v", body, got, gerr, want, werr)
	}
	rgot, rgerr := PeekResponses(body)
	rwant, rwerr := referencePeekResponses(body)
	if rgot != rwant || errText(rgerr) != errText(rwerr) {
		t.Fatalf("PeekResponses(%q) = %+v, %v; reference %+v, %v", body, rgot, rgerr, rwant, rwerr)
	}
	for _, force := range []bool{false, true} {
		out, err := RewriteRequest(body, "up<&>model", force)
		ref, rerr := rewriteByMap(body, "up<&>model", force)
		if (err == nil) != (rerr == nil) {
			t.Fatalf("RewriteRequest(%q, %v) err %v; reference err %v", body, force, err, rerr)
		}
		if err != nil {
			continue
		}
		if !reflect.DeepEqual(decoded(t, out), decoded(t, ref)) {
			t.Fatalf("RewriteRequest(%q, %v) =\n%s\nreference\n%s", body, force, out, ref)
		}
	}
}

func TestScannedRequestsMatchReference(t *testing.T) {
	for name, body := range map[string]string{
		"plain":                  `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
		"stream with options":    `{"model":"m","stream":true,"stream_options":{"include_usage":false,"x":[1,{"y":"}"}]}}`,
		"stream options null":    `{"model":"m","stream":true,"stream_options":null}`,
		"stream options not obj": `{"model":"m","stream_options":3}`,
		"stream options empty":   `{"model":"m","stream_options":{}}`,
		"options repeat a key":   `{"model":"m","stream_options":{"include_usage":false,"include_usage":false}}`,
		"case variants":          `{"model":"a","Model":"x","stream":true,"STREAM":false,"Stream_Options":{}}`,
		"escaped key":            `{"model":"esc","stream":false}`,
		"escaped model":          `{"model":"a\"b\\cé"}`,
		"repeated key":           `{"model":"first","temperature":1,"model":"second","temperature":2}`,
		"model null":             `{"model":null}`,
		"model number":           `{"model":5}`,
		"stream string":          `{"model":"m","stream":"yes"}`,
		"include usage string":   `{"model":"m","stream_options":{"include_usage":"yes"}}`,
		"background":             `{"model":"m","background":true}`,
		"empty object":           `{}`,
		"array":                  `[1]`,
		"null":                   `null`,
		"not json":               `{"model":`,
		"invalid utf-8 in model": "{\"model\":\"a\xffb\"}",
		"whitespace":             " \n{ \"model\" : \"m\" , \"stream\" : true , \"n\" : [ 1 , 2 ] } \t",
		"braces inside strings":  `{"model":"m","messages":[{"content":"{\"[x]\"}"}],"tools":[]}`,
		"html in values":         `{"model":"m","content":"<b>&amp;</b>"}`,
		"deep values":            `{"model":"m","x":` + strings.Repeat(`[{"a":`, 50) + `1` + strings.Repeat(`}]`, 50) + `}`,
	} {
		t.Run(name, func(t *testing.T) { checkAgainstReference(t, []byte(body)) })
	}
}

func FuzzScannedRequestsMatchReference(f *testing.F) {
	for _, s := range []string{
		`{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[]}`,
		`{"Model":"x","model":"y","STREAM":true}`,
		`{"model":"m","stream_options":null}`,
		`{"model":"m","model":"n"}`,
		`{"model":"m","stream_options":{"a":1,"a":2}}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		checkAgainstReference(t, body)
	})
}
