// Command overpass runs the Overpass backend.
package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yuchia329/overpass/internal/api"
	"github.com/yuchia329/overpass/internal/solana"
)

func main() {
	var cfg api.Config
	addr := flag.String("addr", ":8080", "HTTP listen address")
	flag.StringVar(&cfg.DBPath, "db", "overpass.db", "SQLite database path")
	flag.DurationVar(&cfg.ClaimWindow, "claim-window", 60*time.Second, "how long a Task may wait in the Queue before it Expires (demo: 30s)")
	flag.DurationVar(&cfg.SolveWindow, "solve-window", 120*time.Second, "how long a Solver has after Claim before the Task Fails (demo: 60s)")
	flag.Int64Var(&cfg.Price, "price", 10_000, "USDC base units held per Task (10000 = 0.01 USDC)")
	flag.StringVar(&cfg.ServiceWallet, "service-wallet", "CW82aTEMcqsqwLaxppzrpEnM41bC83R8JUXpZgYcrhGt", "Overpass service wallet public key")
	flag.DurationVar(&cfg.ChallengeTTL, "challenge-ttl", 5*time.Minute, "how long a registration challenge can be signed")
	flag.BoolVar(&cfg.DevMode, "dev", false, "enable the dev credit endpoint (POST /v1/dev/credit)")
	flag.StringVar(&cfg.RPCURL, "rpc-url", "https://api.mainnet-beta.solana.com", "Solana JSON-RPC endpoint polled for Deposits (empty disables polling)")
	flag.DurationVar(&cfg.PollInterval, "poll-interval", 5*time.Second, "how often to poll for Deposits")
	flag.DurationVar(&cfg.PingInterval, "ping-interval", 20*time.Second, "how often to ping Bridge and Queue sockets so proxies keep them open (0 disables)")
	stun := flag.String("stun", "stun:stun.l.google.com:19302", "comma-separated STUN URLs for direct Bridge-to-Solver connections (empty for none)")
	turn := flag.String("turn", "", "comma-separated TURN URLs, e.g. turn:host:3478; needs $OVERPASS_TURN_SECRET, coturn's static-auth-secret")
	payoutKeypair := flag.String("payout-keypair", "", "Solana keypair file of the hot wallet that pays Solvers' Withdrawals (empty disables Withdrawals)")
	flag.StringVar(&cfg.USDCMint, "usdc-mint", solana.USDCMint, "USDC mint that Withdrawals pay in")
	flag.Int64Var(&cfg.MinWithdrawal, "min-withdrawal", 100_000, "least USDC base units a Solver receives per Withdrawal (100000 = 0.10 USDC)")
	flag.Int64Var(&cfg.AccountFee, "account-fee", 400_000, "USDC base units kept back from a Withdrawal to a wallet with no USDC token account, for its rent")
	flag.Parse()
	cfg.STUNURLs, cfg.TURNURLs = urlList(*stun), urlList(*turn)
	cfg.TURNSecret = os.Getenv("OVERPASS_TURN_SECRET")
	if *payoutKeypair != "" {
		key, err := solana.ReadKeypair(*payoutKeypair)
		if err != nil {
			log.Fatal(err)
		}
		cfg.PayoutKey, cfg.PayoutRPCURL = key, cfg.RPCURL
	}

	srv, err := api.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	httpSrv := &http.Server{Addr: *addr, Handler: srv}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()

	log.Printf("overpass listening on %s (claim %v, solve %v, price %d, dev %v, stun %v, turn %v)",
		*addr, cfg.ClaimWindow, cfg.SolveWindow, cfg.Price, cfg.DevMode, cfg.STUNURLs, cfg.TURNURLs)
	if cfg.PayoutKey != nil {
		log.Printf("withdrawals paid from hot wallet %s in mint %s (minimum %d, account fee %d)",
			solana.EncodeBase58(cfg.PayoutKey.Public().(ed25519.PublicKey)), cfg.USDCMint, cfg.MinWithdrawal, cfg.AccountFee)
	}
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-drained // in-flight requests finish before the database closes
	if err := srv.Close(); err != nil {
		log.Printf("close: %v", err)
	}
}

// urlList splits a comma-separated flag value, dropping empty entries.
func urlList(s string) []string {
	var urls []string
	for _, u := range strings.Split(s, ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	return urls
}
