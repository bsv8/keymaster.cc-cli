package utxo

import "fmt"

// InputObservation is a trusted per-outpoint fact; HTTP parsing stays in hosts.
type InputObservation struct {
	Outpoint  string  `json:"outpoint"`
	Status    string  `json:"status"`
	TxID      string  `json:"txid,omitempty"`
	Vin       *uint32 `json:"vin,omitempty"`
	Confirmed *bool   `json:"confirmed,omitempty"`
}
type SpentInputFact struct {
	Outpoint  string  `json:"outpoint"`
	TxID      string  `json:"txid"`
	Vin       *uint32 `json:"vin"`
	Confirmed *bool   `json:"confirmed"`
}
type InputConclusion struct {
	Status         string           `json:"status"`
	Reason         string           `json:"reason"`
	ReleasedInputs []string         `json:"releasedInputs"`
	SpentInputs    []SpentInputFact `json:"spentInputs"`
	HeldInputs     []string         `json:"heldInputs"`
	ReleasedSats   string           `json:"releasedSats"`
}

func cloneInputConclusion(value *InputConclusion) *InputConclusion {
	if value == nil {
		return nil
	}
	copy := *value
	copy.ReleasedInputs = append([]string{}, value.ReleasedInputs...)
	copy.HeldInputs = append([]string{}, value.HeldInputs...)
	copy.SpentInputs = append([]SpentInputFact{}, value.SpentInputs...)
	for i := range copy.SpentInputs {
		if copy.SpentInputs[i].Vin != nil {
			v := *copy.SpentInputs[i].Vin
			copy.SpentInputs[i].Vin = &v
		}
		if copy.SpentInputs[i].Confirmed != nil {
			v := *copy.SpentInputs[i].Confirmed
			copy.SpentInputs[i].Confirmed = &v
		}
	}
	return &copy
}
func inputConclusion(status string, keys []string) *InputConclusion {
	return &InputConclusion{Status: status, Reason: "", ReleasedInputs: []string{}, SpentInputs: []SpentInputFact{}, HeldInputs: append([]string{}, keys...), ReleasedSats: "0"}
}

func (k *FundingKernel) validateInputConclusion(entry OutboxEntry) error {
	c := entry.Conclusion
	if c == nil {
		if entry.State == "failed" {
			return fail(ErrStateConflict, "failed outbox lacks conclusion")
		}
		return nil
	}
	invalid := func() error { return fail(ErrStateConflict, "invalid persisted input conclusion") }
	if (c.Status != "conflict" && c.Status != "failed") || c.Reason != "input-conflict" || c.ReleasedInputs == nil || c.SpentInputs == nil || c.HeldInputs == nil || len(c.SpentInputs) == 0 || entry.AllocationID == nil || entry.Delivery != "wallet" {
		return invalid()
	}
	allocation := k.allocations[*entry.AllocationID]
	expectedAllocation, expectedOutbox := "held", "unknown"
	if c.Status == "failed" {
		expectedAllocation, expectedOutbox = "aborted", "failed"
	}
	if allocation == nil || allocation.SignedTxid == nil || *allocation.SignedTxid != entry.TxID || allocation.State != expectedAllocation || entry.State != expectedOutbox || ((len(c.HeldInputs) == 0) != (c.Status == "failed")) {
		return invalid()
	}
	parsed, err := ParseTx(entry.RawTxHex, entry.TxID)
	if err != nil {
		return invalid()
	}
	keys := map[string]bool{}
	for _, input := range parsed.Inputs {
		keys[input.OutpointKey] = true
	}
	if len(keys) != len(allocation.Inputs) {
		return invalid()
	}
	for key := range keys {
		if !containsString(allocation.Inputs, key) {
			return invalid()
		}
	}
	seen := map[string]bool{}
	add := func(key string) bool {
		if !keys[key] || seen[key] {
			return false
		}
		seen[key] = true
		return true
	}
	var amount uint64
	for _, key := range c.ReleasedInputs {
		if !add(key) {
			return invalid()
		}
		row := k.utxos[key]
		if row == nil || (row.OccupiedBy != nil && *row.OccupiedBy == *entry.AllocationID) {
			return invalid()
		}
		if row.ScriptHex == k.config.WalletScriptHex {
			amount, err = AddChecked(amount, row.Value, "released amount")
			if err != nil {
				return invalid()
			}
		}
	}
	for _, fact := range c.SpentInputs {
		if !txidPattern.MatchString(fact.TxID) || fact.TxID == entry.TxID || !add(fact.Outpoint) {
			return invalid()
		}
		row := k.utxos[fact.Outpoint]
		if row == nil || row.SpentBy == nil || *row.SpentBy != fact.TxID {
			return invalid()
		}
	}
	for _, key := range c.HeldInputs {
		row := k.utxos[key]
		if row == nil || ((row.OccupiedBy == nil || *row.OccupiedBy != *entry.AllocationID) && !row.Protected && row.Isolated == nil) {
			return invalid()
		}
		if !add(key) {
			return invalid()
		}
	}
	if len(seen) != len(keys) || c.ReleasedSats != mustFormat(amount) {
		return invalid()
	}
	return nil
}

// ResolveInputObservations runs on a detached candidate; callers persist before
// publication. A foreign spender disproves this exact, but never frees unknown
// inputs, protected funds or outstanding external signature responsibilities.
func (k *FundingKernel) ResolveInputObservations(commandID string, observations []InputObservation) (*InputConclusion, error) {
	var entry *OutboxEntry
	for i := range k.outbox {
		if k.outbox[i].CommandID == commandID {
			entry = &k.outbox[i]
			break
		}
	}
	if entry == nil {
		return nil, fail(ErrAllocationUnknown, "outbox command not found")
	}
	if entry.State == "done" {
		return inputConclusion("accepted", nil), nil
	}
	if entry.State == "failed" && entry.Conclusion != nil {
		return cloneInputConclusion(entry.Conclusion), nil
	}
	if entry.AllocationID == nil || entry.Delivery != "wallet" {
		return nil, fail(ErrPolicyDenied, "input resolution requires wallet allocation")
	}
	id := *entry.AllocationID
	allocation := k.allocations[id]
	if allocation == nil || allocation.SignedTxid == nil || *allocation.SignedTxid != entry.TxID || allocation.State != "held" {
		return nil, fail(ErrAllocationState, "input resolution requires original signed allocation")
	}
	parsed, err := ParseTx(entry.RawTxHex, entry.TxID)
	if err != nil {
		return nil, err
	}
	keys := []string{}
	positions := map[string]uint32{}
	for i, input := range parsed.Inputs {
		keys = append(keys, input.OutpointKey)
		positions[input.OutpointKey] = uint32(i)
	}
	if len(keys) != len(allocation.Inputs) {
		return nil, fail(ErrPolicyDenied, "exact inputs do not match allocation responsibility")
	}
	for _, key := range keys {
		if !containsString(allocation.Inputs, key) {
			return nil, fail(ErrPolicyDenied, "exact inputs do not match allocation responsibility")
		}
	}
	supplied := map[string]InputObservation{}
	for _, fact := range observations {
		index, exists := positions[fact.Outpoint]
		_, duplicate := supplied[fact.Outpoint]
		if !exists || duplicate || (fact.Status != "spent" && fact.Status != "unspent" && fact.Status != "unknown") {
			return nil, fail(ErrPolicyDenied, "invalid input observation set")
		}
		if fact.Status == "spent" {
			canonical, err := CheckTxid(fact.TxID)
			if err != nil || canonical != fact.TxID || fact.Vin == nil || fact.Confirmed == nil {
				return nil, fail(ErrPolicyDenied, "invalid spender observation")
			}
			if fact.TxID == entry.TxID && *fact.Vin != index {
				return nil, fail(ErrPolicyDenied, "self spender vin mismatch")
			}
		} else if fact.TxID != "" || fact.Vin != nil || fact.Confirmed != nil {
			return nil, fail(ErrPolicyDenied, "non-spent observation carries spender")
		}
		supplied[fact.Outpoint] = fact
	}
	priorSpent := map[string]SpentInputFact{}
	released := map[string]bool{}
	if entry.Conclusion != nil {
		for _, fact := range entry.Conclusion.SpentInputs {
			priorSpent[fact.Outpoint] = fact
		}
		for _, key := range entry.Conclusion.ReleasedInputs {
			released[key] = true
		}
	}
	spent := []SpentInputFact{}
	self, other := false, false
	for _, key := range keys {
		if released[key] {
			continue
		}
		fact, hasFact := supplied[key]
		local := ""
		if row := k.utxos[key]; row != nil && row.SpentBy != nil {
			local = *row.SpentBy
		} else if prior, ok := priorSpent[key]; ok {
			local = prior.TxID
		}
		if hasFact && fact.Status == "spent" && fact.TxID == entry.TxID {
			self = true
		}
		if (local != "" && local != entry.TxID) || (hasFact && fact.Status == "spent" && fact.TxID != entry.TxID) {
			other = true
		}
		if local == entry.TxID {
			self = true
		}
		if local != "" && local != entry.TxID {
			previous, ok := priorSpent[key]
			if !ok {
				previous = SpentInputFact{Outpoint: key, TxID: local}
			}
			if hasFact && fact.Status == "spent" && fact.TxID == previous.TxID {
				if previous.Vin != nil && *previous.Vin != *fact.Vin {
					return nil, fail(ErrStateConflict, "spender vin changed")
				}
				confirmed := (previous.Confirmed != nil && *previous.Confirmed) || *fact.Confirmed
				previous.Vin = fact.Vin
				previous.Confirmed = &confirmed
			}
			spent = append(spent, previous)
		} else if hasFact && fact.Status == "spent" && fact.TxID != entry.TxID {
			spent = append(spent, SpentInputFact{Outpoint: key, TxID: fact.TxID, Vin: fact.Vin, Confirmed: fact.Confirmed})
		}
	}
	if self && other {
		return nil, fail(ErrStateConflict, "inconsistent self/other spender evidence")
	}
	if self {
		if err := k.BroadcastResult(commandID, BroadcastReport{Status: "accepted", TxID: entry.TxID}); err != nil {
			return nil, err
		}
		return inputConclusion("accepted", nil), nil
	}
	if !other {
		return inputConclusion("unknown", keys), nil
	}
	for _, fact := range spent {
		if err := k.ReportExternalSpend(fact.Outpoint, fact.TxID); err != nil {
			return nil, err
		}
	}
	for index, out := range parsed.Outputs {
		if out.Script != k.config.WalletScriptHex {
			continue
		}
		key := fmt.Sprintf("%s:%d", entry.TxID, index)
		reason := "commit-conflict:" + entry.TxID
		if row := k.utxos[key]; row != nil {
			if row.Isolated == nil {
				row.Isolated = &reason
			}
		} else {
			creating := entry.TxID
			k.utxos[key] = &storedUtxo{Value: out.Value, ScriptHex: out.Script, Status: "unconfirmed", Source: "local", CreatingTxid: &creating, ParentInputs: append([]string{}, keys...), ProtectionSources: []string{}, Isolated: &reason}
		}
		k.isolateDescendants(key, reason)
	}
	spentKeys := map[string]bool{}
	for _, fact := range spent {
		spentKeys[fact.Outpoint] = true
	}
	held := []string{}
	for _, key := range keys {
		if spentKeys[key] || released[key] {
			continue
		}
		row := k.utxos[key]
		fact := supplied[key]
		duty := false
		for _, record := range k.externalSignatures {
			if record.State != "settled" && (containsString(record.RelatedInputs, key) || containsString(record.ArtifactInputs, key)) {
				duty = true
				break
			}
		}
		if fact.Status == "unspent" && row != nil && row.SpentBy == nil && row.OccupiedBy != nil && *row.OccupiedBy == id && !row.Protected && row.Isolated == nil && row.PendingParent == nil && !duty {
			row.OccupiedBy = nil
			if row.ScriptHex != k.config.WalletScriptHex || containsString(k.SpendableOutpoints(), key) {
				released[key] = true
			} else {
				holder := id
				row.OccupiedBy = &holder
			}
		}
		if !released[key] {
			held = append(held, key)
		}
	}
	releasedInputs := []string{}
	var releasedSats uint64
	for _, key := range keys {
		if released[key] {
			releasedInputs = append(releasedInputs, key)
			if row := k.utxos[key]; row != nil && row.ScriptHex == k.config.WalletScriptHex {
				releasedSats, err = AddChecked(releasedSats, row.Value, "released amount")
				if err != nil {
					return nil, err
				}
			}
		}
	}
	status := "conflict"
	entry.State = "unknown"
	if len(held) == 0 {
		status = "failed"
		entry.State = "failed"
		allocation.State = "aborted"
	}
	entry.Conclusion = cloneInputConclusion(&InputConclusion{Status: status, Reason: "input-conflict", ReleasedInputs: releasedInputs, SpentInputs: spent, HeldInputs: held, ReleasedSats: mustFormat(releasedSats)})
	k.localRev++
	return cloneInputConclusion(entry.Conclusion), nil
}
