package wallet

import (
	"fmt"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"strconv"
)

// BindInputs supplies source amounts to the protocol caller. This is transaction
// construction support, not an allocation-manager permission or verification API.
func BindInputs(signedHex string, sourceRaws [][]byte) (map[string]ProtocolProof, error) {
	tx, err := transaction.NewTransactionFromHex(signedHex)
	if err != nil {
		return nil, err
	}
	known := map[string]ProtocolProof{}
	for _, raw := range sourceRaws {
		source, err := transaction.NewTransactionFromBytes(raw)
		if err != nil {
			return nil, err
		}
		for vout, out := range source.Outputs {
			if out.LockingScript != nil {
				known[source.TxID().String()+":"+strconv.Itoa(vout)] = ProtocolProof{Value: out.Satoshis, ScriptHex: out.LockingScript.String()}
			}
		}
	}
	proofs := map[string]ProtocolProof{}
	for _, in := range tx.Inputs {
		key := in.SourceTXID.String() + ":" + strconv.FormatUint(uint64(in.SourceTxOutIndex), 10)
		proof, ok := known[key]
		if !ok {
			return nil, fmt.Errorf("wallet: source output %s unavailable", key)
		}
		proofs[key] = proof
	}
	return proofs, nil
}
