package server

import (
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/tokens"
)

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
