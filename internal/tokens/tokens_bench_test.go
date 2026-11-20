package tokens_test

import (
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/tokens"
)

func BenchmarkEstimate(b *testing.B) {
	for _, tt := range []struct{ name, text string }{
		{"english 1KB", strings.Repeat("Measure the store write time apart from the upstream time. ", 17)},
		{"thai 1KB", strings.Repeat("วัดเวลาเขียนลงที่เก็บแยกจากเวลาของต้นทาง ", 9)},
		{"code 1KB", strings.Repeat("func (r *Router) Plan(model string) (Plan, error) { return p, nil }\n", 15)},
	} {
		b.Run(tt.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(tt.text)))
			for b.Loop() {
				tokens.Estimate(tt.text)
			}
		})
	}
}
