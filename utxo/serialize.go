package utxo

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// Serialization is byte-identical to the TypeScript FundingKernel.serialize()
// so state hashes match across languages.

type serialUtxo struct {
	Value             string   `json:"value"`
	ScriptHex         string   `json:"scriptHex"`
	Status            string   `json:"status"`
	Height            int64    `json:"height"`
	Source            string   `json:"source"`
	CreatingTxid      *string  `json:"creatingTxid"`
	SpentBy           *string  `json:"spentBy"`
	SpentExternal     bool     `json:"spentExternal"`
	OccupiedBy        *string  `json:"occupiedBy"`
	Protected         bool     `json:"protected"`
	ProtectionSources []string `json:"protectionSources"`
	Isolated          *string  `json:"isolated"`
	PendingParent     *string  `json:"pendingParent"`
	ParentInputs      []string `json:"parentInputs"`
	Confirmed         bool     `json:"confirmed"`
}

type serialOutput struct {
	ScriptHex string `json:"scriptHex"`
	Value     string `json:"value"`
}

type serialProof struct {
	Value     string `json:"value"`
	ScriptHex string `json:"scriptHex"`
}

type serialAllocation struct {
	RequestID            string                 `json:"requestId"`
	RequestHash          string                 `json:"requestHash"`
	Inputs               []string               `json:"inputs"`
	Business             string                 `json:"business"`
	FeeRate              string                 `json:"feeRate"`
	Fee                  string                 `json:"fee"`
	Change               string                 `json:"change"`
	Outputs              []serialOutput         `json:"outputs"`
	Version              uint32                 `json:"version"`
	Locktime             uint32                 `json:"locktime"`
	ExternalProofs       map[string]serialProof `json:"externalProofs"`
	ProtocolBinding      *ProtocolBinding       `json:"protocolBinding"`
	ProtocolInputs       []ProtocolInputFact    `json:"protocolInputs"`
	ProtocolFeeCapSats   string                 `json:"protocolFeeCapSats"`
	ProtocolFeePolicy    string                 `json:"protocolFeePolicy"`
	ProtocolClaims       []string               `json:"protocolClaims"`
	SignedTxid           *string                `json:"signedTxid"`
	SigningIntentHex     string                 `json:"signingIntentHex"`
	SigningIntentDurable bool                   `json:"signingIntentDurable"`
	State                string                 `json:"state"`
}

type serialConfig struct {
	WalletScriptHex         string `json:"walletScriptHex"`
	DustThresholdSats       string `json:"dustThresholdSats"`
	MaxUnconfirmedAncestors int    `json:"maxUnconfirmedAncestors"`
}

type serialState struct {
	Version            string                      `json:"version"`
	Scope              Scope                       `json:"scope"`
	Config             serialConfig                `json:"config"`
	Generation         int64                       `json:"generation"`
	WocRevision        int64                       `json:"wocRevision"`
	LocalRev           int64                       `json:"localRevision"`
	Utxos              map[string]serialUtxo       `json:"utxos"`
	Allocations        map[string]serialAllocation `json:"allocations"`
	Outbox             []OutboxEntry               `json:"outbox"`
	ExternalSignatures []ExternalSignatureRecord   `json:"externalSignatures"`
	FundsFrozen        *string                     `json:"fundsFrozen,omitempty"`
}

func marshalCanonical(value interface{}) (string, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// Serialize renders the canonical state JSON.
func (k *FundingKernel) Serialize() (string, error) {
	state := serialState{
		Version:            ContractVersion,
		Scope:              k.scope,
		Config:             serialConfig{WalletScriptHex: k.config.WalletScriptHex, DustThresholdSats: k.config.DustThresholdSats, MaxUnconfirmedAncestors: k.config.MaxUnconfirmedAncestors},
		Generation:         k.generation,
		WocRevision:        k.wocRevision,
		LocalRev:           k.localRev,
		Utxos:              map[string]serialUtxo{},
		Allocations:        map[string]serialAllocation{},
		Outbox:             append([]OutboxEntry(nil), k.outbox...),
		ExternalSignatures: []ExternalSignatureRecord{},
		FundsFrozen:        k.fundsFrozen,
	}
	if state.Outbox == nil {
		state.Outbox = []OutboxEntry{}
	}
	for _, row := range k.externalSignatures {
		copy := *row
		copy.RelatedInputs = append([]string(nil), row.RelatedInputs...)
		if copy.RelatedInputs == nil {
			copy.RelatedInputs = []string{}
		}
		copy.ArtifactInputs = append([]string(nil), row.ArtifactInputs...)
		if copy.ArtifactInputs == nil {
			copy.ArtifactInputs = []string{}
		}
		state.ExternalSignatures = append(state.ExternalSignatures, copy)
	}
	sort.Slice(state.ExternalSignatures, func(i, j int) bool {
		return state.ExternalSignatures[i].ID < state.ExternalSignatures[j].ID
	})
	for key, row := range k.utxos {
		sources := append([]string(nil), row.ProtectionSources...)
		if sources == nil {
			sources = []string{}
		}
		state.Utxos[key] = serialUtxo{
			Value: mustFormat(row.Value), ScriptHex: row.ScriptHex, Status: row.Status, Height: row.Height,
			Source: row.Source, CreatingTxid: row.CreatingTxid, SpentBy: row.SpentBy,
			SpentExternal: row.SpentExternal, OccupiedBy: row.OccupiedBy, Protected: row.Protected,
			ProtectionSources: sources, Isolated: row.Isolated,
			PendingParent: row.PendingParent, ParentInputs: row.ParentInputs, Confirmed: row.Confirmed,
		}
	}
	for key, record := range k.allocations {
		outputs := make([]serialOutput, 0, len(record.Outputs))
		for _, o := range record.Outputs {
			outputs = append(outputs, serialOutput{ScriptHex: o.ScriptHex, Value: mustFormat(o.Value)})
		}
		proofs := map[string]serialProof{}
		for proofKey, proof := range record.ExternalProofs {
			proofs[proofKey] = serialProof{Value: mustFormat(proof.Value), ScriptHex: proof.ScriptHex}
		}
		inputs := append([]string(nil), record.Inputs...)
		if inputs == nil {
			inputs = []string{}
		}
		protocolInputs := append([]ProtocolInputFact(nil), record.ProtocolInputs...)
		if protocolInputs == nil {
			protocolInputs = []ProtocolInputFact{}
		}
		claims := append([]string(nil), record.ProtocolClaims...)
		if claims == nil {
			claims = []string{}
		}
		state.Allocations[key] = serialAllocation{
			RequestID: record.RequestID, RequestHash: record.RequestHash, Inputs: inputs,
			Business: mustFormat(record.Business), FeeRate: mustFormat(record.FeeRate),
			Fee: mustFormat(record.Fee), Change: mustFormat(record.Change),
			Outputs: outputs, Version: record.Version, Locktime: record.Locktime,
			ExternalProofs: proofs, ProtocolBinding: record.ProtocolBinding,
			ProtocolInputs: protocolInputs, ProtocolFeeCapSats: record.ProtocolFeeCapSats,
			ProtocolFeePolicy: record.ProtocolFeePolicy,
			ProtocolClaims:    claims,
			SignedTxid:        record.SignedTxid, SigningIntentHex: record.SigningIntentHex,
			SigningIntentDurable: record.SigningIntentDurable, State: record.State,
		}
	}
	return marshalCanonical(state)
}

// Restore rebuilds a kernel from canonical JSON. Scope must match.
func Restore(blob string, scope Scope) (*FundingKernel, error) {
	var state serialState
	decoder := json.NewDecoder(strings.NewReader(blob))
	if err := decoder.Decode(&state); err != nil {
		return nil, fail(ErrStateConflict, "snapshot blob is not JSON")
	}
	if state.Version != ContractVersion {
		return nil, fail(ErrVersionUnsupported, "unsupported state "+state.Version)
	}
	if state.Scope.Owner != scope.Owner || state.Scope.Network != scope.Network {
		return nil, fail(ErrScopeMismatch, "snapshot scope does not match")
	}
	kernel, err := NewFundingKernel(scope, ManagerConfig{
		WalletScriptHex:         state.Config.WalletScriptHex,
		DustThresholdSats:       state.Config.DustThresholdSats,
		MaxUnconfirmedAncestors: state.Config.MaxUnconfirmedAncestors,
	})
	if err != nil {
		return nil, err
	}
	for key, row := range state.Utxos {
		if _, _, err := splitOutpoint(key); err != nil {
			return nil, err
		}
		if key != strings.ToLower(key) {
			return nil, fail(ErrTxInvalid, "noncanonical outpoint")
		}
		value, err := ParseAmount(row.Value, "utxo value")
		if err != nil {
			return nil, err
		}
		sources := append([]string(nil), row.ProtectionSources...)
		if sources == nil {
			sources = []string{}
		}
		// A legacy state carried `protected: true` without any source. Its
		// protection meaning must not be lost: an unknown protection keeps
		// blocking and can never be taken over.
		if row.Protected && len(sources) == 0 {
			sources = append(sources, ProtectionSourceLegacy+":unknown")
		}
		kernel.utxos[key] = &storedUtxo{
			Value: value, ScriptHex: row.ScriptHex, Status: row.Status, Height: row.Height,
			Source: row.Source, CreatingTxid: row.CreatingTxid, SpentBy: row.SpentBy,
			SpentExternal: row.SpentExternal, OccupiedBy: row.OccupiedBy, Protected: row.Protected,
			ProtectionSources: sources, Isolated: row.Isolated,
			PendingParent: row.PendingParent, ParentInputs: row.ParentInputs, Confirmed: row.Confirmed,
		}
	}
	for key, record := range state.Allocations {
		outputs := make([]storedOutput, 0, len(record.Outputs))
		for _, o := range record.Outputs {
			value, err := ParseAmount(o.Value, "output value")
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, storedOutput{ScriptHex: o.ScriptHex, Value: value})
		}
		proofs := map[string]storedProof{}
		for proofKey, proof := range record.ExternalProofs {
			value, err := ParseAmount(proof.Value, "proof value")
			if err != nil {
				return nil, err
			}
			proofs[proofKey] = storedProof{Value: value, ScriptHex: proof.ScriptHex}
		}
		business, err := ParseAmount(record.Business, "business")
		if err != nil {
			return nil, err
		}
		feeRate, err := ParseAmount(record.FeeRate, "fee rate")
		if err != nil {
			return nil, err
		}
		fee, err := ParseAmount(record.Fee, "fee")
		if err != nil {
			return nil, err
		}
		change, err := ParseAmount(record.Change, "change")
		if err != nil {
			return nil, err
		}
		kernel.allocations[key] = &storedAllocation{
			RequestID: record.RequestID, RequestHash: record.RequestHash,
			Inputs:   append([]string(nil), record.Inputs...),
			Business: business, FeeRate: feeRate, Fee: fee, Change: change,
			Outputs: outputs, Version: record.Version, Locktime: record.Locktime,
			ExternalProofs: proofs, ProtocolBinding: record.ProtocolBinding,
			ProtocolInputs:     append([]ProtocolInputFact(nil), record.ProtocolInputs...),
			ProtocolFeeCapSats: record.ProtocolFeeCapSats,
			ProtocolFeePolicy:  record.ProtocolFeePolicy,
			ProtocolClaims:     append([]string(nil), record.ProtocolClaims...),
			SignedTxid:         record.SignedTxid, SigningIntentHex: record.SigningIntentHex,
			SigningIntentDurable: record.SigningIntentDurable, State: record.State,
		}
	}
	kernel.outbox = make([]OutboxEntry, 0, len(state.Outbox))
	for _, entry := range state.Outbox {
		if entry.Delivery == "" {
			entry.Delivery = "wallet"
		}
		if entry.AllocationID == nil {
			// Backward compat: pre-allocationId blobs derive the owner from
			// the command shape (strip one trailing ":broadcast").
			if strings.HasSuffix(entry.CommandID, ":broadcast") {
				holder := strings.TrimSuffix(entry.CommandID, ":broadcast")
				entry.AllocationID = &holder
			}
		}
		kernel.outbox = append(kernel.outbox, entry)
	}
	kernel.externalSignatures = make(map[string]*ExternalSignatureRecord, len(state.ExternalSignatures))
	for _, row := range state.ExternalSignatures {
		copy := row
		copy.RelatedInputs = uniqueLowerSorted(row.RelatedInputs)
		cached := uniqueLowerSorted(row.ArtifactInputs)
		copy.ArtifactInputs = cached
		// `ArtifactInputs` is a derived cache, never independent authorization
		// evidence. Every outstanding record is re-parsed from the exact
		// draft; a missing, unreadable or disagreeing cache quarantines it.
		if copy.State != "settled" {
			parsed, parseErr := ParseTx(row.DraftTxHex, "")
			if parseErr != nil {
				copy.State = "quarantined"
			} else {
				inputs := []string{}
				for _, txIn := range parsed.Inputs {
					inputs = append(inputs, txIn.OutpointKey)
				}
				actual := uniqueLowerSorted(inputs)
				switch {
				case len(actual) == 0:
					copy.State = "quarantined"
				case len(cached) == 0:
					copy.ArtifactInputs = actual
				case !equalStrings(cached, actual):
					// Keep every determinable responsibility: the cache may
					// name a real input the draft no longer does, or the draft
					// may spend an input the cache hid. The union stays frozen
					// until trusted evidence resolves the record.
					copy.ArtifactInputs = uniqueLowerSorted(append(append([]string(nil), cached...), actual...))
					copy.State = "quarantined"
				default:
					copy.ArtifactInputs = actual
				}
			}
		}
		incomplete := copy.State != "settled" && len(copy.ArtifactInputs) == 0
		kernel.externalSignatures[copy.ID] = &copy
		// Isolating the record is not enough: the money it names must stop
		// being free balance. Every input the responsibility covers is
		// occupied by that allocation, or isolated when the scope cannot
		// prove who holds it.
		if copy.State == "settled" {
			continue
		}
		if incomplete {
			// The artifact input set is unknown. Occupying the readable
			// metadata is not enough: freeze new funding for this scope until
			// the host resolves the quarantine with trusted evidence.
			if kernel.fundsFrozen == nil {
				reason := "migration:" + copy.ID
				kernel.fundsFrozen = &reason
			}
		}
		covered := uniqueLowerSorted(append(append([]string(nil), copy.RelatedInputs...), copy.ArtifactInputs...))
		allocation := kernel.allocations[copy.AllocationID]
		for _, key := range covered {
			held, exists := kernel.utxos[key]
			if !exists {
				reason := "external-signature-quarantine"
				kernel.utxos[key] = &storedUtxo{
					Status: "unconfirmed", Source: "external", Isolated: &reason,
					ProtectionSources: []string{},
				}
				continue
			}
			if held.OccupiedBy != nil && *held.OccupiedBy != copy.AllocationID {
				reason := "external-signature-quarantine"
				held.Isolated = &reason
				continue
			}
			// Only inputs that belong to the allocation's authorized input
			// set become allocation occupations. A named input outside that
			// set is held by the signature duty itself, so settling the duty
			// can release it without fabricating an allocation claim.
			if allocation != nil && containsString(allocation.Inputs, key) {
				if held.OccupiedBy == nil {
					holder := copy.AllocationID
					held.OccupiedBy = &holder
				}
			} else if held.Isolated == nil {
				reason := "external-signature-quarantine"
				held.Isolated = &reason
			}
			if incomplete && held.Isolated == nil {
				reason := "external-signature-quarantine"
				held.Isolated = &reason
			}
		}
	}
	for _, entry := range kernel.outbox {
		if err := kernel.validateInputConclusion(entry); err != nil {
			return nil, err
		}
	}
	kernel.fundsFrozen = state.FundsFrozen
	kernel.refreshFundsFrozen()
	kernel.generation = state.Generation
	kernel.wocRevision = state.WocRevision
	kernel.localRev = state.LocalRev
	return kernel, nil
}

// StateHash returns the canonical state hash for cross-language comparison.
func (k *FundingKernel) StateHash() (string, error) {
	serialized, err := k.Serialize()
	if err != nil {
		return "", err
	}
	return CalcTxid([]byte(serialized)), nil
}

func uniqueLowerSorted(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		key := strings.ToLower(value)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
