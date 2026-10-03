package solana

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
)

// ReadKeypair reads a keypair file in the Solana CLI's format: a JSON array of
// 64 bytes, the private seed followed by the public key.
func ReadKeypair(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ints []int
	if err := json.Unmarshal(raw, &ints); err != nil {
		return nil, fmt.Errorf("%s: want a JSON array of 64 bytes: %w", path, err)
	}
	if len(ints) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%s: want 64 bytes, got %d", path, len(ints))
	}
	key := make(ed25519.PrivateKey, 0, len(ints))
	for _, v := range ints {
		if v < 0 || v > 255 {
			return nil, fmt.Errorf("%s: %d is not a byte", path, v)
		}
		key = append(key, byte(v))
	}
	if !bytes.Equal(ed25519.NewKeyFromSeed(key.Seed()), key) {
		return nil, fmt.Errorf("%s: the public key does not match the private seed", path)
	}
	return key, nil
}
