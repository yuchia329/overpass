// Command overpass runs the Overpass backend.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yuchia329/overpass/internal/api"
)

func main() {
	var cfg api.Config
	addr := flag.String("addr", ":8080", "HTTP listen address")
	flag.StringVar(&cfg.DBPath, "db", "overpass.db", "SQLite database path")
	flag.DurationVar(&cfg.ClaimWindow, "claim-window", 60*time.Second, "how long a Task may wait in the Queue before it Expires (demo: 30s)")
	flag.DurationVar(&cfg.SolveWindow, "solve-window", 120*time.Second, "how long a Solver has after Claim before the Task Fails (demo: 45s)")
	flag.Int64Var(&cfg.Price, "price", 10_000, "USDC base units held per Task (10000 = 0.01 USDC)")
	flag.StringVar(&cfg.ServiceWallet, "service-wallet", "CW82aTEMcqsqwLaxppzrpEnM41bC83R8JUXpZgYcrhGt", "Overpass service wallet public key")
	flag.DurationVar(&cfg.ChallengeTTL, "challenge-ttl", 5*time.Minute, "how long a registration challenge can be signed")
	flag.BoolVar(&cfg.DevMode, "dev", false, "enable the dev credit endpoint (POST /v1/dev/credit)")
	flag.StringVar(&cfg.RPCURL, "rpc-url", "https://api.mainnet-beta.solana.com", "Solana JSON-RPC endpoint polled for Deposits (empty disables polling)")
	flag.DurationVar(&cfg.PollInterval, "poll-interval", 5*time.Second, "how often to poll for Deposits")
	flag.Parse()

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

	log.Printf("overpass listening on %s (claim %v, solve %v, price %d, dev %v)",
		*addr, cfg.ClaimWindow, cfg.SolveWindow, cfg.Price, cfg.DevMode)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-drained // in-flight requests finish before the database closes
	if err := srv.Close(); err != nil {
		log.Printf("close: %v", err)
	}
}
