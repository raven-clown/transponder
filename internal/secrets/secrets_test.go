package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testKey() []byte { return bytes.Repeat([]byte{7}, keySize) }

func TestParseKey(t *testing.T) {
	key := testKey()
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"base64", base64.StdEncoding.EncodeToString(key), true},
		{"base64 raw url", base64.RawURLEncoding.EncodeToString(key), true},
		{"hex", hex.EncodeToString(key), true},
		{"with newline", base64.StdEncoding.EncodeToString(key) + "\n", true},
		{"short", base64.StdEncoding.EncodeToString(key[:16]), false},
		{"not encoded", "correct horse battery staple", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseKey(tt.in)
			if (err == nil) != tt.ok || (tt.ok && !bytes.Equal(got, key)) {
				t.Fatalf("ParseKey = %x, %v", got, err)
			}
		})
	}
}

func TestSealOpen(t *testing.T) {
	b, err := New(testKey())
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := b.Seal("sk-secret", "providers/openai")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, prefix) || strings.Contains(sealed, "sk-secret") {
		t.Fatalf("sealed %q", sealed)
	}
	again, _ := b.Seal("sk-secret", "providers/openai")
	if again == sealed {
		t.Fatal("two seals of one value are the same; the nonce is not random")
	}
	if got, err := b.Open(sealed, "providers/openai"); err != nil || got != "sk-secret" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if _, err := b.Open(sealed, "providers/other"); err == nil {
		t.Fatal("opened under another context")
	}
	other, _ := New(bytes.Repeat([]byte{8}, keySize))
	if _, err := other.Open(sealed, "providers/openai"); err == nil {
		t.Fatal("opened with another key")
	}
	for _, bad := range []string{"", "plain", prefix + "!!!", prefix + "AAAA"} {
		if _, err := b.Open(bad, "providers/openai"); err == nil {
			t.Fatalf("opened %q", bad)
		}
	}
}

func TestNilBox(t *testing.T) {
	var b *Box
	if _, err := b.Seal("x", "c"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := b.Open("x", "c"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Open: %v", err)
	}
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("New took a short key")
	}
}

func TestLoadOrCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transponder.key")
	b1, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
	sealed, _ := b1.Seal("v", "c")
	b2, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b2.Open(sealed, "c"); err != nil || got != "v" {
		t.Fatalf("the key was not kept: %q, %v", got, err)
	}

	bad := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(bad, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(bad); err == nil {
		t.Fatal("loaded a bad key file")
	}
	if _, err := LoadOrCreate(filepath.Join(t.TempDir(), "missing-dir", "k")); err == nil {
		t.Fatal("created a key in a directory that does not exist")
	}
	if _, err := LoadOrCreate(t.TempDir()); err == nil {
		t.Fatal("read a directory as a key")
	}
}
