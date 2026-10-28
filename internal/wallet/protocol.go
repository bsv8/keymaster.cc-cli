package wallet

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv8/keymaster.cc-cli/internal/woc"
	manager "github.com/bsv8/keymaster.cc-cli/utxo"
)

// BindInputs binds every input of an exact signed transaction to the outputs
// of caller-supplied exact source transactions (SDK-verified pool funding /
// latest payment bytes). Inputs without an exact known source fail closed;
// the caller must never substitute self-asserted values. The returned proofs
// feed PrepareProtocol.
func BindInputs(signedHex string, sourceRaws [][]byte) (map[string]ProtocolProof, error) {
	spend, err := transaction.NewTransactionFromHex(signedHex)
	if err != nil {
		return nil, fmt.Errorf("wallet: parse protocol transaction: %w", err)
	}
	type outpoint struct {
		value  uint64
		script string
	}
	known := map[string]outpoint{}
	for _, raw := range sourceRaws {
		if len(raw) == 0 {
			continue
		}
		source, err := transaction.NewTransactionFromBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("wallet: parse protocol source: %w", err)
		}
		sourceTxID := source.TxID().String()
		for index, output := range source.Outputs {
			if output.LockingScript == nil {
				continue
			}
			key := fmt.Sprintf("%s:%d", strings.ToLower(sourceTxID), index)
			known[key] = outpoint{value: output.Satoshis, script: hex.EncodeToString(*output.LockingScript)}
		}
	}
	proofs := make(map[string]ProtocolProof, len(spend.Inputs))
	for _, input := range spend.Inputs {
		key := fmt.Sprintf("%s:%d", strings.ToLower(input.SourceTXID.String()), input.SourceTxOutIndex)
		knownOut, ok := known[key]
		if !ok {
			return nil, fmt.Errorf("wallet: protocol input %s has no bound exact source", key)
		}
		proofs[key] = ProtocolProof{Value: knownOut.value, ScriptHex: knownOut.script}
	}
	return proofs, nil
}

// Host policy chooses a fee ceiling through the common manager estimators.
func protocolFeeBounds(req ProtocolRequest, parsed *transaction.Transaction) (string, string, error) {
	feeRateAmount, err := manager.ParseAmount(req.FeeRate, "fee rate")
	if err != nil {
		return "", "", err
	}
	policy := req.FeePolicy
	if policy == "" {
		policy = manager.ProtocolFeePolicyRate
	}
	cap := req.FeeCapSats
	if cap == "" {
		lens := make([]int, 0, len(parsed.Outputs))
		for _, output := range parsed.Outputs {
			if output.LockingScript == nil {
				return "", "", fmt.Errorf("wallet: protocol output has no script")
			}
			lens = append(lens, len(output.LockingScript.Bytes()))
		}
		size, err := manager.EstimateSignedSize(len(parsed.Inputs), lens)
		if err != nil {
			return "", "", err
		}
		protocolSize := uint64(len(req.SignedHex)/2) + uint64(len(parsed.Inputs))*6
		if protocolSize > size {
			size = protocolSize
		}
		fee, err := manager.EstimateFeeSats(feeRateAmount, size)
		if err != nil {
			return "", "", err
		}
		cap = strconv.FormatUint(fee, 10)
	}
	return policy, cap, nil
}

// Reuse an original protocol allocation even after its inputs have settled or
// conflicted. This is a read-only host lookup/validation, not a second prepare,
// signature, snapshot, or provider call. All money conclusions stay in manager.
func (w *Wallet) recoverProtocolPreparationLocked(req ProtocolRequest, parsed *transaction.Transaction, declared []manager.RequestOutput, policy, cap string) (*SpendResult, error) {
	txid, exact := parsed.TxID().String(), strings.ToLower(req.SignedHex)
	var found *manager.OutboxEntry
	for _, entry := range w.core.kernel.OutboxView() {
		if entry.TxID != txid {
			continue
		}
		if found != nil || entry.AllocationID == nil || entry.RawTxHex != exact || entry.Delivery != "wallet" {
			return nil, fmt.Errorf("wallet: inconsistent protocol replay outbox")
		}
		copy := entry
		found = &copy
	}
	if found == nil {
		return nil, nil
	}
	id := *found.AllocationID
	intent := w.core.kernel.ProtocolIntentView(id)
	if intent == nil || intent.Binding == nil || *intent.Binding != (manager.ProtocolBinding{Kind: req.Binding.Kind, PoolID: req.Binding.PoolID}) || intent.SignedTxid == nil || *intent.SignedTxid != txid || intent.FeeCapSats != cap || intent.FeePolicy != policy {
		return nil, fmt.Errorf("wallet: protocol replay differs from frozen intent")
	}
	// RequestProtocol's existing-id hash check covers business cap, rate and
	// declared outputs without re-authorizing or changing spent input facts.
	candidate, err := w.core.kernel.Clone()
	if err != nil {
		return nil, err
	}
	view, err := candidate.RequestProtocol(manager.RequestInput{RequestID: id, BusinessAmount: req.Business, FeeRate: req.FeeRate, Outputs: declared})
	if err != nil {
		return nil, mapManagerError(err)
	}
	if len(intent.Inputs) != len(parsed.Inputs) {
		return nil, fmt.Errorf("wallet: inconsistent protocol replay inputs")
	}
	inputs := make([]woc.UTXO, 0, len(intent.Inputs))
	usedProofs := 0
	for i, fact := range intent.Inputs {
		in := parsed.Inputs[i]
		key := fmt.Sprintf("%s:%d", strings.ToLower(in.SourceTXID.String()), in.SourceTxOutIndex)
		if key != fact.Outpoint {
			return nil, fmt.Errorf("wallet: inconsistent protocol replay input order")
		}
		proof, present := req.Proofs[key]
		if fact.Class != manager.ProtocolInputWallet && !present {
			return nil, fmt.Errorf("wallet: protocol replay lacks original proof")
		}
		if present {
			usedProofs++
			if strconv.FormatUint(proof.Value, 10) != fact.Value || strings.TrimPrefix(strings.ToLower(proof.ScriptHex), "0x") != fact.ScriptHex {
				return nil, fmt.Errorf("wallet: protocol replay proof differs")
			}
		}
		value, err := manager.ParseAmount(fact.Value, "protocol input value")
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, woc.UTXO{TxID: in.SourceTXID.String(), Vout: in.SourceTxOutIndex, Value: value})
	}
	if usedProofs != len(req.Proofs) {
		return nil, fmt.Errorf("wallet: protocol replay has unrelated proofs")
	}
	fee, err := manager.ParseAmount(view.FeeSats, "protocol fee")
	if err != nil {
		return nil, err
	}
	return &SpendResult{TxID: txid, RawHex: exact, Inputs: inputs, Fee: fee, Address: w.Address(), AllocationID: id, Transaction: parsed}, nil
}
