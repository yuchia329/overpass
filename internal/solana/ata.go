package solana

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"filippo.io/edwards25519"
)

const (
	// USDCMint is the mainnet USDC mint.
	USDCMint               = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	tokenProgram           = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	associatedTokenProgram = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
)

// AssociatedTokenAddress derives owner's associated token account for mint.
// SPL transfers to owner land in this account, not in owner itself.
func AssociatedTokenAddress(owner, mint string) (string, error) {
	var seeds [][]byte
	for _, k := range []string{owner, tokenProgram, mint} {
		b, ok := DecodeBase58(k)
		if !ok || len(b) != 32 {
			return "", fmt.Errorf("%q is not a Solana public key", k)
		}
		seeds = append(seeds, b)
	}
	program, _ := DecodeBase58(associatedTokenProgram)
	return findProgramAddress(seeds, program)
}

// findProgramAddress returns the first off-curve address, trying bump seeds
// from 255 down, the same search the Solana runtime does.
func findProgramAddress(seeds [][]byte, program []byte) (string, error) {
	for bump := 255; bump >= 0; bump-- {
		h := sha256.New()
		for _, s := range seeds {
			h.Write(s)
		}
		h.Write([]byte{byte(bump)})
		h.Write(program)
		h.Write([]byte("ProgramDerivedAddress"))
		addr := h.Sum(nil)
		if _, err := new(edwards25519.Point).SetBytes(addr); err != nil {
			return EncodeBase58(addr), nil
		}
	}
	return "", errors.New("no off-curve program address")
}
