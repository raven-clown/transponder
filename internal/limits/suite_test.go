package limits_test

import (
	"testing"

	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/limits/limitstest"
)

func TestMemoryStore(t *testing.T) {
	limitstest.Run(t, func(*testing.T) limits.Store { return limits.NewMemoryStore() })
}
