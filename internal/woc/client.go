// Package woc implements the WhatsonChain API surface used by km.
//
// Only the operations the CLI needs are implemented: listing unspent outputs,
// broadcasting exact raw transactions and (later) fetching transactions. The
// base URL is derived from the configured BSV network; the API key is optional.
package woc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ErrNotFound reports that WhatsonChain does not know a requested transaction
// (not yet broadcast, dropped, or never accepted).
var ErrNotFound = errors.New("woc: transaction not found")

// UTXO is one unspent transaction output of the wallet address.
type UTXO struct {
	TxID   string
	Vout   uint32
	Value  uint64
	Height int64
}

var txidPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Client talks to one network's WhatsonChain endpoint.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New creates a client for the given API base URL (for example
// https://api.whatsonchain.com/v1/bsv/test). apiKey may be empty.
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// BaseURL returns the configured API base URL.
func (c *Client) BaseURL() string { return c.baseURL }

type unspentEntry struct {
	Height           int64  `json:"height"`
	TxPos            uint32 `json:"tx_pos"`
	TxHash           string `json:"tx_hash"`
	Value            uint64 `json:"value"`
	SpentInMempoolTx bool   `json:"isSpentInMempoolTx"`
}

// ListUnspent returns the current unspent outputs for address.
func (c *Client) ListUnspent(ctx context.Context, address string) ([]UTXO, error) {
	if address == "" {
		return nil, fmt.Errorf("woc: address is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/address/"+url.PathEscape(address)+"/unspent", nil)
	if err != nil {
		return nil, err
	}
	c.authorize(request)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("woc: list unspent: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("woc: read unspent response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("woc: list unspent status %d: %s", response.StatusCode, snippet(body))
	}
	var entries []unspentEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("woc: invalid unspent response: %w", err)
	}
	utxos := make([]UTXO, 0, len(entries))
	for _, entry := range entries {
		if !txidPattern.MatchString(entry.TxHash) {
			return nil, fmt.Errorf("woc: invalid txid in unspent response")
		}
		// Outputs already spent by a mempool transaction must not be selected:
		// broadcasting a conflicting spend only yields txn-mempool-conflict.
		if entry.SpentInMempoolTx {
			continue
		}
		utxos = append(utxos, UTXO{TxID: entry.TxHash, Vout: entry.TxPos, Value: entry.Value, Height: entry.Height})
	}
	return utxos, nil
}

// ChainHeight returns the current best block height.
func (c *Client) ChainHeight(ctx context.Context) (uint32, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/chain/info", nil)
	if err != nil {
		return 0, err
	}
	c.authorize(request)
	response, err := c.http.Do(request)
	if err != nil {
		return 0, fmt.Errorf("woc: chain info: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("woc: read chain info: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("woc: chain info status %d: %s", response.StatusCode, snippet(body))
	}
	var document struct {
		Blocks uint32 `json:"blocks"`
	}
	if err := json.Unmarshal(body, &document); err != nil || document.Blocks == 0 {
		return 0, fmt.Errorf("woc: chain info is invalid")
	}
	return document.Blocks, nil
}

// TransactionRaw fetches the exact raw transaction bytes for txid. A 404 is
// reported as ErrNotFound so recovery code can keep waiting instead of
// treating an unobserved transaction as failure.
func (c *Client) TransactionRaw(ctx context.Context, txid string) ([]byte, error) {
	if !txidPattern.MatchString(txid) {
		return nil, fmt.Errorf("woc: invalid txid %q", txid)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/tx/"+txid+"/hex", nil)
	if err != nil {
		return nil, err
	}
	c.authorize(request)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("woc: fetch transaction: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("woc: read transaction response: %w", err)
	}
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("woc: fetch transaction status %d: %s", response.StatusCode, snippet(body))
	}
	hexText := strings.TrimSpace(string(body))
	hexText = strings.Trim(hexText, "\"")
	raw, err := hex.DecodeString(hexText)
	if err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("woc: transaction response is not raw hex")
	}
	return raw, nil
}

func (c *Client) authorize(request *http.Request) {
	if c.apiKey != "" {
		request.Header.Set("woc-api-key", c.apiKey)
	}
}

func snippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		return text[:200] + "…"
	}
	return text
}
