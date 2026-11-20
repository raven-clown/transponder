package server

import (
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/tokens"
)

func TestMaxField(t *testing.T) {
	top := topFields([]byte(`{"max_tokens":10,"Max_Completion_Tokens":40,"n":3,"N":2,"max_output_tokens":"x"}`))
	for _, tt := range []struct {
		fields []string
		want   int64
	}{
		{[]string{"max_completion_tokens", "max_tokens"}, 40},
		{[]string{"n"}, 3},
		{[]string{"max_output_tokens"}, 0},
		{[]string{"missing"}, 0},
	} {
		if got := maxField(top, tt.fields...); got != tt.want {
			t.Errorf("maxField(%v) = %d, want %d", tt.fields, got, tt.want)
		}
	}
	if topFields([]byte(`[1]`)) != nil || maxField(nil, "n") != 0 {
		t.Error("a body that is not an object has no fields")
	}
}

func TestEstimateInputImageData(t *testing.T) {
	big := strings.Repeat("QUJD", 25_000)
	bigText := tokens.Estimate(big)
	tests := []struct {
		name     string
		body     string
		min, max int64
	}{
		{"anthropic image", `{"messages":[{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + big + `"}}]}]}`, tokens.Image, tokens.Image + 20},
		{"openai image", `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + big + `"}}]}]}`, tokens.Image, tokens.Image + 20},
		{"responses image", `{"input":[{"content":[{"type":"input_image","image_url":"data:image/png;base64,` + big + `"}]}]}`, tokens.Image, tokens.Image + 20},
		{"responses file", `{"input":[{"content":[{"type":"input_file","file_data":"data:application/pdf;base64,` + big + `"}]}]}`, tokens.Image, tokens.Image + 20},
		{"anthropic document", `{"messages":[{"content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + big + `"}}]}]}`, bigText, bigText + 30},
		{"data hidden in an image part", `{"messages":[{"content":[{"type":"image","source":{"type":"url","url":"https://x"},"extra":{"type":"base64","data":"` + big + `"}}]}]}`, bigText, bigText + tokens.Image + 30},
		{"data URL outside an image part", `{"messages":[{"content":"data:text/plain;base64,` + big + `"}]}`, bigText, bigText + 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := estimateInput([]byte(tt.body)); got < tt.min || got > tt.max {
				t.Fatalf("estimate %d, want %d to %d", got, tt.min, tt.max)
			}
		})
	}
}
