// Command unstuck-register registers a Customer by signing the registration
// challenge with a Solana keypair file, for wallets (like pay.sh accounts) that
// cannot sign messages themselves. It prints the API key.
//
// Export a pay.sh account with: pay account export <name> <path>
// The keypair file is the standard Solana JSON array of 64 bytes. Keep it out of the repo.
package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/yuchia329/unstuck/internal/solana"
)

func main() {
	server := flag.String("server", "http://localhost:8080", "Unstuck backend URL")
	keypairPath := flag.String("keypair", "", "Solana keypair JSON file (64-byte array)")
	flag.Parse()
	if *keypairPath == "" {
		log.Fatal("-keypair is required")
	}

	key, err := solana.ReadKeypair(*keypairPath)
	if err != nil {
		log.Fatal(err)
	}
	wallet := solana.EncodeBase58(key.Public().(ed25519.PublicKey))

	var challenge struct {
		Nonce   string `json:"nonce"`
		Message string `json:"message"`
	}
	if err := post(*server+"/v1/customers/challenge", map[string]string{"wallet": wallet}, &challenge); err != nil {
		log.Fatalf("challenge: %v", err)
	}
	fmt.Fprintf(os.Stderr, "signing as %s:\n%s\n\n", wallet, challenge.Message)

	var registered struct {
		CustomerID string `json:"customer_id"`
		APIKey     string `json:"api_key"`
	}
	err = post(*server+"/v1/customers", map[string]string{
		"wallet":    wallet,
		"nonce":     challenge.Nonce,
		"signature": solana.EncodeBase58(ed25519.Sign(key, []byte(challenge.Message))),
	}, &registered)
	if err != nil {
		log.Fatalf("register: %v", err)
	}
	fmt.Fprintf(os.Stderr, "customer %s\n", registered.CustomerID)
	fmt.Println(registered.APIKey)
}

func post(url string, body, out any) error {
	b, _ := json.Marshal(body)
	res, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&e)
		return fmt.Errorf("status %d: %s", res.StatusCode, e.Error)
	}
	return json.NewDecoder(res.Body).Decode(out)
}
