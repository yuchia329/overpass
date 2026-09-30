# Overpass

Human-in-the-loop unblocking for AI agents. When an Agent's browser hits a
Challenge it cannot pass (reCAPTCHA, a slider puzzle), it calls
`solve(page)`. A human Solver claims the Task on the Queue page, drives the
Agent's browser remotely from their phone or desktop, and clears it. The
Customer pays per Task in USDC from a prepaid Balance. See
[CONTEXT.md](CONTEXT.md) for the glossary.

- `cmd/overpass`: Go backend, which runs the Queue page, the Session relay and the Ledger.
- `cmd/overpass-register`: registers a Customer by signing with a Solana keypair file.
- `bridge/`: TypeScript Bridge SDK (`solve(page)`) plus demo Agents.

## Prerequisites

- Go 1.26+ and Node 22+
- [pay.sh CLI](https://pay.sh) with a funded **local** account (not a
  remote-custody one: it must be exportable)
- ngrok with an authtoken (`ngrok config add-authtoken <token>`)
- Bridge dependencies:

  ```sh
  cd bridge && npm ci && npx playwright install chromium
  ```

## Run the demo: Solver on a phone via ngrok

You need three terminals. All commands run from the repo root unless they
say otherwise.

### 1. Start the backend (terminal 1)

Free port 8080 first if an old server is still running:

```sh
lsof -ti tcp:8080 | xargs kill
```

Then pick one of these two ways to start it.

**With real USDC Deposits.** The server polls Solana mainnet for Deposits
every 5s:

```sh
go run ./cmd/overpass -claim-window 30s -solve-window 60s
```

**Without real USDC.** Deposit polling is off and the dev credit endpoint
is on:

```sh
go run ./cmd/overpass -claim-window 30s -solve-window 60s -dev -rpc-url ""
```

State lives in `overpass.db` in the current directory; change it with
`-db path`. Keep the same file between runs and your registration and
Balance carry over.

The Price is 0.01 USDC (10000 base units) per Task. It is split 80/20:
0.008 USDC is the Solver's Earning and 0.002 USDC is the Fee.

### 2. Register the Customer (once per database)

The Customer proves wallet ownership by signing a challenge. The pay CLI
cannot sign messages, so export the account's keypair and let
`overpass-register` sign:

```sh
pay account export local                      # writes ./pay-account-local-<pubkey>.json
go run ./cmd/overpass-register -keypair ./pay-account-local-*.json
rm ./pay-account-local-*.json                  # it holds the private key
```

It prints the API key. The key is shown only once, so export it in every
terminal that runs an Agent:

```sh
export OVERPASS_API_KEY=op_...
```

Registering the same wallet again issues a new API key and revokes the old
one.

### 3. Fund the Balance

**Real Deposit.** Send USDC from the registered wallet to the service
wallet. The backend sees it within about 5s of confirmation:

```sh
pay send 0.05 CW82aTEMcqsqwLaxppzrpEnM41bC83R8JUXpZgYcrhGt --account local
```

- pay adds a small network fee on top. Pass `--fee-within` to take it out of
  the amount instead.
- Deposits are matched by sender, so send from the wallet you registered.
- A fresh database picks up that wallet's earlier Deposits too, and credits
  them once the wallet registers.

**Dev credit.** This only works if the server was started with `-dev`:

```sh
curl -X POST -H "Authorization: Bearer $OVERPASS_API_KEY" \
  -d '{"amount":100000}' http://localhost:8080/v1/dev/credit
```

Check the Balance (amounts are USDC base units, 1 USDC = 1000000):

```sh
curl -H "Authorization: Bearer $OVERPASS_API_KEY" http://localhost:8080/v1/balance
```

### 4. Open the tunnel (terminal 2)

```sh
ngrok http 8080
```

Copy the `https://….ngrok-free.dev` forwarding URL. On the phone:

1. Open the URL.
2. ngrok's free tier shows a warning page the first time. Tap **Visit Site**.
3. Enter the Solver's Solana wallet address and tap **Connect**. The status
   line should read "Connected as …".

The phone can be on cellular. The Agent keeps talking to
`http://localhost:8080`; only the phone goes through the tunnel.

### 5. Run an Agent (terminal 3)

```sh
cd bridge
npm run demo              # local fake Challenge: button click or slider drag
npm run demo:recaptcha    # Google's reCAPTCHA demo page (default cleared check)
```

A Chromium window opens and the Agent creates a Task. On the phone:

1. The Task appears in the Queue. Tap **Claim** within 30s.
2. The Agent's page appears, with its URL and a solve-window countdown.
3. Clear the Challenge. You can tap, drag the slider, or scroll with a
   mouse wheel on desktop.
4. The Agent's cleared check passes, and the Bridge reports Solved. The phone
   shows "Solved! Earning of 0.008 USDC recorded.", the Agent continues, and
   the Hold is captured.

If the phone's connection drops, reopen the URL and connect with the same
wallet before the solve window ends. The Session resumes.

Other options:

- Run the Agent without a window: `HEADLESS=1 npm run demo`.
- Point the Agent at another backend: `OVERPASS_URL=https://… npm run demo`.

## Run the demo against the public backend

The backend runs on the `hubstream` EC2 instance at
`https://overpass.yuchia.dev`. The Agent stays on the laptop and the Solver
uses a phone or iPad, both over the public internet.

How it is wired:

- Cloudflare proxies `*.yuchia.dev` (wildcard DNS) and terminates TLS. Its
  SSL mode must be **Full**, not Flexible or Full (strict): it connects to
  the instance's k3s Traefik over TLS, and Traefik serves its default
  self-signed certificate. The Ingress in
  [deploy/ingress.yaml](deploy/ingress.yaml) is on Traefik's `websecure`
  entrypoint only. With no plain HTTP route, the API key and session token
  never cross the internet in cleartext.
- The backend runs under systemd ([deploy/overpass.service](deploy/overpass.service))
  on the k3s pod bridge address `10.42.0.1:8080`. Traefik, the host and other
  pods on the cluster can reach it; the internet cannot.
  The SQLite file is at `/var/lib/overpass/overpass.db`.
- The backend never runs with `-dev`, so `POST /v1/dev/credit` returns 404.
  Balance comes only from real mainnet Deposits.
- The backend pings every Bridge and Queue socket every 20s
  (`-ping-interval`). Cloudflare closes WebSockets that are idle for 100s,
  and a Challenge page that sits still sends no frames.
- Traefik's access log is off, so the session token in the Bridge's URL is
  not logged on the instance. Keep it off if you change Traefik's config.
- The instance must already run k3s with its bundled Traefik, and its pod
  bridge `cni0` must be `10.42.0.1`, the k3s default.

### 1. Deploy

```sh
deploy/deploy.sh hubstream
```

This cross-compiles `cmd/overpass` for the instance, installs the binary and
unit, restarts the service and applies the Ingress. Rerunning it keeps the
database. Follow the logs with
`ssh hubstream journalctl -u overpass -f`.

### 2. Register the Customer against the public URL (once per database)

```sh
pay account export local
go run ./cmd/overpass-register -server https://overpass.yuchia.dev -keypair ./pay-account-local-*.json
rm ./pay-account-local-*.json
export OVERPASS_API_KEY=op_...
```

The instance has its own database. On registration it credits every earlier
Deposit from that wallet, including ones already spent against a local
database.

### 3. Fund the Balance

Make a real Deposit as in [step 3 above](#3-fund-the-balance) and check it:

```sh
curl -H "Authorization: Bearer $OVERPASS_API_KEY" https://overpass.yuchia.dev/v1/balance
```

### 4. Run the Agent on the laptop

```sh
cd bridge
OVERPASS_URL=https://overpass.yuchia.dev npm run demo:recaptcha
```

### 5. Solve from the phone or iPad

Open `https://overpass.yuchia.dev`, connect with the Solver's wallet, and
Claim the Task within 30s. The public backend gives the Solver 5 minutes to
clear the Challenge. There is no interstitial page, unlike ngrok.

## Troubleshooting

- **The phone can't reach `http://<laptop-ip>:8080` over Wi-Fi.** Venue and
  office Wi-Fi usually isolates clients, which is why the demo uses ngrok. As
  a fallback, use Tailscale: install the app on the phone, sign in to the same
  account as the laptop, and open `http://<tailscale ip -4>:8080`.
- **The Queue page loads but stays on "Connecting…".** Reload and tap Visit
  Site again. The ngrok cookie may have expired.
- **The Agent fails with `InsufficientBalanceError`.** The available Balance
  is below the Price. Fund it (step 3).
- **The Agent fails with `TaskExpiredError` or `TaskFailedError`.** No Solver
  claimed the Task within the claim window, or the solve window passed. Run
  the Agent again.
- **`address already in use`.** Something else is on 8080. See step 1.

## Tests

```sh
go vet ./... && go test ./...
cd bridge && npm run typecheck && npm test
```
