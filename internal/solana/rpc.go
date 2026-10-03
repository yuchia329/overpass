package solana

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// RPC is a minimal Solana JSON-RPC client. Every read uses `confirmed` commitment.
type RPC struct {
	url    string
	client *http.Client
}

func NewRPC(url string, client *http.Client) *RPC {
	return &RPC{url: url, client: client}
}

// SignatureInfo is one entry from getSignaturesForAddress.
type SignatureInfo struct {
	Signature string          `json:"signature"`
	Err       json.RawMessage `json:"err"` // null when the transaction succeeded
}

// Failed reports whether the transaction failed on chain.
func (s SignatureInfo) Failed() bool {
	return len(s.Err) > 0 && string(s.Err) != "null"
}

// Transaction is the part of a getTransaction result the backend reads.
type Transaction struct {
	Meta struct {
		PreTokenBalances  []TokenBalance `json:"preTokenBalances"`
		PostTokenBalances []TokenBalance `json:"postTokenBalances"`
	} `json:"meta"`
}

// TokenBalance is a token account's balance before or after a transaction.
type TokenBalance struct {
	Mint          string `json:"mint"`
	Owner         string `json:"owner"`
	UITokenAmount struct {
		Amount string `json:"amount"` // base units, as a decimal string
	} `json:"uiTokenAmount"`
}

// SignaturesForAddress lists address's newest 1000 transaction signatures, newest first.
func (c *RPC) SignaturesForAddress(ctx context.Context, address string) ([]SignatureInfo, error) {
	var out []SignatureInfo
	err := c.call(ctx, "getSignaturesForAddress", []any{address, map[string]any{"commitment": "confirmed"}}, &out)
	return out, err
}

// Transaction fetches a transaction. It returns nil if the node does not have it yet.
func (c *RPC) Transaction(ctx context.Context, signature string) (*Transaction, error) {
	var out *Transaction
	err := c.call(ctx, "getTransaction", []any{signature, map[string]any{
		"commitment":                     "confirmed",
		"encoding":                       "json",
		"maxSupportedTransactionVersion": 0,
	}}, &out)
	return out, err
}

// AccountExists reports whether an account holds any lamports.
func (c *RPC) AccountExists(ctx context.Context, address string) (bool, error) {
	var out struct {
		Value json.RawMessage `json:"value"`
	}
	err := c.call(ctx, "getAccountInfo", []any{address, map[string]any{"commitment": "confirmed", "encoding": "base64"}}, &out)
	return len(out.Value) > 0 && string(out.Value) != "null", err
}

// LatestBlockhash returns a recent blockhash and the last block height at
// which a transaction built on it can still land.
func (c *RPC) LatestBlockhash(ctx context.Context) (blockhash string, lastValidBlockHeight uint64, err error) {
	var out struct {
		Value struct {
			Blockhash            string `json:"blockhash"`
			LastValidBlockHeight uint64 `json:"lastValidBlockHeight"`
		} `json:"value"`
	}
	err = c.call(ctx, "getLatestBlockhash", []any{map[string]any{"commitment": "confirmed"}}, &out)
	return out.Value.Blockhash, out.Value.LastValidBlockHeight, err
}

// BlockHeight returns the current block height.
func (c *RPC) BlockHeight(ctx context.Context) (uint64, error) {
	var out uint64
	err := c.call(ctx, "getBlockHeight", []any{map[string]any{"commitment": "confirmed"}}, &out)
	return out, err
}

// SendTransaction submits a signed transaction after the node simulates it.
// An *RPCError means the node refused it, but the caller cannot rule out
// that it was forwarded anyway: only its status, or its blockhash expiring,
// settles that.
func (c *RPC) SendTransaction(ctx context.Context, tx []byte) error {
	var sig string
	return c.call(ctx, "sendTransaction", []any{base64.StdEncoding.EncodeToString(tx), map[string]any{
		"encoding":            "base64",
		"preflightCommitment": "confirmed",
	}}, &sig)
}

// SignatureStatus is where a transaction stands. A nil *SignatureStatus means
// the cluster has not seen it.
type SignatureStatus struct {
	ConfirmationStatus string          `json:"confirmationStatus"` // processed, confirmed or finalized
	Err                json.RawMessage `json:"err"`                // null when the transaction succeeded
}

// Failed reports whether the transaction failed on chain.
func (s SignatureStatus) Failed() bool {
	return len(s.Err) > 0 && string(s.Err) != "null"
}

// Confirmed reports whether a supermajority of the cluster has voted on it.
func (s SignatureStatus) Confirmed() bool {
	return s.ConfirmationStatus == "confirmed" || s.ConfirmationStatus == "finalized"
}

// Status looks a transaction up, in the node's full history too.
func (c *RPC) Status(ctx context.Context, signature string) (*SignatureStatus, error) {
	var out struct {
		Value []*SignatureStatus `json:"value"`
	}
	err := c.call(ctx, "getSignatureStatuses", []any{[]string{signature}, map[string]any{"searchTransactionHistory": true}}, &out)
	if err != nil || len(out.Value) == 0 {
		return nil, err
	}
	return out.Value[0], nil
}

// RPCError is an error the node returned, as opposed to a transport failure.
type RPCError struct {
	Method  string
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("%s: RPC error %d: %s", e.Method, e.Code, e.Message)
}

func (c *RPC) call(ctx context.Context, method string, params []any, result any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("%s: HTTP %d: %s", method, res.StatusCode, bytes.TrimSpace(msg))
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("%s: decode response: %w", method, err)
	}
	if envelope.Error != nil {
		return &RPCError{Method: method, Code: envelope.Error.Code, Message: envelope.Error.Message}
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return fmt.Errorf("%s: decode result: %w", method, err)
	}
	return nil
}
