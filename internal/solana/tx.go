package solana

import (
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"slices"
)

const systemProgram = "11111111111111111111111111111111"

// AccountMeta is one account an instruction reads or writes.
type AccountMeta struct {
	Pubkey   string
	Signer   bool
	Writable bool
}

// Instruction is one call to a program in a transaction.
type Instruction struct {
	Program  string
	Accounts []AccountMeta
	Data     []byte
}

// CreateAssociatedTokenAccountIdempotent creates owner's associated token
// account for mint, paid by payer, unless it already exists.
func CreateAssociatedTokenAccountIdempotent(payer, account, owner, mint string) Instruction {
	return Instruction{
		Program: associatedTokenProgram,
		Accounts: []AccountMeta{
			{Pubkey: payer, Signer: true, Writable: true},
			{Pubkey: account, Writable: true},
			{Pubkey: owner},
			{Pubkey: mint},
			{Pubkey: systemProgram},
			{Pubkey: tokenProgram},
		},
		Data: []byte{1}, // CreateIdempotent
	}
}

// TransferChecked moves amount base units of mint from one token account to
// another; authority owns the source. The token program checks decimals.
func TransferChecked(source, mint, destination, authority string, amount uint64, decimals uint8) Instruction {
	data := make([]byte, 10)
	data[0] = 12 // TransferChecked
	binary.LittleEndian.PutUint64(data[1:9], amount)
	data[9] = decimals
	return Instruction{
		Program: tokenProgram,
		Accounts: []AccountMeta{
			{Pubkey: source, Writable: true},
			{Pubkey: mint},
			{Pubkey: destination, Writable: true},
			{Pubkey: authority, Signer: true},
		},
		Data: data,
	}
}

// SignedTransaction builds a legacy transaction whose only signer, and fee
// payer, is key, and signs it. It returns the wire bytes and the signature,
// which is the transaction's id.
func SignedTransaction(key ed25519.PrivateKey, recentBlockhash string, instructions []Instruction) (tx []byte, signature string, err error) {
	payer := EncodeBase58(key.Public().(ed25519.PublicKey))
	message, err := compile(payer, recentBlockhash, instructions)
	if err != nil {
		return nil, "", err
	}
	sig := ed25519.Sign(key, message)
	tx = appendCompactU16(nil, 1)
	tx = append(tx, sig...)
	tx = append(tx, message...)
	return tx, EncodeBase58(sig), nil
}

// compile serializes a legacy message. Accounts are ordered the way the
// runtime requires: the fee payer first, then writable signers, read-only
// signers, writable non-signers and read-only non-signers.
func compile(payer, recentBlockhash string, instructions []Instruction) ([]byte, error) {
	type account struct {
		key              string
		signer, writable bool
	}
	var accounts []account
	add := func(key string, signer, writable bool) {
		for i := range accounts {
			if accounts[i].key == key {
				accounts[i].signer = accounts[i].signer || signer
				accounts[i].writable = accounts[i].writable || writable
				return
			}
		}
		accounts = append(accounts, account{key, signer, writable})
	}
	add(payer, true, true)
	for _, in := range instructions {
		for _, a := range in.Accounts {
			add(a.Pubkey, a.Signer, a.Writable)
		}
		add(in.Program, false, false)
	}
	rank := func(a account) int {
		switch {
		case a.key == payer:
			return 0
		case a.signer && a.writable:
			return 1
		case a.signer:
			return 2
		case a.writable:
			return 3
		}
		return 4
	}
	slices.SortStableFunc(accounts, func(a, b account) int { return rank(a) - rank(b) })

	var signers, readonlySigners, readonlyUnsigned byte
	index := map[string]byte{}
	keys := make([]byte, 0, 32*len(accounts))
	for i, a := range accounts {
		k, ok := DecodeBase58(a.key)
		if !ok || len(k) != 32 {
			return nil, fmt.Errorf("%q is not a Solana public key", a.key)
		}
		keys = append(keys, k...)
		index[a.key] = byte(i)
		switch {
		case a.signer && !a.writable:
			signers++
			readonlySigners++
		case a.signer:
			signers++
		case !a.writable:
			readonlyUnsigned++
		}
	}
	blockhash, ok := DecodeBase58(recentBlockhash)
	if !ok || len(blockhash) != 32 {
		return nil, fmt.Errorf("%q is not a blockhash", recentBlockhash)
	}

	msg := []byte{signers, readonlySigners, readonlyUnsigned}
	msg = appendCompactU16(msg, len(accounts))
	msg = append(msg, keys...)
	msg = append(msg, blockhash...)
	msg = appendCompactU16(msg, len(instructions))
	for _, in := range instructions {
		msg = append(msg, index[in.Program])
		msg = appendCompactU16(msg, len(in.Accounts))
		for _, a := range in.Accounts {
			msg = append(msg, index[a.Pubkey])
		}
		msg = appendCompactU16(msg, len(in.Data))
		msg = append(msg, in.Data...)
	}
	return msg, nil
}

// appendCompactU16 appends n in Solana's shortvec encoding: 7 bits per byte,
// low bits first, the high bit set while more bytes follow.
func appendCompactU16(b []byte, n int) []byte {
	for {
		c := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}
