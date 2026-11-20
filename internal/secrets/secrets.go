// Package secrets seals values such as provider API keys with AES-256-GCM before they are
// stored, under the 32-byte secrets_key.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	keySize = 32
	prefix  = "v1."
)

// ErrNoKey means a secret has to be sealed or opened but no secrets_key is set.
var ErrNoKey = errors.New("secrets: no secrets_key is set")

// Box seals and opens secrets. A nil Box has no key and fails with ErrNoKey.
type Box struct {
	aead cipher.AEAD
}

// ParseKey reads a 32-byte key written as base64 (standard or URL-safe, padded or not) or as
// 64 hex digits.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) == 2*keySize {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == keySize {
			return b, nil
		}
	}
	return nil, errors.New("secrets_key must be 32 bytes, written as base64 or 64 hex digits; make one with: openssl rand -base64 32")
}

// New returns a Box for key, which must be 32 bytes.
func New(key []byte) (*Box, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("secrets: key is %d bytes, want %d", len(key), keySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// LoadOrCreate reads the key in the file at path, or writes a new random one there, readable
// by the owner only, when there is no file yet.
func LoadOrCreate(path string) (*Box, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, keySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("creating %s: %w", path, err)
		}
		_, werr := f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n")
		if err := errors.Join(werr, f.Close()); err != nil {
			return nil, fmt.Errorf("writing %s: %w", path, err)
		}
		return New(key)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	key, err := ParseKey(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return New(key)
}

// Seal encrypts plaintext. context binds the result to where it is stored, such as
// "providers/openai", so a sealed value copied to another item does not open.
func (b *Box) Seal(plaintext, context string) (string, error) {
	if b == nil {
		return "", ErrNoKey
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(plaintext), []byte(context))
	return prefix + base64.RawURLEncoding.EncodeToString(out), nil
}

// Open decrypts what Seal returned for the same context.
func (b *Box) Open(sealed, context string) (string, error) {
	if b == nil {
		return "", ErrNoKey
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, prefix))
	if !strings.HasPrefix(sealed, prefix) || err != nil || len(raw) < b.aead.NonceSize() {
		return "", errors.New("secrets: not a sealed value")
	}
	n := b.aead.NonceSize()
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], []byte(context))
	if err != nil {
		return "", errors.New("secrets: the value does not open with this secrets_key")
	}
	return string(plain), nil
}
