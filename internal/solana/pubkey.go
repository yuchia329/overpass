// Package solana holds Solana helpers shared across the backend.
package solana

import "math/big"

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// DecodeBase58 decodes a base58 string. ok is false on any invalid character.
func DecodeBase58(s string) (out []byte, ok bool) {
	n := new(big.Int)
	radix := big.NewInt(58)
	for i := 0; i < len(s); i++ {
		idx := -1
		for j := 0; j < len(base58Alphabet); j++ {
			if base58Alphabet[j] == s[i] {
				idx = j
				break
			}
		}
		if idx < 0 {
			return nil, false
		}
		n.Mul(n, radix)
		n.Add(n, big.NewInt(int64(idx)))
	}
	// Each leading '1' encodes a leading zero byte.
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	return append(make([]byte, zeros), n.Bytes()...), true
}

// IsPubkey reports whether s is a base58-encoded 32-byte Solana public key.
func IsPubkey(s string) bool {
	b, ok := DecodeBase58(s)
	return ok && len(b) == 32
}
