package wallet

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/bsv8/keymaster.cc-cli/internal/woc"
	manager "github.com/bsv8/keymaster.cc-cli/utxo"
)

type spentReader interface {
	GetSpentOutput(context.Context, string, uint32) (*woc.SpentOutput, error)
}
type batchSpentReader interface {
	GetSpentOutputs(context.Context, []woc.Outpoint) (*woc.SpentBatch, error)
}
type contextInputObserver struct {
	ctx        context.Context
	single     spentReader
	batch      batchSpentReader
	diagnostic *string
}

func inputObserverFor(ctx context.Context, chain Chain, diagnostic *string) manager.InputObserver {
	single, ok := chain.(spentReader)
	if !ok {
		return nil
	}
	batch, _ := chain.(batchSpentReader)
	return contextInputObserver{ctx: ctx, single: single, batch: batch, diagnostic: diagnostic}
}
func (o contextInputObserver) ObserveInputs(keys []string) ([]manager.InputObservation, error) {
	points := make([]woc.Outpoint, 0, len(keys))
	for _, key := range keys {
		parts := strings.Split(key, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("wallet: invalid input outpoint")
		}
		vout, err := strconv.ParseUint(parts[1], 10, 32)
		if err != nil {
			return nil, err
		}
		points = append(points, woc.Outpoint{TxID: parts[0], Vout: uint32(vout)})
	}
	facts := []manager.InputObservation{}
	diagnostics := []any{}
	for start := 0; start < len(points); start += 20 {
		end := start + 20
		if end > len(points) {
			end = len(points)
		}
		known := map[woc.Outpoint]*woc.SpentOutput{}
		if o.batch != nil && o.ctx.Err() == nil {
			batch, err := o.batch.GetSpentOutputs(o.ctx, points[start:end])
			diagnostics = append(diagnostics, map[string]any{"batch": batch, "error": errorText(err)})
			if err == nil && batch != nil {
				for _, row := range batch.Results {
					if row.Status == "spent" && row.Spender != nil {
						known[row.Outpoint] = row.Spender
					}
				}
			}
		}
		for i := start; i < end; i++ {
			fact := manager.InputObservation{Outpoint: keys[i], Status: "unknown"}
			spender, ok := known[points[i]]
			if !ok && o.ctx.Err() == nil {
				value, err := o.single.GetSpentOutput(o.ctx, points[i].TxID, points[i].Vout)
				diagnostics = append(diagnostics, map[string]any{"outpoint": keys[i], "spender": value, "error": errorText(err)})
				if err == nil {
					if value == nil {
						fact.Status = "unspent"
					} else {
						spender = value
					}
				}
			}
			if spender != nil && (spender.Status == "confirmed" || spender.Status == "unconfirmed") {
				vin := spender.Vin
				confirmed := spender.Status == "confirmed"
				fact.Status = "spent"
				fact.TxID = spender.TxID
				fact.Vin = &vin
				fact.Confirmed = &confirmed
			}
			facts = append(facts, fact)
		}
	}
	if o.diagnostic != nil {
		encoded, _ := json.Marshal(map[string]any{"previous": *o.diagnostic, "spent": diagnostics})
		*o.diagnostic = string(encoded)
	}
	return facts, nil
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
