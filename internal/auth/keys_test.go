package auth

import (
	"regexp"
	"testing"
)

func TestNewKeyFormat(t *testing.T) {
	plain, hash, last4, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^tp_[A-Za-z0-9_-]{43}$`).MatchString(plain) {
		t.Fatalf("key %q has the wrong format", plain)
	}
	if last4 != plain[len(plain)-4:] {
		t.Fatalf("last4 = %q, key ends with %q", last4, plain[len(plain)-4:])
	}
	if HashKey(plain) != hash {
		t.Fatal("HashKey does not match the returned hash")
	}
	other, _, _, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if other == plain {
		t.Fatal("two keys are equal")
	}
}
