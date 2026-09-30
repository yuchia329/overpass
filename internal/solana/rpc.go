package solana

import (
	"bytes"
	"context"
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

// SignaturesForAddress lists address's transaction signatures, newest first,
// stopping before until (all recent ones when until is empty).
func (c *RPC) SignaturesForAddress(ctx context.Context, address, until string) ([]SignatureInfo, error) {
	opts := map[string]any{"commitment": "confirmed"}
	if until != "" {
		opts["until"] = until
	}
	var out []SignatureInfo
	err := c.call(ctx, "getSignaturesForAddress", []any{address, opts}, &out)
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
		return fmt.Errorf("%s: RPC error %d: %s", method, envelope.Error.Code, envelope.Error.Message)
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return fmt.Errorf("%s: decode result: %w", method, err)
	}
	return nil
}
