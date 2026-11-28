package server

import (
	"fmt"
	"math"
	"testing"
)

func TestFormatUSD(t *testing.T) {
	tests := map[int64]string{0: "0.000000", 4542000: "0.004542", 1_500_000_000: "1.500000", 999: "0.000000",
		1_000: "0.000001", 999_999_999: "0.999999", 123_456_789_012: "123.456789"}
	for in, want := range tests {
		if got := formatUSD(in); got != want {
			t.Errorf("formatUSD(%d) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []int64{0, 1, 999, 1_000, 1_001, 4_542_000, 999_999_999, 1_000_000_000, math.MaxInt64, -1, -1_500_000_000, math.MinInt64} {
		if got, want := formatUSD(in), fmt.Sprintf("%d.%06d", in/1_000_000_000, (in%1_000_000_000)/1_000); got != want {
			t.Errorf("formatUSD(%d) = %q, the printf form %q", in, got, want)
		}
	}
}
