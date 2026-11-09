package tokens

import (
	"testing"
	"unicode/utf8"
)

func TestEstimate(t *testing.T) {
	thai := "สวัสดีครับวันนี้อากาศดีมาก"
	tests := []struct {
		name string
		in   string
		min  int64
		max  int64
	}{
		{"empty", "", 0, 0},
		{"english", "abcdefgh", 2, 2},
		{"english rounds up", "abcde", 2, 2},
		{"thai counts a token per character", thai, int64(utf8.RuneCountInString(thai)), int64(utf8.RuneCountInString(thai))},
		{"mixed", "hi สวัสดี", 7, 7},
		{"cjk", "你好世界", 4, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Estimate(tt.in); got < tt.min || got > tt.max {
				t.Fatalf("Estimate(%q) = %d, want %d to %d", tt.in, got, tt.min, tt.max)
			}
		})
	}
}
