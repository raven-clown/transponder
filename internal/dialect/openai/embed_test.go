package openai

import (
	"errors"
	"reflect"
	"testing"

	"github.com/raven-clown/transponder/internal/schema"
)

func TestParseEmbedRequest(t *testing.T) {
	tests := []struct {
		body     string
		want     []string
		wantCode string
	}{
		{body: `{"model":"e","input":"one"}`, want: []string{"one"}},
		{body: `{"model":"e","input":["a","b"]}`, want: []string{"a", "b"}},
		{body: `{"model":"e"}`, wantCode: "missing_field"},
		{body: `{"model":"e","input":[]}`, wantCode: "missing_field"},
		{body: `{"input":"x"}`, wantCode: "missing_field"},
		{body: `{"model":"e","input":[1,2]}`, wantCode: "unsupported"},
		{body: `nope`, wantCode: "invalid_json"},
	}
	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			got, err := ParseEmbedRequest([]byte(tt.body))
			if tt.wantCode != "" {
				var re *RequestError
				if !errors.As(err, &re) || re.Code != tt.wantCode {
					t.Fatalf("err = %v, want %s", err, tt.wantCode)
				}
				return
			}
			if err != nil || got.Model != "e" || !reflect.DeepEqual(got.Input, tt.want) {
				t.Fatalf("ParseEmbedRequest = %+v, %v", got, err)
			}
		})
	}
}

func TestEncodeEmbedResponse(t *testing.T) {
	got, err := EncodeEmbedResponse(&schema.EmbedResponse{Model: "e", Vectors: [][]float32{{0.5, 1}, {0}}, Usage: schema.Usage{InputTokens: 4}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.5,1]},{"object":"embedding","index":1,"embedding":[0]}],"model":"e","usage":{"prompt_tokens":4,"total_tokens":4}}`
	if string(got) != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
