package solana_test

import (
	"testing"

	"github.com/yuchia329/unstuck/internal/solana"
)

// Token accounts observed on mainnet, each created by the Associated Token Account program.
func TestAssociatedTokenAddressMatchesMainnet(t *testing.T) {
	cases := []struct{ owner, want string }{
		{"2Qemdsc7rwW9SFDD2HbjXVSLmn9xo54sT7m9FFGCHJ7b", "3kUZH41UVMh5DYHmWfdByTrQgwM97jnGZZt2kdBv8WUK"},
		{"BcdwLA62UPEAvRn7AWauMUXKtYMXxdLzTPaSQg5tNaFc", "7SyaSKPkVFrzAgmBCR64ydpLwcvehkCwJKom8NCF4WGM"},
		{"8uVMTVTmS159ytsCTJF5RDT2kQzb8oBozPpHk9TjEjDa", "FK8EhQvcGpb2YgjCYqHMv1zs3GX8t4GFBtpANehUTZwX"},
	}
	for _, c := range cases {
		got, err := solana.AssociatedTokenAddress(c.owner, solana.USDCMint)
		if err != nil {
			t.Fatalf("%s: %v", c.owner, err)
		}
		if got != c.want {
			t.Errorf("ATA of %s = %s, want %s", c.owner, got, c.want)
		}
	}
}

func TestAssociatedTokenAddressRejectsInvalidOwner(t *testing.T) {
	if _, err := solana.AssociatedTokenAddress("not-a-key", solana.USDCMint); err == nil {
		t.Error("want error for invalid owner")
	}
}
