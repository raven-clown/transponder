package server

import "testing"

func TestFormatUSD(t *testing.T) {
	tests := map[int64]string{0: "0.000000", 4542000: "0.004542", 1_500_000_000: "1.500000", 999: "0.000000"}
	for in, want := range tests {
		if got := formatUSD(in); got != want {
			t.Errorf("formatUSD(%d) = %q, want %q", in, got, want)
		}
	}
}
