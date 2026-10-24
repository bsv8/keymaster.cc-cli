package woc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
)

type Outpoint struct {
	TxID string `json:"txid"`
	Vout uint32 `json:"vout"`
}
type SpentOutput struct {
	TxID   string `json:"txid"`
	Vin    uint32 `json:"vin"`
	Status string `json:"status"`
}
type SpentResult struct {
	Outpoint Outpoint     `json:"outpoint"`
	Status   string       `json:"status"`
	Spender  *SpentOutput `json:"spender,omitempty"`
}
type SpentBatch struct {
	Results    []SpentResult `json:"results"`
	HTTPStatus int           `json:"httpStatus"`
	Body       string        `json:"body"`
}
type SpentQueryError struct {
	HTTPStatus int
	Body       string
}

func (e *SpentQueryError) Error() string {
	return fmt.Sprintf("woc: spent status %d: %s", e.HTTPStatus, e.Body)
}
func spentIndex(raw json.RawMessage) (uint32, bool) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	number, ok := value.(float64)
	if !ok || number < 0 || number > math.MaxUint32 || math.Trunc(number) != number {
		return 0, false
	}
	return uint32(number), true
}

func parseSpentOutput(raw json.RawMessage) (*SpentOutput, bool) {
	var value struct {
		TxID   string          `json:"txid"`
		Vin    json.RawMessage `json:"vin"`
		Status string          `json:"status"`
	}
	if json.Unmarshal(raw, &value) != nil || !txidPattern.MatchString(value.TxID) || (value.Status != "confirmed" && value.Status != "unconfirmed") {
		return nil, false
	}
	vin, valid := spentIndex(value.Vin)
	if !valid {
		return nil, false
	}
	return &SpentOutput{TxID: value.TxID, Vin: vin, Status: value.Status}, true
}
func (c *Client) spentRequest(ctx context.Context, method, path string, payload []byte) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	if method == http.MethodPost {
		request.Header.Set("content-type", "application/json")
	}
	c.authorize(request)
	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	const max = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, max+1))
	if err != nil {
		return response.StatusCode, body, err
	}
	if len(body) > max {
		return response.StatusCode, body[:max], fmt.Errorf("woc: spent response exceeds limit")
	}
	return response.StatusCode, body, nil
}

// GetSpentOutput: only a completed 404 on this exact endpoint means unspent.
// HTTP400/Unknown UTXO/timeout/malformed 200 are errors, never unspent.
func (c *Client) GetSpentOutput(ctx context.Context, txid string, vout uint32) (*SpentOutput, error) {
	if !txidPattern.MatchString(txid) {
		return nil, fmt.Errorf("woc: invalid spent outpoint")
	}
	status, body, err := c.spentRequest(ctx, http.MethodGet, fmt.Sprintf("/tx/%s/%d/spent", txid, vout), nil)
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return nil, nil
	}
	if status != 200 {
		return nil, &SpentQueryError{HTTPStatus: status, Body: string(body)}
	}
	value, valid := parseSpentOutput(body)
	if !valid {
		return nil, &SpentQueryError{HTTPStatus: status, Body: string(body)}
	}
	return value, nil
}

// GetSpentOutputs does one batch (1..20), binds each row to its outpoint and
// deliberately never infers unspent from an empty error or Unknown UTXO.
func (c *Client) GetSpentOutputs(ctx context.Context, points []Outpoint) (*SpentBatch, error) {
	if len(points) == 0 || len(points) > 20 {
		return nil, fmt.Errorf("woc: spent batch must contain 1..20 outpoints")
	}
	positions := map[Outpoint]int{}
	results := []SpentResult{}
	for i, p := range points {
		if !txidPattern.MatchString(p.TxID) {
			return nil, fmt.Errorf("woc: invalid spent outpoint")
		}
		if _, ok := positions[p]; ok {
			return nil, fmt.Errorf("woc: duplicate spent outpoint")
		}
		positions[p] = i
		results = append(results, SpentResult{Outpoint: p, Status: "unknown"})
	}
	payload, _ := json.Marshal(map[string]any{"utxos": points})
	status, body, err := c.spentRequest(ctx, http.MethodPost, "/utxos/spent", payload)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, &SpentQueryError{HTTPStatus: status, Body: string(body)}
	}
	var rows []struct {
		UTXO *struct {
			TxID string          `json:"txid"`
			Vout json.RawMessage `json:"vout"`
		} `json:"utxo"`
		SpentIn json.RawMessage `json:"spentIn"`
		Error   *string         `json:"error"`
	}
	if json.Unmarshal(body, &rows) != nil || rows == nil {
		return nil, &SpentQueryError{HTTPStatus: status, Body: string(body)}
	}
	seen := map[Outpoint]bool{}
	for _, row := range rows {
		if row.UTXO == nil {
			return nil, &SpentQueryError{HTTPStatus: status, Body: string(body)}
		}
		vout, valid := spentIndex(row.UTXO.Vout)
		if !valid {
			return nil, &SpentQueryError{HTTPStatus: status, Body: string(body)}
		}
		point := Outpoint{TxID: row.UTXO.TxID, Vout: vout}
		index, ok := positions[point]
		if !ok || seen[point] {
			return nil, &SpentQueryError{HTTPStatus: status, Body: string(body)}
		}
		seen[point] = true
		if row.Error != nil && *row.Error == "" {
			if spender, valid := parseSpentOutput(row.SpentIn); valid {
				results[index].Status = "spent"
				results[index].Spender = spender
			}
		}
	}
	return &SpentBatch{Results: results, HTTPStatus: status, Body: string(body)}, nil
}
