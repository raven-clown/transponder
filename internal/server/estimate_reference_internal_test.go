package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/tokens"
)

// base64DataURI is the pattern the reference used for an inline payload; dataURI must agree with it.
var base64DataURI = regexp.MustCompile(`^data:[a-z]+/[a-zA-Z0-9.+-]+;base64,`)

// u writes the JSON escape for a UTF-16 code unit, such as u("00e9") for é.
func u(hex string) string { return `\` + "u" + hex }

// referenceEstimate is the estimate as it was first written, decoding the body into a tree and
// joining its text. estimateInput must give the same number for every body without duplicate keys.
func referenceEstimate(body []byte) int64 {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return tokens.Estimate(string(body))
	}
	var b bytes.Buffer
	var numbers, images int64
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch x := v.(type) {
		case map[string]any:
			if referenceIsImage(x) {
				images++
				x = referenceWithoutImageData(x)
			}
			for k, val := range x {
				b.WriteString(k)
				b.WriteByte(' ')
				walk(k, val)
			}
		case []any:
			for _, val := range x {
				if _, ok := val.(float64); ok {
					numbers++
					continue
				}
				walk(key, val)
			}
		case string:
			if key == "signature" {
				return
			}
			b.WriteString(x)
			b.WriteByte(' ')
		}
	}
	walk("", v)
	return tokens.Estimate(b.String()) + numbers + images*tokens.Image
}

func referenceWithoutImageData(part map[string]any) map[string]any {
	out := maps.Clone(part)
	if src, ok := out["source"].(map[string]any); ok && src["type"] == "base64" {
		src = maps.Clone(src)
		delete(src, "data")
		out["source"] = src
	}
	if u, ok := out["image_url"].(map[string]any); ok && referenceIsDataURI(u["url"]) {
		u = maps.Clone(u)
		delete(u, "url")
		out["image_url"] = u
	}
	for _, k := range []string{"image_url", "file_data"} {
		if referenceIsDataURI(out[k]) {
			delete(out, k)
		}
	}
	return out
}

func referenceIsDataURI(v any) bool {
	s, ok := v.(string)
	return ok && base64DataURI.MatchString(s)
}

func referenceIsImage(part map[string]any) bool {
	switch part["type"] {
	case "input_image", "input_file":
		return true
	case "image_url":
		return part["image_url"] != nil
	case "image":
		return part["source"] != nil
	}
	return false
}

// hasDuplicateKeys reports whether any object in a valid body names a key twice, which the
// reference keeps only the last of.
func hasDuplicateKeys(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	type frame struct {
		object bool
		keys   map[string]bool
		key    bool // the next string is a key
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return false
		}
		if err != nil {
			return false
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.object && top.key {
			if k, ok := tok.(string); ok {
				if top.keys[k] {
					return true
				}
				top.keys[k] = true
				top.key = false
				continue
			}
		}
		switch tok {
		case json.Delim('{'):
			if top != nil && top.object {
				top.key = true
			}
			stack = append(stack, &frame{object: true, keys: map[string]bool{}, key: true})
			continue
		case json.Delim('['):
			if top != nil && top.object {
				top.key = true
			}
			stack = append(stack, &frame{})
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
			continue
		}
		if top != nil && top.object {
			top.key = true
		}
	}
}

func TestEstimateInputMatchesReference(t *testing.T) {
	big := strings.Repeat("QUJD", 2_000)
	for name, body := range map[string]string{
		"empty":                      ``,
		"not json":                   `{"model": "gpt", oops`,
		"trailing data":              `{} {}`,
		"top-level string":           `"hello there"`,
		"top-level number":           `12`,
		"numbers in arrays":          `{"input":[1,2,3,[4,5,-6.5e3]],"n":2}`,
		"signature skipped":          `{"signature":"abc","blocks":[{"signature":["x","y"],"text":"kept"}]}`,
		"escapes":                    `{"text":"line\nnext\t\"quoted\" \\ \/ \b\f\r"}`,
		"unicode escapes":            `{"text":"caf` + u("00e9") + ` ` + u("0e2a") + u("0e27") + ` ` + u("4f60") + u("597d") + ` ` + u("0041") + `"}`,
		"surrogate pair":             `{"text":"` + u("d83d") + u("de00") + ` and ` + u("D83D") + u("DE00") + `"}`,
		"reversed surrogates":        `{"text":"` + u("de00") + u("d83d") + ` end ` + u("d83d") + `"}`,
		"lone surrogates":            `{"text":"\ud83d x \ude00 \ud83dA \ud83d😀"}`,
		"raw utf-8":                  `{"text":"สวัสดีครับ 你好世界 😀"}`,
		"invalid utf-8":              "{\"text\":\"a\xffb\xc3\x28c\xe2\x82\"}",
		"escaped keys":               `{"ty` + u("0070") + `e":"image","sour` + u("0063") + `e":{"type":"base64","d` + u("0061") + `ta":"` + big + `"}}`,
		"escaped type":               `{"type":"imag` + u("0065") + `_url","image_url":{"url":"data:image/png;base64,` + big + `"}}`,
		"escaped base64 type":        `{"type":"image","source":{"type":"base` + u("0036") + `4","data":"` + big + `"}}`,
		"escaped data uri":           `{"type":"input_image","image_url":"dat` + u("0061") + `:image\/png;base64,` + big + `"}`,
		"escaped url key":            `{"type":"image_url","image_url":{"u` + u("0072") + `l":"data:image/png;base64,` + big + `"}}`,
		"escaped signature key":      `{"sig` + u("006e") + `ature":"skipped text","text":"kept"}`,
		"long escaped key":           `{"` + strings.Repeat(u("0061"), 40) + `":"x","type":"image","source":{"type":"base64","data":"` + big + `"}}`,
		"escaped url not a data uri": `{"type":"image_url","image_url":{"url":"https:` + u("002f") + `/x/a.png"}}`,
		"anthropic image":            `{"messages":[{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + big + `"}}]}]}`,
		"anthropic url image":        `{"messages":[{"content":[{"type":"image","source":{"type":"url","url":"https://x/a.png"}}]}]}`,
		"anthropic document":         `{"messages":[{"content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + big + `"}}]}]}`,
		"openai image":               `{"messages":[{"content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + big + `","detail":"low"}}]}]}`,
		"openai image at a url":      `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"https://x/a.png"}}]}]}`,
		"image_url null":             `{"type":"image_url","image_url":null}`,
		"source null":                `{"type":"image","source":null}`,
		"source false":               `{"type":"image","source":false}`,
		"responses image":            `{"input":[{"content":[{"type":"input_image","image_url":"data:image/png;base64,` + big + `"}]}]}`,
		"responses file":             `{"input":[{"content":[{"type":"input_file","file_data":"data:application/pdf;base64,` + big + `","filename":"a.pdf"}]}]}`,
		"data hidden in image part":  `{"type":"image","source":{"type":"url","url":"https://x"},"extra":{"type":"base64","data":"` + big + `"}}`,
		"data url outside image":     `{"messages":[{"content":"data:text/plain;base64,` + big + `"}]}`,
		"image inside a source":      `{"type":"image","source":{"type":"base64","data":{"type":"input_image","x":"y"}}}`,
		"non-string type":            `{"type":5,"image_url":{"url":"data:image/png;base64,AAAA"}}`,
		"whitespace":                 " \n\t{ \"a\" : [ 1 , \"b\" , { } , [ ] ] , \"c\" : null , \"d\" : true , \"e\" : false } \r\n",
		"deep":                       strings.Repeat(`{"a":[`, 200) + `"x"` + strings.Repeat(`]}`, 200),
	} {
		t.Run(name, func(t *testing.T) {
			if got, want := estimateInput([]byte(body)), referenceEstimate([]byte(body)); got != want {
				t.Fatalf("estimate %d, reference %d", got, want)
			}
		})
	}
}

func TestEstimateInputCountsDuplicateKeysAtLeastOnce(t *testing.T) {
	data := strings.Repeat("QUJD", 5_000)
	for name, body := range map[string]string{
		"a repeated text key": `{"text":"short","text":"a much longer value that replaces it"}`,
		// A later type that is not a string must undo an earlier image type, or text hidden as
		// an image's data would count as one image.
		"type, then null":           `{"type":"image","type":null,"source":{"type":"base64","data":"` + data + `"}}`,
		"type, then a number":       `{"type":"image","type":5,"source":{"type":"base64","data":"` + data + `"}}`,
		"type, then an object":      `{"type":"input_image","type":{},"image_url":"data:image/png;base64,` + data + `"}`,
		"source type, then number":  `{"type":"image","source":{"type":"base64","type":5,"data":"` + data + `"}}`,
		"type, then another string": `{"type":"image","type":"text","source":{"type":"base64","data":"` + data + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if !hasDuplicateKeys([]byte(body)) {
				t.Fatal("the helper missed a duplicate key")
			}
			if got, ref := estimateInput([]byte(body)), referenceEstimate([]byte(body)); got < ref {
				t.Fatalf("estimate %d is below the reference %d", got, ref)
			}
		})
	}
}

func TestDataURIMatchesPattern(t *testing.T) {
	for _, s := range []string{
		"data:image/png;base64,AAAA", "data:image/png;base64,", "data:application/pdf;base64,x",
		"data:a/b;base64,", "data:image/svg+xml;base64,x", "data:image/x-icon.v2;base64,x",
		"data:Image/png;base64,x", "data:/png;base64,x", "data:image/;base64,x", "data:image/png;base64",
		"data:image/png;charset=utf-8;base64,x", "data:image/png,x", "data:image png;base64,x",
		"data:", "data", "", "xdata:image/png;base64,", "data:image/pngé;base64,x", "data:imageé/png;base64,x",
		"data:image/png;BASE64,x", " data:image/png;base64,x",
	} {
		quoted, _ := json.Marshal(s)
		var escaped strings.Builder
		for _, r := range s {
			if r < 0x10000 {
				fmt.Fprintf(&escaped, `\u%04x`, r)
			} else {
				escaped.WriteRune(r)
			}
		}
		want := base64DataURI.MatchString(s)
		for _, raw := range []string{string(quoted), `"` + escaped.String() + `"`} {
			sc := inputScanner{b: []byte(raw)}
			lo, hi, esc, _, _ := sc.str()
			if got := sc.dataURI(scanned{lo: lo, hi: hi, esc: esc}); got != want {
				t.Errorf("dataURI(%s) = %v, the pattern says %v", raw, got, want)
			}
		}
	}
}

func TestEstimateInputDoesNotCopyEscapedStrings(t *testing.T) {
	body := []byte(`{"type":"image_url","image_url":{"url":"see` + strings.Repeat(`\n this `, 25_000) + `"},"file_data":"x` + strings.Repeat(u("0041"), 10_000) + `"}`)
	if !json.Valid(body) {
		t.Fatal("the test body is not JSON")
	}
	if allocs := testing.AllocsPerRun(20, func() { estimateInput(body) }); allocs > 0 {
		t.Fatalf("%v allocations for a body of escaped strings", allocs)
	}
	if got, want := estimateInput(body), referenceEstimate(body); got != want {
		t.Fatalf("estimate %d, reference %d", got, want)
	}
}

func BenchmarkEstimateInput(b *testing.B) {
	chat := `{"model":"gpt-4o-mini","messages":[{"role":"system","content":"You are a careful assistant that answers in short paragraphs."},` +
		`{"role":"user","content":"` + strings.Repeat("Summarise the plot of a film about a lighthouse keeper. ", 8) + `"}],` +
		`"tools":[{"type":"function","function":{"name":"get_weather","description":"Weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"max_tokens":256}`
	image := `{"model":"claude-sonnet-4-5","max_tokens":256,"messages":[{"role":"user","content":[{"type":"text","text":"what is this"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + strings.Repeat("QUJD", 75_000) + `"}}]}]}`
	for _, tt := range []struct{ name, body string }{{"chat", chat}, {"image_300KB", image}} {
		body := []byte(tt.body)
		b.Run(tt.name+"/scan", func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				estimateInput(body)
			}
		})
		b.Run(tt.name+"/tree", func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				referenceEstimate(body)
			}
		})
	}
}

func FuzzEstimateInputMatchesReference(f *testing.F) {
	for _, s := range []string{
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
		`{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
		`{"input":[{"content":[{"type":"input_file","file_data":"data:application/pdf;base64,AAAA"}]}],"signature":"s"}`,
		`{"text":"😀 é \ud83d x"}`,
		"{\"t\":\"\xff\xfe\"}",
		`[1,2,[3,"x"]]`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		got, want := estimateInput(body), referenceEstimate(body)
		if json.Valid(body) && hasDuplicateKeys(body) {
			if got < want {
				t.Fatalf("estimate %d below reference %d for %q", got, want, body)
			}
			return
		}
		if got != want {
			t.Fatalf("estimate %d, reference %d for %q", got, want, body)
		}
	})
}
