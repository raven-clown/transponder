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
		{"thai counts one and a half tokens a character", thai, int64(utf8.RuneCountInString(thai)) * 3 / 2, int64(utf8.RuneCountInString(thai))*3/2 + 1},
		{"mixed", "hi สวัสดี", 10, 10},
		{"cjk", "你好世界", 6, 6},
		{"chinese that a tokenizer counts high", "我们今天去看了一场很好的电影", 17, 21},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Estimate(tt.in); got < tt.min || got > tt.max {
				t.Fatalf("Estimate(%q) = %d, want %d to %d", tt.in, got, tt.min, tt.max)
			}
		})
	}
}
