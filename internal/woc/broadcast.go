package woc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// BroadcastReceipt is a host HTTP diagnostic, not a manager-specific state.
// Conflict requires per-input spent observations; unknown never permits release.
type BroadcastReceipt struct {
	Status         string `json:"status"` // accepted, already-known, conflict, unknown
	TxID           string `json:"txid"`
	HTTPStatus     int    `json:"httpStatus"`
	HTTPStatusText string `json:"httpStatusText"`
	Body           string `json:"body"`
	BodyTruncated  bool   `json:"bodyTruncated,omitempty"`
}

type BroadcastError struct{ Receipt BroadcastReceipt }

func (e *BroadcastError) Error() string {
	return fmt.Sprintf("woc: broadcast %s status %d: %s", e.Receipt.Status, e.Receipt.HTTPStatus, e.Receipt.Body)
}

// BroadcastExact interprets only the exact response to this POST. HTTP/body
// interpretation stays in the host; the common manager consumes the outcome.
func (c *Client) BroadcastExact(ctx context.Context, rawHex string) (BroadcastReceipt, error) {
	raw, err := hex.DecodeString(rawHex)
	if err != nil || len(raw) == 0 {
		return BroadcastReceipt{}, fmt.Errorf("woc: raw transaction must be nonempty hex")
	}
	first := sha256.Sum256(raw)
	second := sha256.Sum256(first[:])
	for i, j := 0, len(second)-1; i < j; i, j = i+1, j-1 {
		second[i], second[j] = second[j], second[i]
	}
	result := BroadcastReceipt{Status: "unknown", TxID: hex.EncodeToString(second[:])}
	payload, err := json.Marshal(map[string]string{"txhex": rawHex})
	if err != nil {
		return result, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/tx/raw", bytes.NewReader(payload))
	if err != nil {
		return result, err
	}
	request.Header.Set("content-type", "application/json")
	c.authorize(request)
	response, err := c.http.Do(request)
	if err != nil {
		return result, fmt.Errorf("woc: broadcast: %w", err)
	}
	defer response.Body.Close()
	result.HTTPStatus = response.StatusCode
	result.HTTPStatusText = strings.TrimPrefix(response.Status, fmt.Sprintf("%d ", response.StatusCode))
	const maxBody = 1 << 20
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if len(body) > maxBody {
		result.BodyTruncated = true
		body = body[:maxBody]
	}
	result.Body = string(body)
	if readErr != nil {
		return result, fmt.Errorf("woc: read broadcast response: %w", readErr)
	}
	if result.BodyTruncated {
		return result, nil
	}
	var text string
	var document struct {
		TxID string `json:"txid"`
	}
	stringReceipt := json.Unmarshal(body, &text) == nil
	txid := text
	if !stringReceipt && json.Unmarshal(body, &document) == nil {
		txid = document.TxID
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 && strings.ToLower(txid) == result.TxID {
		result.Status = "accepted"
	} else if response.StatusCode == 400 && stringReceipt {
		switch text {
		case "unexpected response code 500: Transaction already in the mempool":
			result.Status = "already-known"
		case "unexpected response code 500: 258: txn-mempool-conflict":
			result.Status = "conflict"
		}
	}
	return result, nil
}

// Broadcast preserves the Chain interface while exposing structured unknown or
// conflict diagnosis via errors.As(*BroadcastError). Mempool success uses the
// locally computed canonical txid, never an invented provider txid.
func (c *Client) Broadcast(ctx context.Context, rawHex string) (string, error) {
	receipt, err := c.BroadcastExact(ctx, rawHex)
	if err != nil {
		return "", err
	}
	if receipt.Status == "accepted" || receipt.Status == "already-known" {
		return receipt.TxID, nil
	}
	return "", &BroadcastError{Receipt: receipt}
}
