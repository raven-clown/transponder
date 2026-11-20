package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

const keyPrefix = "tp_"

// NewKey returns a new API key, its hash and its last four characters.
func NewKey() (plaintext string, hash [32]byte, last4 string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", hash, "", err
	}
	plaintext = keyPrefix + base64.RawURLEncoding.EncodeToString(b)
	return plaintext, HashKey(plaintext), plaintext[len(plaintext)-4:], nil
}

// HashKey returns the hash under which a key is stored.
func HashKey(plaintext string) [32]byte {
	return sha256.Sum256([]byte(plaintext))
}
