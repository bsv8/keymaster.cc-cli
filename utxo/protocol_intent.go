package utxo

import (
	"sort"
	"strconv"
	"strings"
)

// Protection source kinds.
//
// "occupation" = a protocol submission's own input occupation. It may be
// taken over by the intent that created it and it never protects the funds.
// "fund" = a real fund protection (dedicated funding, another plugin). It can
// never be taken over and it can never be downgraded to a plain wallet input.
// "legacy" = a protection recovered from an older state that carried no
// source: it keeps blocking and is never takeable.
const (
	ProtectionSourceOccupation = "occupation"
	ProtectionSourceFund       = "fund"
	ProtectionSourceLegacy     = "legacy"
)

// ProtectionSourceKind returns the kind of a protection source string.
func ProtectionSourceKind(source string) string {
	kind := source
	if index := strings.Index(source, ":"); index >= 0 {
		kind = source[:index]
	}
	switch kind {
	case ProtectionSourceOccupation, ProtectionSourceFund:
		return kind
	}
	return ProtectionSourceLegacy
}

// OccupationSource is the source string of a protocol submission's own input
// occupation.
func OccupationSource(submissionID string) string {
	return ProtectionSourceOccupation + ":" + submissionID
}

// FundSource is the source string of a real fund protection.
func FundSource(owner string) string { return ProtectionSourceFund + ":" + owner }

// ProtocolIntentInput is the pre-signature protocol intent handed to the
// kernel by a trusted adapter. Every actual input of the unsigned
// transaction must be classified; the kernel verifies each class against its
// own facts and takes exclusive responsibility for the wallet and protected
// inputs.
type ProtocolIntentInput struct {
	AllocationID   string
	UnsignedTxHex  string
	Inputs         []ProtocolInputFact
	FeeCapSats     string
	FeePolicy      string
	Binding        *ProtocolBinding
	ExternalProofs map[string]ExternalProof
	// Claims are the protection sources this intent takes over. A protocol
	// submission's own input claim is an occupation, not a fund protection:
	// the manager may take it over, but only by naming it here. Every other
	// protection source keeps blocking the input.
	Claims []string
}

// ProtocolIntentResult reports the frozen protocol intent.
type ProtocolIntentResult struct {
	AllocationID string `json:"allocationId"`
	UnsignedTx   string `json:"unsignedTxHex"`
	FeeCapSats   string `json:"feeCapSats"`
}

// CompleteProtocolInput carries the signed bytes of a protocol intent and
// who is responsible for putting them on the network.
type CompleteProtocolInput struct {
	AllocationID string
	SignedTxHex  string
	Delivery     string
}

// CompleteProtocolResult reports the validated protocol transaction.
type CompleteProtocolResult struct {
	AllocationID string `json:"allocationId"`
	TxID         string `json:"txid"`
	FeeSats      string `json:"feeSats"`
}

// ExternalSignatureEvidence is the trusted, structured fact that retires an
// external signature responsibility. There is no evidence-free settlement.
type ExternalSignatureEvidence struct {
	// Kind is "input-consumed" (a credibly observed transaction consumed the
	// inputs the signature affected) or "signature-invalidated" (the
	// signature became unusable without consuming an input).
	Kind string
	// TxID is the observed transaction (required for "input-consumed").
	TxID string
	// RawTxHex is the exact observed transaction bytes (required for
	// "input-consumed"). The settlement projects the money from these
	// bytes: a txid alone cannot produce a spendable change output.
	RawTxHex string
	// Inputs are the inputs the observed transaction consumed (required for
	// "input-consumed").
	Inputs []string
	// Reason explains an invalidation (required for "signature-invalidated").
	Reason string
}

// ExternalSignatureInput records a partial signature that leaves the wallet.
type ExternalSignatureInput struct {
	ID            string
	AllocationID  string
	Scope         string
	DraftTxHex    string
	SignatureHex  string
	RelatedInputs []string
	Reason        string
	Binding       *ProtocolBinding
}

// protocolIntentDigestPayload is the canonical shape of a protocol intent
// digest. The field order matches the TypeScript implementation byte for
// byte, so both languages derive the same digest for the same state.
type protocolIntentDigestPayload struct {
	Skeleton   string                 `json:"skeleton"`
	Inputs     []ProtocolInputFact    `json:"inputs"`
	Proofs     map[string]storedProof `json:"proofs"`
	FeeCapSats string                 `json:"feeCapSats"`
	FeePolicy  string                 `json:"feePolicy"`
	Binding    *ProtocolBinding       `json:"binding"`
	Outputs    []storedOutput         `json:"outputs"`
	Claims     []string               `json:"claims"`
}

// protocolIntentDigest is the canonical digest of a protocol intent. Two
// intents are the same only when the frozen skeleton, the classified inputs,
// the trusted proofs, the fee policy, the binding and the declared outputs
// are all identical.
func protocolIntentDigest(input protocolIntentDigestPayload) string {
	payload := protocolIntentDigestPayload{
		Skeleton: input.Skeleton, FeeCapSats: input.FeeCapSats, FeePolicy: input.FeePolicy,
		Proofs: map[string]storedProof{}, Inputs: []ProtocolInputFact{}, Outputs: []storedOutput{},
		Claims: []string{},
	}
	for _, claim := range input.Claims {
		payload.Claims = append(payload.Claims, claim)
	}
	sort.Strings(payload.Claims)
	for key, proof := range input.Proofs {
		payload.Proofs[key] = storedProof{Value: proof.Value, ScriptHex: proof.ScriptHex}
	}
	for _, fact := range input.Inputs {
		claim := (*string)(nil)
		if fact.ClaimID != nil {
			holder := *fact.ClaimID
			claim = &holder
		}
		payload.Inputs = append(payload.Inputs, ProtocolInputFact{
			Outpoint: fact.Outpoint, Class: fact.Class, Value: fact.Value,
			ScriptHex: fact.ScriptHex, ClaimID: claim,
		})
	}
	for _, output := range input.Outputs {
		payload.Outputs = append(payload.Outputs, storedOutput{ScriptHex: output.ScriptHex, Value: output.Value})
	}
	if input.Binding != nil {
		payload.Binding = &ProtocolBinding{Kind: input.Binding.Kind, PoolID: input.Binding.PoolID}
	}
	blob, err := marshalCanonical(payload)
	if err != nil {
		return ""
	}
	return CalcTxid([]byte(blob))
}

// matchProtocolOutputs matches a transaction's outputs against the anchor's
// declared manifest. Declared outputs (business payouts AND
// protocol-earmarked self outputs such as a dedicated funding output) are
// consumed by script+value. At most one undeclared self output may remain:
// that is ordinary change.
func (k *FundingKernel) matchProtocolOutputs(declared []storedOutput, outputs []TxOutput) ([]uint64, uint64, error) {
	remaining := append([]storedOutput(nil), declared...)
	ownOut := []uint64{}
	var change uint64
	changeCount := 0
	for _, out := range outputs {
		script := strings.ToLower(out.Script)
		found := -1
		for i, r := range remaining {
			if r.ScriptHex == script && r.Value == out.Value {
				found = i
				break
			}
		}
		if found >= 0 {
			remaining = append(remaining[:found], remaining[found+1:]...)
			if script == k.config.WalletScriptHex {
				ownOut = append(ownOut, out.Value)
			}
			continue
		}
		if script == k.config.WalletScriptHex {
			change = out.Value
			changeCount++
			ownOut = append(ownOut, out.Value)
			continue
		}
		return nil, 0, fail(ErrPolicyDenied, "protocol template has an undeclared non-self output")
	}
	if len(remaining) != 0 {
		return nil, 0, fail(ErrPolicyDenied, "protocol template misses declared outputs")
	}
	if changeCount > 1 {
		return nil, 0, fail(ErrPolicyDenied, "at most one change output is allowed")
	}
	return ownOut, change, nil
}

// PrepareProtocolIntent freezes a protocol-bound unsigned transaction before
// any signer runs. Unclassified, mismatched or over-cap inputs fail closed.
func (k *FundingKernel) PrepareProtocolIntent(input ProtocolIntentInput) (*ProtocolIntentResult, error) {
	if err := k.assertFundsNotFrozen("prepare a protocol intent"); err != nil {
		return nil, err
	}
	id, err := checkID(input.AllocationID)
	if err != nil {
		return nil, err
	}
	record, ok := k.allocations[id]
	if !ok {
		return nil, fail(ErrAllocationUnknown, "allocation not found")
	}
	if record.State != "held" {
		return nil, fail(ErrAllocationState, "cannot prepare from "+record.State)
	}
	if input.Binding == nil {
		return nil, fail(ErrPolicyDenied, "valid protocol binding is required")
	}
	kind := input.Binding.Kind
	if kind != ProtocolKindRefund && kind != ProtocolKindClose && kind != ProtocolKindFund && kind != ProtocolKindState {
		return nil, fail(ErrPolicyDenied, "valid protocol binding is required")
	}
	if input.Binding.PoolID == "" {
		return nil, fail(ErrPolicyDenied, "protocol binding needs a pool id")
	}
	feeCap, err := ParseAmount(input.FeeCapSats, "fee cap")
	if err != nil {
		return nil, err
	}
	if feeCap == 0 {
		return nil, fail(ErrFeeInvalid, "fee cap must be positive")
	}
	if input.FeePolicy != ProtocolFeePolicyRate && input.FeePolicy != ProtocolFeePolicyExact {
		return nil, fail(ErrPolicyDenied, "protocol intent needs a valid fee policy")
	}
	if record.FeeRate == 0 {
		return nil, fail(ErrFeeInvalid, "protocol allocation needs a fee rate")
	}
	parsed, err := ParseTx(input.UnsignedTxHex, "")
	if err != nil {
		return nil, err
	}
	if len(parsed.Inputs) == 0 {
		return nil, fail(ErrTxInvalid, "protocol intent needs at least one input")
	}

	factsByKey := map[string]ProtocolInputFact{}
	for _, raw := range input.Inputs {
		if raw.Outpoint == "" {
			return nil, fail(ErrTxInvalid, "protocol input needs an outpoint")
		}
		txid, vout, err := splitOutpoint(strings.ToLower(raw.Outpoint))
		if err != nil {
			return nil, err
		}
		key, err := outpointKey(txid, vout)
		if err != nil {
			return nil, err
		}
		if _, exists := factsByKey[key]; exists {
			return nil, fail(ErrTxInvalid, "duplicate protocol input "+key)
		}
		if raw.Class != ProtocolInputWallet && raw.Class != ProtocolInputProtected && raw.Class != ProtocolInputExternal {
			return nil, fail(ErrPolicyDenied, "unknown protocol input class for "+key)
		}
		value, err := ParseAmount(raw.Value, "input value")
		if err != nil {
			return nil, err
		}
		scriptHex, err := checkScriptHex(raw.ScriptHex, "input script")
		if err != nil {
			return nil, err
		}
		fact := ProtocolInputFact{
			Outpoint: key, Class: raw.Class, Value: mustFormat(value), ScriptHex: scriptHex,
		}
		if raw.Class == ProtocolInputProtected {
			if raw.ClaimID == nil || *raw.ClaimID == "" {
				return nil, fail(ErrPolicyDenied, "input "+key+" needs a protocol claim id")
			}
			holder := *raw.ClaimID
			fact.ClaimID = &holder
		}
		factsByKey[key] = fact
	}
	if len(factsByKey) != len(parsed.Inputs) {
		return nil, fail(ErrSnapshotInputInvalid, "intent inputs do not match the transaction")
	}

	takenClaims := uniqueLowerSorted(input.Claims)
	for _, claim := range takenClaims {
		// Only an occupation may be taken over, and only by the intent that
		// created it: a fund protection is never a claimable balance.
		if ProtectionSourceKind(claim) != ProtectionSourceOccupation {
			return nil, fail(ErrPolicyDenied, "claim "+claim+" is not a takeover-able occupation")
		}
	}
	walletKeys := []string{}
	facts := make([]ProtocolInputFact, 0, len(parsed.Inputs))
	normalizedProofs := map[string]storedProof{}
	walletValues := []uint64{}
	externalValues := []uint64{}
	for _, txIn := range parsed.Inputs {
		key := txIn.OutpointKey
		fact, ok := factsByKey[key]
		if !ok {
			return nil, fail(ErrSnapshotInputInvalid, "input "+key+" has no trusted classification")
		}
		value, err := ParseAmount(fact.Value, "input value")
		if err != nil {
			return nil, err
		}
		row, rowExists := k.utxos[key]
		if rowExists && (row.SpentBy != nil || row.Isolated != nil || row.PendingParent != nil) {
			return nil, fail(ErrSnapshotInputInvalid, "input "+key+" is spent, isolated or has an unknown parent")
		}
		if rowExists && row.OccupiedBy != nil && *row.OccupiedBy != id {
			return nil, fail(ErrAlreadyOccupied, "input "+key+" is occupied")
		}

		// Gate 1 — exclusive occupation, BEFORE any use permission and for
		// EVERY classification. A fund/protected authorization says what the
		// money may be used for; it never says somebody else's already-signed
		// occupation may be ignored.
		if rowExists && row.Protected {
			for _, entry := range row.ProtectionSources {
				if ProtectionSourceKind(entry) != ProtectionSourceFund && !containsString(takenClaims, entry) {
					return nil, fail(ErrFundsProtected, "input "+key+" is a protected output reserved by another occupation")
				}
			}
		}

		if fact.Class == ProtocolInputExternal {
			proof, hasProof := input.ExternalProofs[key]
			if !hasProof {
				return nil, fail(ErrUnknownInput, "input "+key+" has no trusted proof")
			}
			proofValue, err := ParseAmount(proof.Value, "proof value")
			if err != nil {
				return nil, err
			}
			proofScript, err := checkScriptHex(proof.ScriptHex, "proof script")
			if err != nil {
				return nil, err
			}
			if proofScript == k.config.WalletScriptHex {
				return nil, fail(ErrPolicyDenied, "input "+key+" claims the wallet script without kernel proof")
			}
			if proofValue != value {
				return nil, fail(ErrStateConflict, "proof value does not match the declared input "+key)
			}
			if rowExists && (row.ScriptHex != proofScript || row.Value != proofValue) {
				return nil, fail(ErrStateConflict, "proof does not match the recorded prevout "+key)
			}
			normalizedProofs[key] = storedProof{Value: proofValue, ScriptHex: proofScript}
			externalValues = append(externalValues, proofValue)
		} else {
			if !rowExists {
				return nil, fail(ErrUnknownInput, "input "+key+" is not a known wallet output")
			}
			if row.ScriptHex != k.config.WalletScriptHex {
				return nil, fail(ErrUnknownInput, "input "+key+" is not a proven wallet input")
			}
			if row.Value != value || row.ScriptHex != fact.ScriptHex {
				return nil, fail(ErrStateConflict, "input "+key+" does not match the recorded prevout")
			}
			if fact.Class == ProtocolInputWallet {
				// A plain wallet input must not carry a fund protection: that
				// is a protocol-marked output and needs its own business claim.
				for _, entry := range row.ProtectionSources {
					if ProtectionSourceKind(entry) == ProtectionSourceFund {
						return nil, fail(ErrFundsProtected, "input "+key+" is a protected output")
					}
				}
			}
			if fact.Class == ProtocolInputProtected {
				if !row.Protected {
					return nil, fail(ErrFundsProtected, "input "+key+" is not a protected output")
				}
				if fact.ClaimID == nil {
					return nil, fail(ErrPolicyDenied, "input "+key+" needs a protocol claim id")
				}
				// A mere input claim is not a business protection: the input
				// must carry at least one FUND source.
				fund := false
				for _, entry := range row.ProtectionSources {
					if ProtectionSourceKind(entry) == ProtectionSourceFund {
						fund = true
						break
					}
				}
				if !fund {
					return nil, fail(ErrFundsProtected, "input "+key+" has no fund protection to claim")
				}
			}
			walletKeys = append(walletKeys, key)
			walletValues = append(walletValues, row.Value)
		}
		facts = append(facts, fact)
	}
	if err := k.checkAncestors(walletKeys); err != nil {
		return nil, err
	}

	// The output manifest is matched completely: a declared output (business
	// or a protocol-earmarked self output) is consumed by script+value, and at
	// most one undeclared self output may remain as ordinary change.
	ownOut, _, err := k.matchProtocolOutputs(record.Outputs, parsed.Outputs)
	if err != nil {
		return nil, err
	}

	// The exact amounts are already known before signing: the input values
	// come from trusted prevout facts and the outputs from the unsigned
	// template. The fee and the wallet outflow are therefore checked HERE,
	// not only after the signer ran.
	var walletIn, externalIn uint64
	for _, value := range walletValues {
		walletIn, err = AddChecked(walletIn, value, "wallet inputs")
		if err != nil {
			return nil, err
		}
	}
	for _, value := range externalValues {
		externalIn, err = AddChecked(externalIn, value, "external inputs")
		if err != nil {
			return nil, err
		}
	}
	totalIn, err := AddChecked(walletIn, externalIn, "inputs")
	if err != nil {
		return nil, err
	}
	outputValues := make([]uint64, 0, len(parsed.Outputs))
	for _, out := range parsed.Outputs {
		outputValues = append(outputValues, out.Value)
	}
	totalOut, err := SumChecked(outputValues, "outputs")
	if err != nil {
		return nil, err
	}
	if totalIn < totalOut {
		return nil, fail(ErrFeeOutOfRange, "protocol template outputs exceed inputs")
	}
	actualFee := totalIn - totalOut
	if actualFee > feeCap {
		return nil, fail(ErrFeeOutOfRange, "protocol fee "+mustFormat(actualFee)+" above the authorized cap "+mustFormat(feeCap))
	}
	var ownTotal uint64
	for _, value := range ownOut {
		ownTotal, err = AddChecked(ownTotal, value, "own outputs")
		if err != nil {
			return nil, err
		}
	}
	walletLoss := uint64(0)
	if walletIn > ownTotal {
		walletLoss = walletIn - ownTotal
	}
	walletCap, err := AddChecked(record.Business, feeCap, "wallet cap")
	if err != nil {
		return nil, err
	}
	if walletLoss > walletCap {
		return nil, fail(ErrPolicyDenied, "wallet pays more than business plus fee share")
	}

	// Necessary condition only: the unsigned template is a lower bound of the
	// signed size, so a cap that cannot even cover it cannot cover the final
	// transaction. The authoritative bounds are checked on the exact bytes.
	unsignedFee, err := EstimateFeeSats(record.FeeRate, uint64(parsed.SizeBytes))
	if err != nil {
		return nil, err
	}
	if unsignedFee > feeCap {
		return nil, fail(ErrFeeOutOfRange, "authorized fee cap is below the estimated miner fee")
	}

	skeleton, err := unsignedSkeleton(input.UnsignedTxHex)
	if err != nil {
		return nil, err
	}
	// A frozen intent is immutable: the same content is idempotent, different
	// content is an explicit conflict. Replacing it needs a new allocation.
	digest := protocolIntentDigest(protocolIntentDigestPayload{
		Skeleton: skeleton, Inputs: facts, Proofs: normalizedProofs,
		FeeCapSats: mustFormat(feeCap), FeePolicy: input.FeePolicy,
		Binding: &ProtocolBinding{Kind: kind, PoolID: input.Binding.PoolID}, Outputs: record.Outputs,
		Claims: takenClaims,
	})
	if record.SigningIntentHex != "" {
		frozen := protocolIntentDigest(protocolIntentDigestPayload{
			Skeleton: record.SigningIntentHex, Inputs: record.ProtocolInputs, Proofs: record.ExternalProofs,
			FeeCapSats: record.ProtocolFeeCapSats, FeePolicy: record.ProtocolFeePolicy,
			Binding: record.ProtocolBinding, Outputs: record.Outputs,
			Claims: record.ProtocolClaims,
		})
		if frozen != digest {
			return nil, fail(ErrStateConflict, "protocol intent differs from the frozen intent")
		}
		return &ProtocolIntentResult{
			AllocationID: id, UnsignedTx: strings.ToLower(input.UnsignedTxHex), FeeCapSats: mustFormat(feeCap),
		}, nil
	}

	// Every actual input now carries this allocation's responsibility.
	for _, fact := range facts {
		key := fact.Outpoint
		row, ok := k.utxos[key]
		if ok {
			holder := id
			row.OccupiedBy = &holder
			continue
		}
		proof := normalizedProofs[key]
		holder := id
		k.utxos[key] = &storedUtxo{
			Value: proof.Value, ScriptHex: proof.ScriptHex, Status: "unconfirmed", Height: 0,
			Source: "external", ParentInputs: nil, Confirmed: false,
			SpentBy: nil, SpentExternal: false, OccupiedBy: &holder, Protected: false,
			Isolated: nil, PendingParent: nil,
		}
	}
	inputs := make([]string, 0, len(facts))
	for _, fact := range facts {
		inputs = append(inputs, fact.Outpoint)
	}
	record.ProtocolInputs = facts
	record.ProtocolFeeCapSats = mustFormat(feeCap)
	record.ProtocolFeePolicy = input.FeePolicy
	record.ProtocolClaims = takenClaims
	record.ExternalProofs = normalizedProofs
	record.ProtocolBinding = &ProtocolBinding{Kind: kind, PoolID: input.Binding.PoolID}
	record.Inputs = inputs
	record.SigningIntentHex = skeleton
	k.localRev++
	return &ProtocolIntentResult{
		AllocationID: id, UnsignedTx: strings.ToLower(input.UnsignedTxHex), FeeCapSats: mustFormat(feeCap),
	}, nil
}

// CompleteProtocol validates the signed bytes of a protocol intent against
// the frozen intent and records the exact transaction. "external" delivery
// starts in the unknown state: the wallet's own provider proves nothing
// about whether an outside party already broadcast the bytes.
func (k *FundingKernel) CompleteProtocol(input CompleteProtocolInput) (*CompleteProtocolResult, error) {
	if err := k.assertFundsNotFrozen("complete a protocol spend"); err != nil {
		return nil, err
	}
	id, err := checkID(input.AllocationID)
	if err != nil {
		return nil, err
	}
	record, ok := k.allocations[id]
	if !ok {
		return nil, fail(ErrAllocationUnknown, "allocation not found")
	}
	if record.State != "held" {
		return nil, fail(ErrAllocationState, "cannot complete from "+record.State)
	}
	if len(record.ProtocolInputs) == 0 {
		return nil, fail(ErrAllocationState, "protocol intent was not prepared")
	}
	parsed, err := ParseTx(input.SignedTxHex, "")
	if err != nil {
		return nil, err
	}
	if record.SignedTxid != nil && *record.SignedTxid != parsed.CanonicalTxID {
		return nil, fail(ErrStateConflict, "protocol allocation already binds another transaction")
	}
	skeleton, err := unsignedSkeleton(input.SignedTxHex)
	if err != nil {
		return nil, err
	}
	if record.SigningIntentHex != "" && record.SigningIntentHex != skeleton {
		return nil, fail(ErrStateConflict, "transaction differs from frozen signing intent")
	}
	if len(parsed.Inputs) != len(record.ProtocolInputs) {
		return nil, fail(ErrSnapshotInputInvalid, "signed inputs do not match the protocol intent")
	}
	for i, txIn := range parsed.Inputs {
		if txIn.OutpointKey != record.ProtocolInputs[i].Outpoint {
			return nil, fail(ErrSnapshotInputInvalid, "signed inputs do not match the protocol intent")
		}
	}

	// The same manifest rules apply to the exact bytes: a declared output
	// (including a protocol-earmarked self output) is consumed by script and
	// value; at most one undeclared self output is ordinary change.
	ownOut, _, err := k.matchProtocolOutputs(record.Outputs, parsed.Outputs)
	if err != nil {
		return nil, err
	}
	var ownTotal uint64
	for _, value := range ownOut {
		ownTotal, err = AddChecked(ownTotal, value, "own outputs")
		if err != nil {
			return nil, err
		}
	}

	var walletIn, externalIn uint64
	for _, fact := range record.ProtocolInputs {
		if fact.Class == ProtocolInputExternal {
			proof, ok := record.ExternalProofs[fact.Outpoint]
			if !ok {
				return nil, fail(ErrUnknownInput, "input "+fact.Outpoint+" has no trusted proof")
			}
			externalIn, err = AddChecked(externalIn, proof.Value, "external inputs")
			if err != nil {
				return nil, err
			}
			continue
		}
		value := uint64(0)
		if row, ok := k.utxos[fact.Outpoint]; ok {
			value = row.Value
		} else {
			parsedValue, parseErr := ParseAmount(fact.Value, "input value")
			if parseErr != nil {
				return nil, parseErr
			}
			value = parsedValue
		}
		walletIn, err = AddChecked(walletIn, value, "wallet inputs")
		if err != nil {
			return nil, err
		}
	}
	totalIn, err := AddChecked(walletIn, externalIn, "inputs")
	if err != nil {
		return nil, err
	}
	outputValues := make([]uint64, 0, len(parsed.Outputs))
	for _, out := range parsed.Outputs {
		outputValues = append(outputValues, out.Value)
	}
	totalOut, err := SumChecked(outputValues, "outputs")
	if err != nil {
		return nil, err
	}
	if totalIn < totalOut {
		return nil, fail(ErrFeeOutOfRange, "protocol tx outputs exceed inputs")
	}
	fee := totalIn - totalOut
	feeFloor := uint64(1)
	if record.ProtocolFeePolicy != ProtocolFeePolicyExact {
		floor, err := EstimateFeeSats(record.FeeRate, uint64(parsed.SizeBytes))
		if err != nil {
			return nil, err
		}
		feeFloor = floor
	}
	feeCap, err := ParseAmount(record.ProtocolFeeCapSats, "fee cap")
	if err != nil {
		return nil, err
	}
	if fee < feeFloor {
		return nil, fail(ErrFeeOutOfRange, "protocol fee below actual-size minimum")
	}
	if fee > feeCap {
		return nil, fail(ErrFeeOutOfRange, "protocol fee above the authorized cap")
	}
	walletLoss := uint64(0)
	if walletIn > ownTotal {
		walletLoss = walletIn - ownTotal
	}
	walletCap, err := AddChecked(record.Business, feeCap, "wallet cap")
	if err != nil {
		return nil, err
	}
	if walletLoss > walletCap {
		return nil, fail(ErrPolicyDenied, "wallet pays more than business plus fee share")
	}

	outboxID := id + ":broadcast"
	exactHex := strings.ToLower(input.SignedTxHex)
	var existing *OutboxEntry
	for i := range k.outbox {
		if k.outbox[i].CommandID == outboxID {
			existing = &k.outbox[i]
			break
		}
	}
	if existing != nil && (existing.TxID != parsed.CanonicalTxID || existing.RawTxHex != exactHex) {
		return nil, fail(ErrStateConflict, "protocol outbox already holds different exact bytes")
	}
	if existing == nil {
		delivery := "wallet"
		state := "pending"
		if input.Delivery == "external" {
			delivery = "external"
			state = "unknown"
		}
		holder := id
		k.outbox = append(k.outbox, OutboxEntry{
			CommandID: outboxID, AllocationID: &holder, TxID: parsed.CanonicalTxID,
			RawTxHex: exactHex, State: state, Delivery: delivery,
		})
	}
	record.SignedTxid = &parsed.CanonicalTxID
	record.Fee = fee
	k.localRev++
	return &CompleteProtocolResult{AllocationID: id, TxID: parsed.CanonicalTxID, FeeSats: mustFormat(fee)}, nil
}

// RecordExternalSignature records a partial signature that leaves the wallet.
// The receiver may complete and spend it, so the responsibility survives
// until a trusted observation settles the related outpoints.
//
// The record is bound to a real allocation and to that allocation's own
// inputs: a signature that is not attached to held funds cannot create (or
// clear) a money responsibility.
func (k *FundingKernel) RecordExternalSignature(input ExternalSignatureInput) (string, error) {
	if err := k.assertFundsNotFrozen("record an external signature"); err != nil {
		return "", err
	}
	id, err := checkID(input.ID)
	if err != nil {
		return "", err
	}
	if input.AllocationID == "" {
		return "", fail(ErrPolicyDenied, "external signature needs an allocation")
	}
	allocation, ok := k.allocations[input.AllocationID]
	if !ok {
		return "", fail(ErrAllocationUnknown, "external signature allocation not found")
	}
	if allocation.State != "held" {
		return "", fail(ErrAllocationState, "external signature cannot attach to a "+allocation.State+" allocation")
	}
	if input.DraftTxHex == "" {
		return "", fail(ErrTxInvalid, "external signature needs the exact draft bytes")
	}
	if input.SignatureHex == "" {
		return "", fail(ErrTxInvalid, "external signature needs the signature bytes")
	}
	// The exact draft is parsed: the responsibility follows the artifact, not
	// the caller's metadata. A signature only ever affects inputs the signed
	// artifact actually spends.
	artifact, err := ParseTx(input.DraftTxHex, "")
	if err != nil {
		return "", fail(ErrTxInvalid, "external signature draft is not a valid transaction")
	}
	artifactInputs := []string{}
	seenArtifact := map[string]bool{}
	for _, txIn := range artifact.Inputs {
		key := txIn.OutpointKey
		if seenArtifact[key] {
			continue
		}
		seenArtifact[key] = true
		artifactInputs = append(artifactInputs, key)
	}
	sort.Strings(artifactInputs)
	if len(artifactInputs) == 0 {
		return "", fail(ErrTxInvalid, "external signature draft spends no input")
	}
	related := make([]string, 0, len(input.RelatedInputs))
	seen := map[string]bool{}
	for _, key := range input.RelatedInputs {
		lower := strings.ToLower(key)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		related = append(related, lower)
	}
	sort.Strings(related)
	if len(related) == 0 {
		return "", fail(ErrPolicyDenied, "external signature needs the related inputs it exposes")
	}
	for _, key := range related {
		if !containsString(artifactInputs, key) {
			return "", fail(ErrPolicyDenied, "external signature input "+key+" is not spent by the signed artifact")
		}
	}
	// Every input the allocation holds and the artifact spends is covered by
	// the responsibility: a signature may cover part of a transaction, but
	// it may not leave the wallet's own money uncovered.
	for _, held := range allocation.Inputs {
		if !containsString(artifactInputs, held) {
			continue
		}
		if !containsString(related, held) {
			return "", fail(ErrPolicyDenied, "external signature does not cover the held input "+held)
		}
	}
	// The responsibility may only cover inputs this allocation already owns.
	// "The kernel does not know the outpoint yet" is not a credential: an
	// unknown input must not be registered now and left for a later snapshot
	// to promote into free wallet balance.
	for _, key := range artifactInputs {
		if !containsString(related, key) {
			return "", fail(ErrPolicyDenied, "external signature related inputs do not cover the signed artifact")
		}
	}
	type missingExternal struct {
		key       string
		value     uint64
		scriptHex string
	}
	missing := []missingExternal{}
	for _, key := range artifactInputs {
		if !containsString(allocation.Inputs, key) {
			return "", fail(ErrPolicyDenied, "external signature input "+key+" is not held by the allocation")
		}
		row, exists := k.utxos[key]
		if exists && row.SpentBy != nil {
			return "", fail(ErrPolicyDenied, "external signature input "+key+" is already spent")
		}
		if exists && row.Isolated != nil {
			return "", fail(ErrStateConflict, "external signature input "+key+" is isolated")
		}
		if exists && row.OccupiedBy != nil && *row.OccupiedBy != input.AllocationID {
			return "", fail(ErrAlreadyOccupied, "external signature input "+key+" is held by another allocation")
		}
		isWalletInput := exists && row.ScriptHex == k.config.WalletScriptHex
		if isWalletInput {
			if row.OccupiedBy == nil || *row.OccupiedBy != input.AllocationID {
				return "", fail(ErrPolicyDenied, "external signature input "+key+" is not held by the allocation")
			}
			continue
		}
		// A third-party input is not wallet balance, but the allocation must
		// have proved its prevout and taken responsibility for it at pre-sign
		// time. A row that merely exists is not an authorization.
		proof, proven := allocation.ExternalProofs[key]
		if !proven {
			return "", fail(ErrPolicyDenied, "external signature input "+key+" is not a proven external input of the allocation")
		}
		if exists {
			if row.ScriptHex != proof.ScriptHex || row.Value != proof.Value {
				return "", fail(ErrStateConflict, "external signature input "+key+" does not match the trusted prevout proof")
			}
			if row.OccupiedBy == nil || *row.OccupiedBy != input.AllocationID {
				return "", fail(ErrPolicyDenied, "external signature input "+key+" is not held by the allocation")
			}
		} else {
			missing = append(missing, missingExternal{key: key, value: proof.Value, scriptHex: proof.ScriptHex})
		}
	}
	existing, ok := k.externalSignatures[id]
	if ok {
		olderKind, olderPool := "", ""
		if existing.Binding != nil {
			olderKind, olderPool = existing.Binding.Kind, existing.Binding.PoolID
		}
		newerKind, newerPool := "", ""
		if input.Binding != nil {
			newerKind, newerPool = input.Binding.Kind, input.Binding.PoolID
		}
		if existing.DraftTxHex != strings.ToLower(input.DraftTxHex) ||
			existing.SignatureHex != strings.ToLower(input.SignatureHex) ||
			strings.Join(existing.RelatedInputs, ",") != strings.Join(related, ",") ||
			existing.AllocationID != input.AllocationID ||
			existing.Scope != input.Scope ||
			olderKind != newerKind || olderPool != newerPool {
			return "", fail(ErrIDConflict, "same external signature id with different content")
		}
		return id, nil
	}
	// A legacy allocation may carry a trusted proof whose outpoint was not
	// materialized. Create the responsibility row now, already occupied by
	// this allocation, so a later snapshot cannot turn it into free money.
	for _, external := range missing {
		holder := input.AllocationID
		k.utxos[external.key] = &storedUtxo{
			Value: external.value, ScriptHex: external.scriptHex, Status: "unconfirmed", Source: "external",
			OccupiedBy: &holder, ProtectionSources: []string{},
		}
	}
	record := &ExternalSignatureRecord{
		ID: id, AllocationID: input.AllocationID, Scope: input.Scope,
		DraftTxHex: strings.ToLower(input.DraftTxHex), SignatureHex: strings.ToLower(input.SignatureHex),
		RelatedInputs: related, ArtifactInputs: artifactInputs, Reason: input.Reason, State: "responsible",
	}
	if input.Binding != nil {
		record.Binding = &ProtocolBinding{Kind: input.Binding.Kind, PoolID: input.Binding.PoolID}
	}
	k.externalSignatures[id] = record
	k.localRev++
	return id, nil
}

// SettleExternalSignature marks a partial-signature responsibility as
// settled by trusted evidence.
//
// If the observation conflicts with an already recorded spender, the
// candidate is mutated to isolate the affected inputs/descendants and the
// call returns ErrStateConflict. The host MUST persist that candidate: the
// isolation is the safety action, not an optional side effect.
func (k *FundingKernel) SettleExternalSignature(id string, evidence ExternalSignatureEvidence) error {
	key, err := checkID(id)
	if err != nil {
		return err
	}
	record, ok := k.externalSignatures[key]
	if !ok {
		return fail(ErrAllocationUnknown, "external signature not found")
	}
	if record.State == "settled" {
		return nil
	}
	// There is no evidence-free settlement: a bare id is not proof.
	switch evidence.Kind {
	case "input-consumed":
		txid, txErr := CheckTxid(evidence.TxID)
		if txErr != nil {
			return fail(ErrPolicyDenied, "settlement evidence needs a valid txid")
		}
		if evidence.RawTxHex == "" {
			return fail(ErrPolicyDenied, "settlement evidence needs the observed exact transaction")
		}
		exact, exactErr := ParseTx(evidence.RawTxHex, txid)
		if exactErr != nil {
			return fail(ErrPolicyDenied, "settlement evidence does not carry the observed exact transaction")
		}
		consumed := uniqueLowerSorted(evidence.Inputs)
		if len(consumed) == 0 {
			return fail(ErrPolicyDenied, "settlement evidence must name the consumed inputs")
		}
		// The exact transaction is the primary fact. Its real inputs, not the
		// caller's list, decide which responsibility it touched.
		exactInputs := []string{}
		for _, txIn := range exact.Inputs {
			exactInputs = append(exactInputs, txIn.OutpointKey)
		}
		exactInputs = uniqueLowerSorted(exactInputs)
		responsibility := uniqueLowerSorted(append(append([]string(nil), record.ArtifactInputs...), record.RelatedInputs...))
		responsible := map[string]bool{}
		for _, key := range responsibility {
			responsible[key] = true
		}
		hits := []string{}
		for _, key := range exactInputs {
			if responsible[key] {
				hits = append(hits, key)
			}
		}
		if len(hits) == 0 {
			return fail(ErrPolicyDenied, "the observed transaction does not spend the responsibility")
		}
		if !equalStrings(exactInputs, consumed) {
			// A declaration that disagrees with the exact bytes is not a
			// usable settlement, but a real consumption of a responsible
			// input still freezes the old chain.
			for _, key := range hits {
				k.isolateConflict(key, txid)
			}
			k.localRev++
			return fail(ErrPolicyDenied, "the observed transaction does not spend the declared inputs; related change is isolated")
		}
		if _, ok := k.allocations[record.AllocationID]; !ok {
			return fail(ErrAllocationUnknown, "external signature allocation no longer exists")
		}
		outboxID := record.AllocationID + ":broadcast"
		outboxConflict := false
		for _, entry := range k.outbox {
			if entry.CommandID != outboxID {
				continue
			}
			if entry.TxID != txid || entry.RawTxHex != strings.ToLower(evidence.RawTxHex) {
				outboxConflict = true
			}
			break
		}
		conflicting := []string{}
		for _, key := range exactInputs {
			row, exists := k.utxos[key]
			if !exists {
				continue
			}
			if row.SpentBy != nil && *row.SpentBy != txid {
				conflicting = append(conflicting, key)
				continue
			}
			if row.OccupiedBy != nil && *row.OccupiedBy != record.AllocationID {
				conflicting = append(conflicting, key)
				continue
			}
			if row.Isolated != nil {
				conflicting = append(conflicting, key)
			}
		}
		if len(conflicting) > 0 || outboxConflict {
			// Do not silently skip a conflicting input and then clear the
			// duty. Isolate every responsible/conflicting outpoint and its old
			// local descendants; the responsibility stays outstanding until
			// the host resolves the conflict with a consistent trusted fact.
			isolate := uniqueLowerSorted(append(append([]string(nil), hits...), conflicting...))
			for _, key := range isolate {
				k.isolateConflict(key, txid)
			}
			k.localRev++
			return fail(ErrStateConflict, "observed consumption conflicts with the recorded spender; related change is isolated")
		}

		coversArtifact := true
		for _, key := range record.ArtifactInputs {
			if !containsString(exactInputs, key) {
				coversArtifact = false
				break
			}
		}
		if !coversArtifact {
			// A competing transaction that consumes only part of the artifact
			// is a real external-consumption fact, not a successful settlement
			// of the whole responsibility. Isolate the touched chain; keep the
			// rest of the duty outstanding.
			for _, key := range hits {
				k.isolateConflict(key, txid)
			}
			k.localRev++
			return fail(ErrStateConflict, "the observed transaction only consumes part of the responsibility; related change is isolated")
		}

		allocation := k.allocations[record.AllocationID]
		authorized := k.allocationExactMatches(allocation, exact, evidence.RawTxHex)

		// Retire the whole round first, then project the money. Only a match
		// with this allocation's frozen template may complete the allocation.
		holder := txid
		for _, sibling := range k.externalSignatures {
			if sibling.AllocationID != record.AllocationID || sibling.State == "settled" {
				continue
			}
			// Only retire a responsibility whose WHOLE artifact was consumed.
			// A sibling with an uncovered input keeps its duty and occupation.
			if len(sibling.ArtifactInputs) == 0 {
				continue
			}
			covered := true
			for _, key := range sibling.ArtifactInputs {
				if !containsString(exactInputs, key) {
					covered = false
					break
				}
			}
			if !covered {
				continue
			}
			sibling.State = "settled"
			sibling.SettledTxid = &holder
		}
		for _, key := range exactInputs {
			row, exists := k.utxos[key]
			if !exists {
				continue
			}
			row.SpentBy = &holder
			row.SpentExternal = true
			row.OccupiedBy = nil
		}
		// The observed exact transaction is the parent of its own outputs: the
		// ordinary self change becomes spendable immediately, without a WoC
		// tick, a block or the end of the fee pool. Another transaction that
		// merely consumes the same inputs gets a separate observation entry and
		// never borrows `<allocation>:broadcast`.
		mode := "observation"
		if authorized {
			mode = "allocation"
		}
		if err := k.projectSettledTransaction(record.AllocationID, exact, strings.ToLower(evidence.RawTxHex), mode); err != nil {
			return err
		}
		k.refreshFundsFrozen()
		k.releaseOrphanSignatureQuarantine()
		k.localRev++
		return nil
	case "signature-invalidated":
		if evidence.Reason == "" {
			return fail(ErrPolicyDenied, "an invalidated signature needs a reason")
		}
		if evidence.TxID != "" {
			if _, txErr := CheckTxid(evidence.TxID); txErr != nil {
				return fail(ErrPolicyDenied, "settlement evidence needs a valid txid")
			}
		}
		if evidence.TxID != "" {
			holder := strings.ToLower(evidence.TxID)
			record.SettledTxid = &holder
		}
		record.State = "settled"
		k.refreshFundsFrozen()
		k.releaseOrphanSignatureQuarantine()
		k.localRev++
		return nil
	}
	return fail(ErrPolicyDenied, "unknown settlement evidence kind")
}

// allocationExactMatches proves that a trusted exact observation is the
// allocation's own authorized transaction: the frozen skeleton (inputs,
// outputs, version, locktime, sequence) must match, and an already stored
// exact txid must be the observed one. Merely spending the same inputs is not
// the same authorization.
func (k *FundingKernel) allocationExactMatches(record *storedAllocation, exact *ParsedTx, rawTxHex string) bool {
	if record == nil || record.SigningIntentHex == "" {
		return false
	}
	skeleton, err := UnsignedSkeleton(rawTxHex)
	if err != nil || skeleton != record.SigningIntentHex {
		return false
	}
	if record.SignedTxid != nil && *record.SignedTxid != exact.CanonicalTxID {
		return false
	}
	return true
}

// isolateConflict isolates one consumed outpoint and the local descendants of
// the recorded spender. The recorded spender is never overwritten: a conflict
// is not a new canonical spend.
func (k *FundingKernel) isolateConflict(outpoint, observedTxid string) {
	key := strings.ToLower(outpoint)
	row, exists := k.utxos[key]
	if !exists {
		reason := "conflict:" + observedTxid
		k.utxos[key] = &storedUtxo{
			Status: "unconfirmed", Source: "external", Isolated: &reason,
			ProtectionSources: []string{},
		}
		return
	}
	if row.Isolated == nil {
		reason := "conflict:" + observedTxid
		row.Isolated = &reason
	}
	k.isolateDescendants(key, "ancestor-conflict:"+key)
}

// ExternalResponsibilityFor returns the active external-signature
// responsibility of an allocation, if any. Release paths consult it: a
// signature that already left the wallet keeps its inputs responsible until
// a trusted terminal state is recorded.
func (k *FundingKernel) ExternalResponsibilityFor(allocationID string) *ExternalSignatureRecord {
	id, err := checkID(allocationID)
	if err != nil {
		return nil
	}
	// Canonical id order is stable across languages and serialize/restore.
	for _, record := range k.ExternalSignatureView() {
		if record.AllocationID != id {
			continue
		}
		if record.State != "responsible" && record.State != "superseded" && record.State != "quarantined" {
			continue
		}
		return &record
	}
	return nil
}

// SupersedeExternalSignature models a newer protocol round that legitimately
// replaces an older partial signature. The replacement is explicit and
// auditable: the old record stays in the ledger and points at the record that
// superseded it.
func (k *FundingKernel) SupersedeExternalSignature(oldID, newID string) error {
	oldKey, err := checkID(oldID)
	if err != nil {
		return err
	}
	newKey, err := checkID(newID)
	if err != nil {
		return err
	}
	if oldKey == newKey {
		return fail(ErrIDConflict, "a signature cannot supersede itself")
	}
	older, ok := k.externalSignatures[oldKey]
	if !ok {
		return fail(ErrAllocationUnknown, "external signature not found")
	}
	newer, ok := k.externalSignatures[newKey]
	if !ok {
		return fail(ErrAllocationUnknown, "superseding external signature not found")
	}
	if newer.State != "responsible" {
		return fail(ErrAllocationState, "superseding signature is not responsible")
	}
	if older.AllocationID != newer.AllocationID {
		return fail(ErrPolicyDenied, "a signature can only supersede one of the same allocation")
	}
	if older.Scope != newer.Scope {
		return fail(ErrPolicyDenied, "a signature can only supersede one of the same scope")
	}
	olderKind, olderPool := "", ""
	if older.Binding != nil {
		olderKind, olderPool = older.Binding.Kind, older.Binding.PoolID
	}
	newerKind, newerPool := "", ""
	if newer.Binding != nil {
		newerKind, newerPool = newer.Binding.Kind, newer.Binding.PoolID
	}
	if olderKind != newerKind || olderPool != newerPool {
		return fail(ErrPolicyDenied, "a signature can only supersede one of the same contract")
	}
	if strings.Join(older.RelatedInputs, ",") != strings.Join(newer.RelatedInputs, ",") {
		return fail(ErrPolicyDenied, "a signature can only supersede one over the same inputs")
	}
	if older.State == "settled" {
		return nil
	}
	// The old artifact is still valid: it stays responsible and visible.
	older.State = "superseded"
	holder := newKey
	older.SupersededBy = &holder
	k.localRev++
	return nil
}

// ProtocolIntentView is the frozen protocol intent of an allocation.
type ProtocolIntentView struct {
	Inputs     []ProtocolInputFact `json:"inputs"`
	FeeCapSats string              `json:"feeCapSats"`
	FeePolicy  string              `json:"feePolicy"`
	Binding    *ProtocolBinding    `json:"binding"`
	SignedTxid *string             `json:"signedTxid"`
}

// ProtocolIntentView returns the frozen protocol intent of an allocation
// (nil for ordinary ones). The host uses it to prove that a
// protocol-protected input was claimed through the manager rather than by a
// caller flag.
func (k *FundingKernel) ProtocolIntentView(allocationID string) *ProtocolIntentView {
	id, err := checkID(allocationID)
	if err != nil {
		return nil
	}
	record, ok := k.allocations[id]
	if !ok || len(record.ProtocolInputs) == 0 {
		return nil
	}
	inputs := make([]ProtocolInputFact, 0, len(record.ProtocolInputs))
	for _, fact := range record.ProtocolInputs {
		fact.ClaimID = copyStringPointer(fact.ClaimID)
		inputs = append(inputs, fact)
	}
	var binding *ProtocolBinding
	if record.ProtocolBinding != nil {
		copy := *record.ProtocolBinding
		binding = &copy
	}
	return &ProtocolIntentView{
		Inputs: inputs, FeeCapSats: record.ProtocolFeeCapSats, FeePolicy: record.ProtocolFeePolicy,
		Binding: binding, SignedTxid: copyStringPointer(record.SignedTxid),
	}
}

// ResponsibilityView reports the outstanding money responsibilities: the
// exact transactions the wallet still owns (or handed outside) and the
// partial signatures that may still be completed by their receivers.
func (k *FundingKernel) ResponsibilityView() []ResponsibilityView {
	views := []ResponsibilityView{}
	for _, entry := range k.outbox {
		if entry.State == "done" || entry.State == "failed" {
			continue
		}
		var inputs []string
		if entry.AllocationID != nil {
			if record, ok := k.allocations[*entry.AllocationID]; ok {
				inputs = append([]string(nil), record.Inputs...)
			}
		}
		if inputs == nil {
			inputs = []string{}
		}
		var totalIn uint64
		for _, key := range inputs {
			if row, ok := k.utxos[key]; ok {
				totalIn += row.Value
			}
		}
		ownOutputs := []ResponsibilityOwnOutput{}
		if entry.Delivery == "external" || entry.State != "pending" {
			parsed, err := ParseTx(entry.RawTxHex, entry.TxID)
			if err == nil {
				for i, out := range parsed.Outputs {
					if strings.ToLower(out.Script) != k.config.WalletScriptHex {
						continue
					}
					ownOutputs = append(ownOutputs, ResponsibilityOwnOutput{
						Outpoint: entry.TxID + ":" + strconv.Itoa(i), Value: mustFormat(out.Value),
					})
				}
			}
		}
		reason := "wallet broadcast outcome unknown"
		if entry.Delivery == "external" {
			reason = "signed bytes delivered outside the wallet"
		}
		views = append(views, ResponsibilityView{
			CommandID: entry.CommandID, AllocationID: copyStringPointer(entry.AllocationID), TxID: entry.TxID,
			State: entry.State, Delivery: entry.Delivery, Reason: reason,
			Inputs: inputs, TotalInSats: mustFormat(totalIn), OwnOutputs: ownOutputs,
		})
	}
	external := k.ExternalSignatureView()
	for _, row := range external {
		if row.State == "settled" {
			continue
		}
		inputs := row.RelatedInputs
		if len(row.ArtifactInputs) > 0 {
			inputs = row.ArtifactInputs
		}
		views = append(views, ResponsibilityView{
			CommandID: row.ID, AllocationID: &row.AllocationID, TxID: "",
			State: "unknown", Delivery: "external", Reason: row.Reason,
			Inputs: inputs, TotalInSats: "0", OwnOutputs: []ResponsibilityOwnOutput{},
		})
	}
	return views
}

// assertNoExternalResponsibility refuses a release while a partial signature
// that already left the wallet is still responsible for the allocation.
func (k *FundingKernel) assertNoExternalResponsibility(allocationID string) error {
	responsibility := k.ExternalResponsibilityFor(allocationID)
	if responsibility != nil {
		return fail(ErrAllocationState,
			"allocation holds an external signature responsibility ("+responsibility.ID+"); it cannot be released")
	}
	return nil
}

// ExternalSignatureView lists the external partial-signature ledger.
func (k *FundingKernel) ExternalSignatureView() []ExternalSignatureRecord {
	rows := make([]ExternalSignatureRecord, 0, len(k.externalSignatures))
	for _, record := range k.externalSignatures {
		copy := *record
		copy.RelatedInputs = append([]string(nil), record.RelatedInputs...)
		if copy.RelatedInputs == nil {
			copy.RelatedInputs = []string{}
		}
		copy.ArtifactInputs = append([]string(nil), record.ArtifactInputs...)
		if copy.ArtifactInputs == nil {
			copy.ArtifactInputs = []string{}
		}
		copy.SupersededBy = copyStringPointer(record.SupersededBy)
		copy.SettledTxid = copyStringPointer(record.SettledTxid)
		if record.Binding != nil {
			binding := *record.Binding
			copy.Binding = &binding
		}
		rows = append(rows, copy)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// projectSettledTransaction projects a trusted observed transaction that
// retires an external signature responsibility.
//
// It reuses the ordinary exact-settlement projection: the parent exact bytes
// are stored, the self outputs become spendable local outputs with their
// parent relation, and the spent inputs keep their tombstones. The allocation
// is only marked committed when this exact transaction really is its own
// transaction AND no other responsibility is outstanding.
func (k *FundingKernel) projectSettledTransaction(allocationID string, exact *ParsedTx, rawTxHex, mode string) error {
	txid := exact.CanonicalTxID
	parentInputs := []string{}
	for _, txIn := range exact.Inputs {
		parentInputs = append(parentInputs, txIn.OutpointKey)
	}
	// A trusted observation of another transaction consumes the money but is
	// NOT proof that this allocation's authorized transaction succeeded. It
	// gets its own evidence entry and never borrows `<allocation>:broadcast`.
	outboxID := allocationID + ":broadcast"
	if mode != "allocation" {
		outboxID = "observed:" + txid
	}
	var entry *OutboxEntry
	for index := range k.outbox {
		if k.outbox[index].CommandID == outboxID {
			entry = &k.outbox[index]
			break
		}
	}
	if entry != nil && entry.RawTxHex != rawTxHex {
		return fail(ErrStateConflict, "the observed transaction differs from the recorded exact bytes")
	}
	if entry == nil {
		var holder *string
		if mode == "allocation" {
			value := allocationID
			holder = &value
		}
		k.outbox = append(k.outbox, OutboxEntry{
			CommandID: outboxID, AllocationID: holder, TxID: txid, RawTxHex: rawTxHex,
			State: "done", Delivery: "external",
		})
	} else {
		entry.State = "done"
	}
	// The self outputs of the observed exact transaction become spendable.
	for index, out := range exact.Outputs {
		if strings.ToLower(out.Script) == k.config.WalletScriptHex {
			k.mergeSelfOutput(txid, parentInputs, out, index)
		}
	}
	if mode != "allocation" {
		return nil
	}
	// The caller already proved the frozen template and stored exact txid.
	// Only an allocation without any remaining responsibility is completed.
	record, ok := k.allocations[allocationID]
	if !ok {
		return nil
	}
	outstanding := false
	for _, row := range k.externalSignatures {
		if row.AllocationID == allocationID && row.State != "settled" {
			outstanding = true
			break
		}
	}
	if !outstanding {
		record.State = "committed"
		txidHolder := txid
		record.SignedTxid = &txidHolder
	}
	return nil
}
