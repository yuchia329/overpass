// Package secret makes random bearer secrets and the hashes stored in their place.
package secret

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// New returns a random 192-bit secret, hex encoded, with a readable prefix.
func New(prefix string) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

// Hash returns the value stored instead of the secret itself.
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
