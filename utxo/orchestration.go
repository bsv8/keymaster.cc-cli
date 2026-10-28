// Trusted submit orchestration (utm/1).
//
// State transitions are constructed on detached candidates, durably committed
// together with exact transaction/result bytes, and only then published to the
// live kernel. Dispatch is durable before network I/O.
package utxo

import (
	"encoding/json"
	"strings"
)

// DurableStore is the atomic host persistence port. Each write commits the
// associated exact bytes/result and the complete manager state as one unit.
type DurableStore interface {
	SaveIntent(allocationID, intentJSON, stateBlob string) error
	LoadIntent(allocationID string) (string, bool)
	SaveDispatch(commandID, txid, rawTxHex, stateBlob string) error
	LoadExact(txid string) (string, bool)
	RecordResult(commandID, resultJSON, stateBlob string) error
}

// StateStore is implemented by hosts that can persist a manager state blob
// that carries no per-command artifact, such as the external-signature
// responsibility ledger (E01). Hosts that cannot must refuse external
// delivery rather than lose the responsibility.
type StateStore interface {
	SaveState(stateBlob string) error
}

// BroadcastOutcome is one provider result.
type BroadcastOutcome struct {
	Status   string
	TxID     string
	RawTxHex string
	Reason   string
}

// Broadcaster is the host broadcast port.
type Broadcaster interface {
	Broadcast(rawTxHex string) (BroadcastOutcome, error)
}

// InputObserver supplies trusted per-input facts; HTTP interpretation stays in hosts.
type InputObserver interface {
	ObserveInputs(outpoints []string) ([]InputObservation, error)
}

// ChainObserver is the host chain observation port.
type ChainObserver interface {
	Observe(txid string) (string, bool)
}

// Ports bundles host I/O. Fault is an internal deterministic test hook.
type Ports struct {
	Store         DurableStore
	Broadcaster   Broadcaster
	Chain         ChainObserver
	InputObserver InputObserver
	Fault         func(point string) error
}

// SubmitResult reports a controlled submit.
type SubmitResult struct {
	AllocationID string           `json:"allocationId"`
	TxID         string           `json:"txid"`
	FeeSats      string           `json:"feeSats"`
	Delivery     string           `json:"delivery"`
	Conclusion   *InputConclusion `json:"conclusion,omitempty"`
}

type signingIntent struct {
	AllocationID  string `json:"allocationId"`
	UnsignedTxHex string `json:"unsignedTxHex"`
	FeeSats       string `json:"feeSats"`
}

func resultJSON(state, txid, reason string) string {
	value := map[string]string{"state": state, "txid": txid}
	if reason != "" {
		value["reason"] = reason
	}
	blob, _ := json.Marshal(value)
	return string(blob)
}

func runFault(ports Ports, point string) error {
	if ports.Fault == nil {
		return nil
	}
	return ports.Fault(point)
}

// PrepareIntent validates and durably freezes the unsigned transaction before
// authorizing the trusted signer. A failed store write leaves the live state
// unchanged.
func PrepareIntent(kernel *FundingKernel, store DurableStore, allocationID, unsignedTxHex string) (*PrepareResult, error) {
	candidate, err := kernel.Clone()
	if err != nil {
		return nil, err
	}
	prepared, err := candidate.Prepare(PrepareInput{AllocationID: allocationID, UnsignedTxHex: unsignedTxHex})
	if err != nil {
		return nil, err
	}
	if err := candidate.MarkSigningIntentDurable(allocationID); err != nil {
		return nil, err
	}
	intent, err := json.Marshal(signingIntent{
		AllocationID: allocationID, UnsignedTxHex: prepared.UnsignedTx, FeeSats: prepared.FeeSats,
	})
	if err != nil {
		return nil, err
	}
	stateBlob, err := candidate.Serialize()
	if err != nil {
		return nil, err
	}
	if err := store.SaveIntent(allocationID, string(intent), stateBlob); err != nil {
		return nil, err
	}
	if err := kernel.ReplaceFrom(candidate); err != nil {
		return nil, err
	}
	return prepared, nil
}

// SubmitSigned verifies durable signing authorization, persists exact bytes,
// responsibility and dispatch state before network I/O, then persists the
// result and settled state before publishing it to the live kernel.
func SubmitSigned(kernel *FundingKernel, ports Ports, allocationID, signedTxHex string) (*SubmitResult, error) {
	return runSubmit(kernel, ports, allocationID, signedTxHex, func(candidate *FundingKernel, id, hex string) (*CompletePrepareResult, error) {
		return candidate.CompletePrepare(CompletePrepareInput{AllocationID: id, SignedTxHex: hex})
	})
}

// SubmitProtocolSigned submits a protocol-bound transaction with the same
// durable ordering as an ordinary submit; the signed bytes are validated
// against the frozen protocol intent.
func SubmitProtocolSigned(kernel *FundingKernel, ports Ports, allocationID, signedTxHex string) (*SubmitResult, error) {
	return runSubmit(kernel, ports, allocationID, signedTxHex, func(candidate *FundingKernel, id, hex string) (*CompletePrepareResult, error) {
		completed, err := candidate.CompleteProtocol(CompleteProtocolInput{AllocationID: id, SignedTxHex: hex, Delivery: "wallet"})
		if err != nil {
			return nil, err
		}
		return &CompletePrepareResult{AllocationID: completed.AllocationID, TxID: completed.TxID, FeeSats: completed.FeeSats}, nil
	})
}

// ReconcileInputs observes spent facts without broadcasting and atomically
// persists the common per-input result before publication.
func ReconcileInputs(kernel *FundingKernel, ports Ports, allocationID string) (*SubmitResult, error) {
	allocation, err := kernel.AllocationView(allocationID)
	if err != nil {
		return nil, err
	}
	commandID := allocationID + ":broadcast"
	var entry *OutboxEntry
	for _, row := range kernel.OutboxView() {
		if row.CommandID == commandID {
			copy := row
			entry = &copy
			break
		}
	}
	if entry == nil {
		return nil, fail(ErrAllocationUnknown, "outbox command not found")
	}
	if entry.AllocationID == nil || *entry.AllocationID != allocationID {
		return nil, fail(ErrStateConflict, "outbox allocation mismatch")
	}
	if entry.State == "done" {
		return &SubmitResult{AllocationID: allocationID, TxID: entry.TxID, FeeSats: allocation.FeeSats, Delivery: "accepted"}, nil
	}
	if entry.State == "failed" {
		return &SubmitResult{AllocationID: allocationID, TxID: entry.TxID, FeeSats: allocation.FeeSats, Delivery: "failed", Conclusion: cloneInputConclusion(entry.Conclusion)}, nil
	}
	if ports.InputObserver == nil {
		return nil, fail(ErrPolicyDenied, "input observation port unavailable")
	}
	exact, found := ports.Store.LoadExact(entry.TxID)
	if !found || exact == "" || exact != entry.RawTxHex {
		return nil, fail(ErrStateConflict, "durable exact missing or inconsistent")
	}
	parsed, err := ParseTx(exact, entry.TxID)
	if err != nil {
		return nil, err
	}
	keys := []string{}
	for _, input := range parsed.Inputs {
		keys = append(keys, input.OutpointKey)
	}
	observations, observeErr := ports.InputObserver.ObserveInputs(keys)
	if observeErr != nil {
		observations = []InputObservation{}
	}
	candidate, err := kernel.Clone()
	if err != nil {
		return nil, err
	}
	conclusion, err := candidate.ResolveInputObservations(commandID, observations)
	if err != nil {
		return nil, err
	}
	delivery := "unknown"
	if conclusion.Status == "accepted" {
		delivery = "accepted"
	}
	if conclusion.Status == "failed" {
		delivery = "failed"
	}
	result := &SubmitResult{AllocationID: allocationID, TxID: entry.TxID, FeeSats: allocation.FeeSats, Delivery: delivery}
	if conclusion.Status == "conflict" || conclusion.Status == "failed" {
		result.Conclusion = conclusion
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	blob, err := candidate.Serialize()
	if err != nil {
		return nil, err
	}
	if err = ports.Store.RecordResult(commandID, string(encoded), blob); err != nil {
		return nil, err
	}
	if err = runFault(ports, "after-result-persist-before-publish"); err != nil {
		return nil, err
	}
	if err = kernel.ReplaceFrom(candidate); err != nil {
		return nil, err
	}
	return result, nil
}

// RetryExact performs one explicitly requested dispatch of the original durable
// exact. Scheduling, explicit user invocation and cancellation belong to hosts.
func RetryExact(kernel *FundingKernel, ports Ports, allocationID string) (*SubmitResult, error) {
	allocation, err := kernel.AllocationView(allocationID)
	if err != nil {
		return nil, err
	}
	var entry *OutboxEntry
	for _, row := range kernel.OutboxView() {
		if row.CommandID == allocationID+":broadcast" {
			copy := row
			entry = &copy
			break
		}
	}
	if entry == nil {
		return nil, fail(ErrAllocationUnknown, "outbox command not found")
	}
	if entry.AllocationID == nil || *entry.AllocationID != allocationID {
		return nil, fail(ErrStateConflict, "outbox allocation mismatch")
	}
	if entry.State == "done" {
		return &SubmitResult{AllocationID: allocationID, TxID: entry.TxID, FeeSats: allocation.FeeSats, Delivery: "accepted"}, nil
	}
	if entry.State == "failed" {
		return &SubmitResult{AllocationID: allocationID, TxID: entry.TxID, FeeSats: allocation.FeeSats, Delivery: "failed", Conclusion: cloneInputConclusion(entry.Conclusion)}, nil
	}
	if ports.InputObserver != nil {
		result, err := ReconcileInputs(kernel, ports, allocationID)
		if err != nil {
			return nil, err
		}
		if result.Delivery != "unknown" || (result.Conclusion != nil && result.Conclusion.Status == "conflict") {
			return result, nil
		}
	} else if entry.Conclusion != nil {
		return &SubmitResult{AllocationID: allocationID, TxID: entry.TxID, FeeSats: allocation.FeeSats, Delivery: "unknown", Conclusion: cloneInputConclusion(entry.Conclusion)}, nil
	}
	if entry.Delivery != "wallet" {
		return nil, fail(ErrPolicyDenied, "external delivery cannot be retried by wallet")
	}
	if allocation.State != "held" {
		return nil, fail(ErrAllocationState, "only held allocations can retry exact")
	}
	exact, found := ports.Store.LoadExact(entry.TxID)
	if !found || exact != entry.RawTxHex {
		return nil, fail(ErrStateConflict, "durable exact missing or inconsistent")
	}
	parsed, err := ParseTx(exact, entry.TxID)
	if err != nil || parsed.CanonicalTxID != entry.TxID {
		return nil, fail(ErrStateConflict, "durable exact missing or inconsistent")
	}
	// Reuse the same authorization, write-ahead dispatch and receipt/result path
	// as both ordinary and protocol submission. No completer or signer runs.
	return runSubmit(kernel, ports, allocationID, exact, func(*FundingKernel, string, string) (*CompletePrepareResult, error) {
		return &CompletePrepareResult{AllocationID: allocationID, TxID: entry.TxID, FeeSats: allocation.FeeSats}, nil
	})
}

func runSubmit(kernel *FundingKernel, ports Ports, allocationID, signedTxHex string, complete func(*FundingKernel, string, string) (*CompletePrepareResult, error)) (*SubmitResult, error) {
	intentJSON, found := ports.Store.LoadIntent(allocationID)
	if !found {
		return nil, fail(ErrStateConflict, "durable signing intent not found")
	}
	var savedIntent signingIntent
	if err := json.Unmarshal([]byte(intentJSON), &savedIntent); err != nil {
		return nil, fail(ErrStateConflict, "durable signing intent is invalid")
	}
	intentHex, durable, err := kernel.SigningIntent(allocationID)
	if err != nil {
		return nil, err
	}
	if !durable || savedIntent.AllocationID != allocationID || strings.ToLower(savedIntent.UnsignedTxHex) != intentHex {
		return nil, fail(ErrStateConflict, "signed transaction has no matching durable intent")
	}

	candidate, err := kernel.Clone()
	if err != nil {
		return nil, err
	}
	completed, err := complete(candidate, allocationID, signedTxHex)
	if err != nil {
		return nil, err
	}
	commandID := allocationID + ":broadcast"
	exactHex := strings.ToLower(signedTxHex)
	if err := candidate.MarkDispatching(commandID); err != nil {
		return nil, err
	}
	persistedHex := ""
	for _, entry := range candidate.OutboxView() {
		if entry.CommandID == commandID {
			persistedHex = entry.RawTxHex
			if entry.TxID != completed.TxID {
				return nil, fail(ErrStateConflict, "outbox txid does not match signed transaction")
			}
			break
		}
	}
	if persistedHex == "" || persistedHex != exactHex {
		return nil, fail(ErrStateConflict, "outbox bytes do not match signed transaction")
	}
	stateBlob, err := candidate.Serialize()
	if err != nil {
		return nil, err
	}
	if err := ports.Store.SaveDispatch(commandID, completed.TxID, exactHex, stateBlob); err != nil {
		return nil, err
	}
	if err := kernel.ReplaceFrom(candidate); err != nil {
		return nil, err
	}
	if err := runFault(ports, "after-dispatch-persist-before-broadcast"); err != nil {
		return nil, err
	}

	outcome, broadcastErr := ports.Broadcaster.Broadcast(persistedHex)
	if err := runFault(ports, "after-broadcast-before-result-persist"); err != nil {
		return nil, err
	}

	delivery := "unknown"
	report := BroadcastReport{Status: "unknown", TxID: completed.TxID}
	durableResult := resultJSON("unknown", completed.TxID, "invalid-provider-outcome")
	if broadcastErr != nil {
		durableResult = resultJSON("unknown", completed.TxID, "transport-error")
	} else {
		switch outcome.Status {
		case "accepted":
			txid, checkErr := CheckTxid(outcome.TxID)
			if checkErr == nil && strings.ToLower(txid) == completed.TxID {
				report = BroadcastReport{Status: "accepted", TxID: completed.TxID}
				durableResult = resultJSON("done", completed.TxID, "")
				delivery = "accepted"
			} else {
				durableResult = resultJSON("unknown", completed.TxID, "txid-mismatch")
			}
		case "already-known":
			sameExact := false
			if txid, checkErr := CheckTxid(outcome.TxID); checkErr == nil && strings.ToLower(txid) == completed.TxID {
				if observed, parseErr := ParseTx(outcome.RawTxHex, completed.TxID); parseErr == nil && observed.CanonicalTxID == completed.TxID && strings.ToLower(outcome.RawTxHex) == exactHex {
					sameExact = true
				}
			}
			if sameExact {
				report = BroadcastReport{Status: "already-known", TxID: completed.TxID, RawTxHex: outcome.RawTxHex}
				durableResult = resultJSON("done", completed.TxID, "")
				delivery = "already-known"
			} else {
				durableResult = resultJSON("unknown", completed.TxID, "already-known-mismatch")
			}
		case "not-dispatched":
			reason := outcome.Reason
			if reason == "" {
				reason = "not-dispatched"
			}
			durableResult = resultJSON("unknown", completed.TxID, reason)
		default:
			durableResult = resultJSON("unknown", completed.TxID, "invalid-provider-outcome")
		}
	}

	resultCandidate, err := kernel.Clone()
	if err != nil {
		return nil, err
	}
	if resultErr := resultCandidate.BroadcastResult(commandID, report); resultErr != nil {
		// A late receipt that conflicts with a trusted spender has already
		// isolated the affected inputs in the candidate. Persist that safety
		// state as unknown/conflict (never accepted), publish it, then report
		// the error to the caller.
		if CodeOf(resultErr) == ErrStateConflict {
			resultState, stateErr := resultCandidate.Serialize()
			if stateErr != nil {
				return nil, stateErr
			}
			if storeErr := ports.Store.RecordResult(commandID, resultJSON("unknown", completed.TxID, "result-conflict"), resultState); storeErr != nil {
				return nil, storeErr
			}
			if faultErr := runFault(ports, "after-result-persist-before-publish"); faultErr != nil {
				return nil, faultErr
			}
			if replaceErr := kernel.ReplaceFrom(resultCandidate); replaceErr != nil {
				return nil, replaceErr
			}
		}
		return nil, resultErr
	}
	resultState, err := resultCandidate.Serialize()
	if err != nil {
		return nil, err
	}
	if err := ports.Store.RecordResult(commandID, durableResult, resultState); err != nil {
		return nil, err
	}
	if err := runFault(ports, "after-result-persist-before-publish"); err != nil {
		return nil, err
	}
	if err := kernel.ReplaceFrom(resultCandidate); err != nil {
		return nil, err
	}
	if broadcastErr == nil && outcome.Status == "conflict" && ports.InputObserver != nil {
		return ReconcileInputs(kernel, ports, allocationID)
	}
	return &SubmitResult{AllocationID: allocationID, TxID: completed.TxID, FeeSats: completed.FeeSats, Delivery: delivery}, nil
}

// ReconcileUnknown verifies the same exact chain transaction and atomically
// persists the resulting manager state before publishing it.
func ReconcileUnknown(kernel *FundingKernel, ports Ports, allocationID string) (string, error) {
	commandID := allocationID + ":broadcast"
	var entry *OutboxEntry
	for _, candidate := range kernel.OutboxView() {
		if candidate.CommandID == commandID {
			copy := candidate
			entry = &copy
			break
		}
	}
	if entry == nil {
		return "", fail(ErrAllocationUnknown, "outbox command not found")
	}
	if entry.State == "done" {
		return "committed", nil
	}
	if entry.State == "failed" {
		return "failed", nil
	}
	if entry.Conclusion != nil {
		return "still-unknown", nil
	}
	observed, found := ports.Chain.Observe(entry.TxID)
	candidate, err := kernel.Clone()
	if err != nil {
		return "", err
	}
	var result string
	if found {
		result, err = candidate.Reconcile(commandID, true, observed)
	} else {
		result, err = candidate.Reconcile(commandID, false, "")
	}
	if err != nil {
		if CodeOf(err) == ErrStateConflict {
			stateBlob, stateErr := candidate.Serialize()
			if stateErr != nil {
				return "", stateErr
			}
			if storeErr := ports.Store.RecordResult(commandID, resultJSON("unknown", entry.TxID, "result-conflict"), stateBlob); storeErr != nil {
				return "", storeErr
			}
			if faultErr := runFault(ports, "after-result-persist-before-publish"); faultErr != nil {
				return "", faultErr
			}
			if replaceErr := kernel.ReplaceFrom(candidate); replaceErr != nil {
				return "", replaceErr
			}
		}
		return "", err
	}
	state := "unknown"
	if result == "committed" {
		state = "done"
	} else if result == "failed" {
		state = "failed"
	}
	stateBlob, err := candidate.Serialize()
	if err != nil {
		return "", err
	}
	if err := ports.Store.RecordResult(commandID, resultJSON(state, entry.TxID, ""), stateBlob); err != nil {
		return "", err
	}
	if err := runFault(ports, "after-result-persist-before-publish"); err != nil {
		return "", err
	}
	if err := kernel.ReplaceFrom(candidate); err != nil {
		return "", err
	}
	return result, nil
}

// PrepareProtocolIntent freezes a protocol intent durably before any signer
// runs. The caller must persist this state before it touches the wallet's
// payment channel.
func PrepareProtocolIntent(kernel *FundingKernel, store DurableStore, intent ProtocolIntentInput) (*ProtocolIntentResult, error) {
	candidate, err := kernel.Clone()
	if err != nil {
		return nil, err
	}
	prepared, err := candidate.PrepareProtocolIntent(intent)
	if err != nil {
		return nil, err
	}
	if err := candidate.MarkSigningIntentDurable(intent.AllocationID); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]string{
		"allocationId":  intent.AllocationID,
		"unsignedTxHex": prepared.UnsignedTx,
		"feeCapSats":    prepared.FeeCapSats,
		"kind":          intent.Binding.Kind,
		"poolId":        intent.Binding.PoolID,
	})
	if err != nil {
		return nil, err
	}
	stateBlob, err := candidate.Serialize()
	if err != nil {
		return nil, err
	}
	if err := store.SaveIntent(intent.AllocationID, string(payload), stateBlob); err != nil {
		return nil, err
	}
	if err := kernel.ReplaceFrom(candidate); err != nil {
		return nil, err
	}
	return prepared, nil
}

// CompleteProtocolIntent persists the exact signed bytes of a protocol
// intent. With delivery = "external" the entry starts in the unknown state:
// the bytes leave the wallet, so the wallet's own provider proves nothing
// about whether they were broadcast. This write happens BEFORE the bytes
// are handed outside (E01).
func CompleteProtocolIntent(kernel *FundingKernel, store DurableStore, allocationID, signedTxHex, delivery string) (*CompleteProtocolResult, error) {
	candidate, err := kernel.Clone()
	if err != nil {
		return nil, err
	}
	completed, err := candidate.CompleteProtocol(CompleteProtocolInput{
		AllocationID: allocationID, SignedTxHex: signedTxHex, Delivery: delivery,
	})
	if err != nil {
		return nil, err
	}
	commandID := allocationID + ":broadcast"
	exactHex := strings.ToLower(signedTxHex)
	found := false
	for _, entry := range candidate.OutboxView() {
		if entry.CommandID != commandID {
			continue
		}
		if entry.RawTxHex != exactHex || entry.TxID != completed.TxID {
			return nil, fail(ErrStateConflict, "outbox bytes do not match signed transaction")
		}
		if delivery == "external" && entry.State != "unknown" {
			return nil, fail(ErrStateConflict, "external delivery must start in the unknown state")
		}
		found = true
		break
	}
	if !found {
		return nil, fail(ErrStateConflict, "outbox bytes do not match signed transaction")
	}
	stateBlob, err := candidate.Serialize()
	if err != nil {
		return nil, err
	}
	if err := store.SaveDispatch(commandID, completed.TxID, exactHex, stateBlob); err != nil {
		return nil, err
	}
	if err := kernel.ReplaceFrom(candidate); err != nil {
		return nil, err
	}
	return completed, nil
}

// RecordExternalSignature records a partial signature that leaves the wallet.
// The responsibility is persisted durably BEFORE the signature is handed
// outside (E01). Without a durable state port the host must refuse.
func RecordExternalSignature(kernel *FundingKernel, store StateStore, input ExternalSignatureInput) (string, error) {
	if store == nil {
		return "", fail(ErrPolicyDenied, "host cannot persist external signature responsibility")
	}
	candidate, err := kernel.Clone()
	if err != nil {
		return "", err
	}
	id, err := candidate.RecordExternalSignature(input)
	if err != nil {
		return "", err
	}
	stateBlob, err := candidate.Serialize()
	if err != nil {
		return "", err
	}
	if err := store.SaveState(stateBlob); err != nil {
		return "", err
	}
	if err := kernel.ReplaceFrom(candidate); err != nil {
		return "", err
	}
	return id, nil
}
