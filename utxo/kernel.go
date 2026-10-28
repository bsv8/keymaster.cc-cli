package utxo

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Scope identifies one wallet in one network.
type Scope struct {
	Owner   string `json:"owner"`
	Network string `json:"network"`
}

// ManagerConfig configures one kernel instance.
type ManagerConfig struct {
	WalletScriptHex         string
	DustThresholdSats       string
	MaxUnconfirmedAncestors int
}

// SnapshotUtxo is one WoC-reported output.
type SnapshotUtxo struct {
	TxID      string `json:"txid"`
	Vout      uint32 `json:"vout"`
	Value     string `json:"value"`
	ScriptHex string `json:"scriptHex"`
	Status    string `json:"status"`
	Height    int64  `json:"height"`
}

// SnapshotInput is one validated WoC snapshot.
type SnapshotInput struct {
	Generation int64
	Revision   int64
	Utxos      []SnapshotUtxo
}

// RequestOutput is one requested business output.
type RequestOutput struct {
	ScriptHex string `json:"scriptHex"`
	Value     string `json:"value"`
}

// RequestInput is one funding request.
type RequestInput struct {
	RequestID      string
	BusinessAmount string
	FeeRate        string
	Outputs        []RequestOutput
	Version        uint32
	HasVersion     bool
	Locktime       uint32
	HasLocktime    bool
}

// ExternalProof is trusted prevout evidence for a non-wallet input.
type ExternalProof struct {
	Value     string `json:"value"`
	ScriptHex string `json:"scriptHex"`
}

// ProtocolBinding marks refund/close spends.
type ProtocolBinding struct {
	Kind   string `json:"kind"`
	PoolID string `json:"poolId"`
}

// AllocationView is the public allocation state.
type AllocationView struct {
	AllocationID string   `json:"allocationId"`
	RequestID    string   `json:"requestId"`
	Inputs       []string `json:"inputs"`
	TotalInSats  string   `json:"totalInSats"`
	BusinessSats string   `json:"businessSats"`
	FeeSats      string   `json:"feeSats"`
	ChangeSats   string   `json:"changeSats"`
	State        string   `json:"state"`
}

// OutboxEntry is one persisted broadcast command.
type OutboxEntry struct {
	CommandID    string  `json:"commandId"`
	AllocationID *string `json:"allocationId"`
	TxID         string  `json:"txid"`
	RawTxHex     string  `json:"rawTxHex"`
	State        string  `json:"state"`
	// Delivery says who must put this exact transaction on the network:
	// "wallet" = this wallet's provider, "external" = the signed bytes were
	// handed outside and may already be broadcast by the receiver. The two
	// are never interchangeable.
	Delivery   string           `json:"delivery"`
	Conclusion *InputConclusion `json:"conclusion,omitempty"`
}

// ProtocolInputClass classifies one actual input of a protocol transaction.
const (
	ProtocolInputWallet    = "wallet"
	ProtocolInputProtected = "protected"
	ProtocolInputExternal  = "external"
)

// ProtocolBindingKind marks the protocol role of a signed transaction.
const (
	ProtocolKindRefund = "refund"
	ProtocolKindClose  = "close"
	ProtocolKindFund   = "fund"
	ProtocolKindState  = "state"
)

// Protocol fee policies: "rate" imposes a rate x size floor, "exact" only
// applies the authorized ceiling because the business approved a fixed fee.
const (
	ProtocolFeePolicyRate  = "rate"
	ProtocolFeePolicyExact = "exact"
)

// ProtocolInputFact is the trusted classification of one actual input.
type ProtocolInputFact struct {
	Outpoint  string  `json:"outpoint"`
	Class     string  `json:"class"`
	Value     string  `json:"value"`
	ScriptHex string  `json:"scriptHex"`
	ClaimID   *string `json:"claimId"`
}

// ExternalSignatureRecord is a durable responsibility for a partial
// signature that left the wallet.
type ExternalSignatureRecord struct {
	ID            string           `json:"id"`
	AllocationID  string           `json:"allocationId"`
	Scope         string           `json:"scope"`
	Binding       *ProtocolBinding `json:"binding"`
	DraftTxHex    string           `json:"draftTxHex"`
	SignatureHex  string           `json:"signatureHex"`
	RelatedInputs []string         `json:"relatedInputs"`
	// ArtifactInputs are the inputs the signed artifact actually spends,
	// parsed from the exact draft. The responsibility follows the artifact,
	// not the caller's metadata.
	ArtifactInputs []string `json:"artifactInputs"`
	Reason         string   `json:"reason"`
	// State is "responsible" (the signature may still be completed and
	// spent), "superseded" (a newer round exists, but the old artifact is
	// still valid and keeps its responsibility) or "settled" (trusted
	// evidence retired it).
	State string `json:"state"`
	// SupersededBy names the record that legitimately replaced this one.
	SupersededBy *string `json:"supersededBy,omitempty"`
	// SettledTxid is the trusted evidence that retired the responsibility.
	SettledTxid *string `json:"settledTxid,omitempty"`
}

// ResponsibilityView is the human-readable responsibility report.
type ResponsibilityView struct {
	CommandID    string                    `json:"commandId"`
	AllocationID *string                   `json:"allocationId"`
	TxID         string                    `json:"txid"`
	State        string                    `json:"state"`
	Delivery     string                    `json:"delivery"`
	Reason       string                    `json:"reason"`
	Inputs       []string                  `json:"inputs"`
	TotalInSats  string                    `json:"totalInSats"`
	OwnOutputs   []ResponsibilityOwnOutput `json:"ownOutputs"`
}

// ResponsibilityOwnOutput is one self output of a responsible transaction.
type ResponsibilityOwnOutput struct {
	Outpoint string `json:"outpoint"`
	Value    string `json:"value"`
}

type storedUtxo struct {
	Value         uint64
	ScriptHex     string
	Status        string
	Height        int64
	Source        string
	CreatingTxid  *string
	ParentInputs  []string
	Confirmed     bool
	SpentBy       *string
	SpentExternal bool
	OccupiedBy    *string
	Protected     bool
	// ProtectionSources records why an output is protected, one entry per
	// source. A protocol submission's own input claim is an occupation, not a
	// fund protection; a dedicated funding record is a real protection.
	ProtectionSources []string
	Isolated          *string
	// PendingParent marks outputs of an unsettled imported broadcast: they
	// are adopted but not selectable until the entry settles.
	PendingParent *string
}

type storedOutput struct {
	ScriptHex string
	Value     uint64
}

type storedProof struct {
	Value     uint64
	ScriptHex string
}

type storedAllocation struct {
	RequestID            string
	RequestHash          string
	Inputs               []string
	Business             uint64
	FeeRate              uint64
	Fee                  uint64
	Change               uint64
	Outputs              []storedOutput
	Version              uint32
	Locktime             uint32
	ExternalProofs       map[string]storedProof
	ProtocolBinding      *ProtocolBinding
	ProtocolInputs       []ProtocolInputFact
	ProtocolFeeCapSats   string
	ProtocolFeePolicy    string
	ProtocolClaims       []string
	SignedTxid           *string
	SigningIntentHex     string
	SigningIntentDurable bool
	State                string
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9:_.-]+$`)

func checkID(id string) (string, error) {
	if id == "" || len(id) > 128 || !idPattern.MatchString(id) {
		return "", fail(ErrPolicyDenied, "bad request/allocation id")
	}
	return id, nil
}

func checkScriptHex(scriptHex, what string) (string, error) {
	clean := scriptHex
	if strings.HasPrefix(clean, "0x") || strings.HasPrefix(clean, "0X") {
		clean = clean[2:]
	}
	if len(clean)%2 != 0 || len(clean)/2 > 10000 {
		return "", fail(ErrTxInvalid, what+" is not valid script hex")
	}
	for i := 0; i < len(clean); i++ {
		c := clean[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return "", fail(ErrTxInvalid, what+" is not valid script hex")
		}
	}
	return strings.ToLower(clean), nil
}

func outpointKey(txid string, vout uint32) (string, error) {
	if _, err := CheckTxid(txid); err != nil {
		return "", err
	}
	return strings.ToLower(txid) + ":" + strconv.FormatUint(uint64(vout), 10), nil
}

func splitOutpoint(key string) (string, uint32, error) {
	parts := strings.Split(key, ":")
	if len(parts) != 2 {
		return "", 0, fail(ErrTxInvalid, "bad outpoint")
	}
	vout64, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil || parts[1] != strconv.FormatUint(vout64, 10) {
		return "", 0, fail(ErrTxInvalid, "bad vout")
	}
	if _, err := CheckTxid(parts[0]); err != nil {
		return "", 0, err
	}
	return parts[0], uint32(vout64), nil
}

// UnsignedSkeleton rebuilds the transaction without its unlocking scripts.
// Hosts use it to bind a protocol intent to a signed transaction.
func UnsignedSkeleton(rawTxHex string) (string, error) { return unsignedSkeleton(rawTxHex) }

// isOutputLevelIsolation reports the isolation reasons that are scoped to the
// outpoint that carries them: the transaction that created or spent it stays
// valid, and only that outpoint (plus the descendants that depend on it) goes
// out of service.
//
//   - "conflict:<txid>": two parties claim to spend the same outpoint.
//   - "woc-conflict:<value>:<script>": the chain reports different facts for
//     this outpoint than the local record holds.
//   - "local-conflict:<outpoint>": two local records disagree about this
//     outpoint.
//   - "external-spend-unknown": the chain reports an unknown spender and the
//     outpoint's own facts are unknown.
//   - "protected-unknown": a protected placeholder whose facts are unknown.
//   - "external-signature-quarantine": the outpoint is held for an external
//     signature responsibility.
//
// Only an explicitly listed reason may keep the sibling branch usable.
func isOutputLevelIsolation(reason string) bool {
	return strings.HasPrefix(reason, "conflict:") ||
		strings.HasPrefix(reason, "woc-conflict:") ||
		strings.HasPrefix(reason, "local-conflict:") ||
		reason == "external-spend-unknown" ||
		reason == "protected-unknown" ||
		reason == "external-signature-quarantine"
}

// isTransactionLevelIsolation reports the isolation reasons that mean the
// *transaction* that owns the outpoint is broken, so the transaction and
// everything descending from it is invalid.
//
//   - "commit-conflict:<txid>": the transaction lost the race for one of its
//     inputs, so its change is quarantined.
//   - "ancestor-conflict:<outpoint>": the outpoint descends from a conflicted
//     outpoint, so its creating transaction is invalid too.
func isTransactionLevelIsolation(reason string) bool {
	return strings.HasPrefix(reason, "commit-conflict:") || strings.HasPrefix(reason, "ancestor-conflict:")
}

// FundingKernel is the deterministic funding kernel (utm/1).
type FundingKernel struct {
	scope              Scope
	config             ManagerConfig
	dust               uint64
	utxos              map[string]*storedUtxo
	allocations        map[string]*storedAllocation
	outbox             []OutboxEntry
	externalSignatures map[string]*ExternalSignatureRecord
	// fundsFrozen blocks new allocations/signing when a migrated
	// responsibility cannot be scoped. Snapshots and trusted observations
	// stay available so the host can resolve it.
	fundsFrozen *string
	generation  int64
	wocRevision int64
	localRev    int64
}

// NewFundingKernel creates a kernel for one scope.
func NewFundingKernel(scope Scope, config ManagerConfig) (*FundingKernel, error) {
	if scope.Owner == "" {
		return nil, fail(ErrScopeMismatch, "scope owner is required")
	}
	if scope.Network != "main" && scope.Network != "test" {
		return nil, fail(ErrScopeMismatch, "bad network")
	}
	script, err := checkScriptHex(config.WalletScriptHex, "wallet script")
	if err != nil {
		return nil, err
	}
	dust, err := ParseAmount(config.DustThresholdSats, "dust threshold")
	if err != nil {
		return nil, err
	}
	if config.MaxUnconfirmedAncestors < 0 {
		return nil, fail(ErrPolicyDenied, "bad maxUnconfirmedAncestors")
	}
	return &FundingKernel{
		scope:              scope,
		config:             ManagerConfig{WalletScriptHex: script, DustThresholdSats: mustFormat(dust), MaxUnconfirmedAncestors: config.MaxUnconfirmedAncestors},
		dust:               dust,
		utxos:              map[string]*storedUtxo{},
		allocations:        map[string]*storedAllocation{},
		externalSignatures: map[string]*ExternalSignatureRecord{},
	}, nil
}

// Scope returns the kernel scope.
func (k *FundingKernel) Scope() Scope { return k.scope }

// Config returns a detached configuration value, equivalent to TS config.
func (k *FundingKernel) Config() ManagerConfig { return k.config }

// assertFundsNotFrozen refuses new allocations/signing while a migrated
// responsibility cannot be scoped. Reads, snapshots, trusted observations and
// settlement remain available so the host can resolve the quarantine.
func (k *FundingKernel) assertFundsNotFrozen(operation string) error {
	if k.fundsFrozen != nil {
		return fail(ErrStateConflict, "cannot "+operation+": scope funds are frozen ("+*k.fundsFrozen+")")
	}
	return nil
}

// releaseOrphanSignatureQuarantine releases quarantine held only by settled
// signature duties. A normal snapshot or protection refresh never clears it.
func (k *FundingKernel) releaseOrphanSignatureQuarantine() {
	covered := map[string]bool{}
	for _, row := range k.externalSignatures {
		if row.State == "settled" {
			continue
		}
		for _, key := range row.RelatedInputs {
			covered[key] = true
		}
		for _, key := range row.ArtifactInputs {
			covered[key] = true
		}
	}
	for key, row := range k.utxos {
		if row.Isolated == nil || *row.Isolated != "external-signature-quarantine" || covered[key] {
			continue
		}
		if row.Value == 0 && row.ScriptHex == "" && !row.Protected && row.OccupiedBy == nil {
			delete(k.utxos, key)
		} else {
			row.Isolated = nil
		}
	}
}

// refreshFundsFrozen re-derives the migration freeze after a responsibility
// became terminal. The lexicographically first quarantined id keeps the
// serialized reason deterministic across map iteration order.
func (k *FundingKernel) refreshFundsFrozen() {
	if k.fundsFrozen != nil && !strings.HasPrefix(*k.fundsFrozen, "migration:") {
		return
	}
	first := ""
	for _, row := range k.externalSignatures {
		if row.State == "settled" || row.State != "quarantined" {
			continue
		}
		if first == "" || row.ID < first {
			first = row.ID
		}
	}
	if first == "" {
		k.fundsFrozen = nil
		return
	}
	reason := "migration:" + first
	k.fundsFrozen = &reason
}

// Clone returns a detached candidate state for validate-persist-publish flows.
func (k *FundingKernel) Clone() (*FundingKernel, error) {
	blob, err := k.Serialize()
	if err != nil {
		return nil, err
	}
	return Restore(blob, k.scope)
}

// ReplaceFrom publishes a previously validated and durably persisted candidate.
func (k *FundingKernel) ReplaceFrom(candidate *FundingKernel) error {
	if candidate == nil || candidate.scope != k.scope || candidate.config != k.config {
		return fail(ErrScopeMismatch, "candidate manager scope/config does not match")
	}
	blob, err := candidate.Serialize()
	if err != nil {
		return err
	}
	restored, err := Restore(blob, k.scope)
	if err != nil {
		return err
	}
	k.utxos = restored.utxos
	k.allocations = restored.allocations
	k.outbox = restored.outbox
	k.externalSignatures = restored.externalSignatures
	k.fundsFrozen = restored.fundsFrozen
	k.generation = restored.generation
	k.wocRevision = restored.wocRevision
	k.localRev = restored.localRev
	return nil
}

// MarkSigningIntentDurable records that a validated template was persisted
// before the trusted signer was allowed to run.
func (k *FundingKernel) MarkSigningIntentDurable(allocationID string) error {
	if err := k.assertFundsNotFrozen("mark a signing intent durable"); err != nil {
		return err
	}
	id, err := checkID(allocationID)
	if err != nil {
		return err
	}
	record, ok := k.allocations[id]
	if !ok || record.SigningIntentHex == "" {
		return fail(ErrAllocationState, "signing intent was not prepared")
	}
	record.SigningIntentDurable = true
	k.localRev++
	return nil
}

// SigningIntent returns the frozen template and its durability marker.
func (k *FundingKernel) SigningIntent(allocationID string) (string, bool, error) {
	id, err := checkID(allocationID)
	if err != nil {
		return "", false, err
	}
	record, ok := k.allocations[id]
	if !ok {
		return "", false, fail(ErrAllocationUnknown, "allocation not found")
	}
	return record.SigningIntentHex, record.SigningIntentDurable, nil
}

// SnapshotRevision returns the current WoC revision, for hosts that
// persist the snapshot cursor across restarts.
func (k *FundingKernel) SnapshotRevision() int64 {
	return k.wocRevision
}

// SnapshotResult reports a snapshot merge.
type SnapshotResult struct {
	Added    int      `json:"added"`
	Removed  int      `json:"removed"`
	Isolated []string `json:"isolated"`
}

// ApplySnapshot merges a validated WoC snapshot.
func (k *FundingKernel) ApplySnapshot(input SnapshotInput) (*SnapshotResult, error) {
	if input.Generation <= 0 {
		return nil, fail(ErrPolicyDenied, "bad generation")
	}
	if input.Revision <= 0 {
		return nil, fail(ErrPolicyDenied, "bad revision")
	}
	if input.Generation < k.generation {
		return nil, fail(ErrSnapshotStale, "older generation")
	}
	if input.Generation == k.generation && input.Revision <= k.wocRevision {
		return nil, fail(ErrSnapshotStale, "revision did not advance")
	}
	type incomingRow struct {
		key       string
		txid      string
		vout      uint32
		value     uint64
		valueStr  string
		scriptHex string
		status    string
		height    int64
	}
	order := []string{}
	byKey := map[string]*incomingRow{}
	for _, row := range input.Utxos {
		key, err := outpointKey(row.TxID, row.Vout)
		if err != nil {
			return nil, err
		}
		value, err := ParseAmount(row.Value, "utxo value")
		if err != nil {
			return nil, err
		}
		scriptHex, err := checkScriptHex(row.ScriptHex, "utxo script")
		if err != nil {
			return nil, err
		}
		if row.Status != "confirmed" && row.Status != "unconfirmed" {
			return nil, fail(ErrPolicyDenied, "bad utxo status")
		}
		if row.Height < 0 {
			return nil, fail(ErrPolicyDenied, "bad utxo height")
		}
		if prev, ok := byKey[key]; ok {
			if prev.valueStr != row.Value || prev.scriptHex != scriptHex {
				return nil, fail(ErrOutpointConflict, "conflicting duplicate "+key)
			}
			continue
		}
		byKey[key] = &incomingRow{key: key, txid: strings.ToLower(row.TxID), vout: row.Vout, value: value, valueStr: row.Value, scriptHex: scriptHex, status: row.Status, height: row.Height}
		order = append(order, key)
	}
	result := &SnapshotResult{Isolated: []string{}}
	for _, key := range order {
		row := byKey[key]
		existing, ok := k.utxos[key]
		if ok {
			if existing.SpentBy != nil {
				continue
			}
			if existing.Protected && existing.Isolated != nil && *existing.Isolated == "protected-unknown" && existing.Value == 0 && existing.ScriptHex == "" {
				existing.Value = row.value
				existing.ScriptHex = row.scriptHex
				existing.Status = row.status
				existing.Height = row.height
				existing.Confirmed = row.status == "confirmed" && row.height > 0
				existing.Isolated = nil
				continue
			}
			if existing.Isolated != nil && existing.Value == 0 && existing.ScriptHex == "" {
				// A quarantined placeholder: adopt the real facts but keep the
				// isolation. A later normal snapshot must never clear it.
				existing.Value = row.value
				existing.ScriptHex = row.scriptHex
				existing.Status = row.status
				existing.Height = row.height
				existing.Confirmed = row.status == "confirmed" && row.height > 0
				continue
			}
			if mustFormat(existing.Value) != row.valueStr || existing.ScriptHex != row.scriptHex {
				reason := "woc-conflict:" + row.valueStr + ":" + row.scriptHex[:min(16, len(row.scriptHex))]
				existing.Isolated = &reason
				result.Isolated = append(result.Isolated, key)
				continue
			}
			existing.Status = row.status
			existing.Height = row.height
			if existing.Source == "local" && existing.CreatingTxid == nil {
				existing.Source = "woc"
			}
			existing.Confirmed = row.status == "confirmed" && row.height > 0
			continue
		}
		k.utxos[key] = &storedUtxo{Value: row.value, ScriptHex: row.scriptHex, Status: row.status, Height: row.height, Source: "woc", Confirmed: row.status == "confirmed" && row.height > 0}
		result.Added++
	}
	for key, existing := range k.utxos {
		// Snapshot-first change keeps WoC provenance after exact settlement.
		// The trusted creating transaction, not provenance, preserves it when
		// a later snapshot temporarily omits this output.
		if existing.Source == "local" || existing.CreatingTxid != nil || existing.SpentBy != nil {
			continue
		}
		if _, ok := byKey[key]; !ok && existing.Isolated == nil {
			if existing.OccupiedBy == nil && !existing.Protected {
				delete(k.utxos, key)
				result.Removed++
			}
		}
	}
	k.generation = input.Generation
	k.wocRevision = input.Revision
	k.localRev++
	return result, nil
}

// ReportExternalSpend tombstones an outpoint spent by another transaction.
// If another tx already spent it, the conflict is isolated transitively
// (the outpoint plus all unconfirmed local descendants) instead of
// throwing: a network fact is never a caller error. The first-seen spend
// stays canonical.
func (k *FundingKernel) ReportExternalSpend(outpoint, spendingTxid string) error {
	key := strings.ToLower(outpoint)
	if _, _, err := splitOutpoint(key); err != nil {
		return err
	}
	lowerSpending, err := CheckTxid(spendingTxid)
	if err != nil {
		return err
	}
	lowerSpending = strings.ToLower(lowerSpending)
	if existing, ok := k.utxos[key]; ok {
		if existing.SpentBy != nil && *existing.SpentBy != lowerSpending {
			reason := "conflict:" + lowerSpending
			existing.Isolated = &reason
			k.isolateDescendants(key, "ancestor-conflict:"+key)
		} else if existing.SpentBy == nil {
			existing.SpentBy = &lowerSpending
			existing.SpentExternal = true
			existing.OccupiedBy = nil
		}
	} else {
		reason := "external-spend-unknown"
		k.utxos[key] = &storedUtxo{Status: "unconfirmed", Source: "woc", SpentBy: &lowerSpending, SpentExternal: true, Isolated: &reason}
	}
	k.localRev++
	return nil
}

// isolateDescendants isolates an outpoint and all unconfirmed local outputs
// descending from it. Spent outputs are traversed but keep their state;
// confirmed history stands.
func (k *FundingKernel) isolateDescendants(rootOutpoint, reason string) {
	seen := map[string]bool{rootOutpoint: true}
	seenTxids := map[string]bool{}
	if root, ok := k.utxos[rootOutpoint]; ok && root.SpentBy != nil {
		seenTxids[*root.SpentBy] = true
	}
	grown := true
	for grown {
		grown = false
		for key, row := range k.utxos {
			if seen[key] {
				continue
			}
			// Dependency, not provenance: a WoC-seen output of a known parent
			// is still a descendant. creatingTxid covers outputs whose parent
			// link arrived after the snapshot.
			parentMatch := false
			for _, parent := range row.ParentInputs {
				if seen[parent] {
					parentMatch = true
					break
				}
			}
			depends := parentMatch || (row.CreatingTxid != nil && seenTxids[*row.CreatingTxid])
			if !depends {
				continue
			}
			seen[key] = true
			grown = true
			if row.CreatingTxid != nil {
				seenTxids[*row.CreatingTxid] = true
			}
			if row.SpentBy != nil || row.Isolated != nil || row.Confirmed {
				continue
			}
			row.Isolated = &reason
			row.OccupiedBy = nil
		}
	}
}

// SetProtected marks outpoints as protocol-protected (or clears them).
// SetProtected protects or releases outputs, remembering *why* each one is
// protected. `source` identifies the protection provider (for example
// "protocol-claim:<submissionId>" or "protocol-funding:<fundingId>"). Sources
// accumulate: an outpoint protected by two providers keeps both, so taking
// over one of them never drops the other.
func (k *FundingKernel) SetProtected(outpoints []string, protect bool, source string) error {
	for _, raw := range outpoints {
		key := strings.ToLower(raw)
		txid, vout, err := splitOutpoint(key)
		if err != nil {
			return err
		}
		canonical, err := outpointKey(txid, vout)
		if err != nil {
			return err
		}
		if existing, ok := k.utxos[canonical]; ok {
			if !protect {
				if existing.Isolated != nil && *existing.Isolated == "protected-unknown" && existing.Value == 0 && existing.ScriptHex == "" {
					delete(k.utxos, canonical)
					continue
				}
				existing.Protected = false
				existing.ProtectionSources = []string{}
				if existing.Isolated != nil && *existing.Isolated == "protected-unknown" {
					existing.Isolated = nil
				}
				continue
			}
			existing.Protected = true
			found := false
			for _, entry := range existing.ProtectionSources {
				if entry == source {
					found = true
					break
				}
			}
			if !found {
				existing.ProtectionSources = append(existing.ProtectionSources, source)
				sort.Strings(existing.ProtectionSources)
			}
			if existing.Isolated != nil && *existing.Isolated == "protected-unknown" {
				existing.Isolated = nil
			}
		} else if protect {
			reason := "protected-unknown"
			k.utxos[canonical] = &storedUtxo{
				Status: "unconfirmed", Source: "woc", Protected: true, Isolated: &reason,
				ProtectionSources: []string{source},
			}
		}
	}
	k.localRev++
	return nil
}

// ProtectedOutpointKeys returns protected outpoints in canonical order.
func (k *FundingKernel) ProtectedOutpointKeys() []string {
	out := make([]string, 0)
	for key, row := range k.utxos {
		if row.Protected {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// SpendableOutpoints returns the currently selectable outpoints in selection order.
func (k *FundingKernel) SpendableOutpoints() []string {
	candidates := k.candidates()
	out := make([]string, len(candidates))
	for i, candidate := range candidates {
		out[i] = candidate.key
	}
	return out
}

type candidate struct {
	key   string
	value uint64
	txid  string
	vout  uint32
}

func (k *FundingKernel) candidates() []candidate {
	var out []candidate
	if k.fundsFrozen != nil {
		return out
	}
	for key, row := range k.utxos {
		if row.SpentBy != nil || row.OccupiedBy != nil || row.Protected || row.Isolated != nil || row.PendingParent != nil {
			continue
		}
		if row.ScriptHex != k.config.WalletScriptHex {
			continue
		}
		txid, vout, err := splitOutpoint(key)
		if err != nil {
			continue
		}
		out = append(out, candidate{key: key, value: row.Value, txid: txid, vout: vout})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].value != out[j].value {
			return out[i].value > out[j].value
		}
		if out[i].txid != out[j].txid {
			return out[i].txid < out[j].txid
		}
		return out[i].vout < out[j].vout
	})
	return out
}

func stableRequestHash(business, feeRate string, locktime uint32, outputs []RequestOutput, version uint32) string {
	sorted := append([]RequestOutput(nil), outputs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ScriptHex != sorted[j].ScriptHex {
			return sorted[i].ScriptHex < sorted[j].ScriptHex
		}
		return sorted[i].Value < sorted[j].Value
	})
	var sb strings.Builder
	sb.WriteString(`{"businessAmountSats":"`)
	sb.WriteString(business)
	sb.WriteString(`","feeRateSatPerKb":"`)
	sb.WriteString(feeRate)
	sb.WriteString(`","locktime":`)
	sb.WriteString(strconv.FormatUint(uint64(locktime), 10))
	sb.WriteString(`,"outputs":[`)
	for i, o := range sorted {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"scriptHex":"`)
		sb.WriteString(strings.ToLower(o.ScriptHex))
		sb.WriteString(`","value":"`)
		sb.WriteString(o.Value)
		sb.WriteString(`"}`)
	}
	sb.WriteString(`],"version":`)
	sb.WriteString(strconv.FormatUint(uint64(version), 10))
	sb.WriteString(`}`)
	return sb.String()
}

// Request allocates inputs for a business amount plus fee.
func (k *FundingKernel) Request(input RequestInput) (*AllocationView, error) {
	if err := k.assertFundsNotFrozen("request funds"); err != nil {
		return nil, err
	}
	id, err := checkID(input.RequestID)
	if err != nil {
		return nil, err
	}
	business, err := ParseAmount(input.BusinessAmount, "business amount")
	if err != nil {
		return nil, err
	}
	if business == 0 {
		return nil, fail(ErrPolicyDenied, "business amount must be positive")
	}
	feeRate, err := ParseFeeRate(input.FeeRate)
	if err != nil {
		return nil, err
	}
	version := uint32(1)
	if input.HasVersion {
		version = input.Version
	}
	locktime := uint32(0)
	if input.HasLocktime {
		locktime = input.Locktime
	}
	outputs := make([]storedOutput, 0, len(input.Outputs))
	var businessSum uint64
	for _, o := range input.Outputs {
		script, err := checkScriptHex(o.ScriptHex, "output script")
		if err != nil {
			return nil, err
		}
		value, err := ParseAmount(o.Value, "output value")
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, storedOutput{ScriptHex: script, Value: value})
		businessSum, err = AddChecked(businessSum, value, "business outputs")
		if err != nil {
			return nil, err
		}
	}
	if len(outputs) == 0 {
		return nil, fail(ErrPolicyDenied, "at least one business output is required")
	}
	if businessSum > business {
		return nil, fail(ErrPolicyDenied, "business outputs exceed authorized amount")
	}
	hash := stableRequestHash(input.BusinessAmount, input.FeeRate, locktime, input.Outputs, version)
	if existing, ok := k.allocations[id]; ok {
		if existing.RequestHash == hash {
			return k.view(id, existing)
		}
		return nil, fail(ErrIDConflict, "same id with different content")
	}
	pool := k.candidates()
	if len(pool) == 0 {
		return nil, fail(ErrNoUTXOs, "no spendable utxos")
	}
	scriptLens := make([]int, 0, len(outputs)+1)
	for _, o := range outputs {
		scriptLens = append(scriptLens, len(o.ScriptHex)/2)
	}
	var chosen []candidate
	var total uint64
	var fee uint64
	var change uint64
	decided := false
	for i := range pool {
		chosen = append(chosen, pool[i])
		next, err := AddChecked(total, pool[i].value, "selected total")
		if err != nil {
			return nil, err
		}
		total = next
		withChangeLens := append(append([]int(nil), scriptLens...), len(k.config.WalletScriptHex)/2)
		withChangeSize, err := EstimateSignedSize(len(chosen), withChangeLens)
		if err != nil {
			return nil, err
		}
		withChange, err := EstimateFeeSats(feeRate, withChangeSize)
		if err != nil {
			return nil, err
		}
		needWithChange, err := AddChecked(business, withChange, "need")
		if err == nil {
			needWithChange, err = AddChecked(needWithChange, k.dust, "need")
		}
		if err == nil && total >= needWithChange {
			fee = withChange
			change = total - business - withChange
			decided = true
			break
		}
		if i == len(pool)-1 {
			noChangeSize, err := EstimateSignedSize(len(chosen), scriptLens)
			if err != nil {
				return nil, err
			}
			noChange, err := EstimateFeeSats(feeRate, noChangeSize)
			if err != nil {
				return nil, err
			}
			needNoChange, err := AddChecked(business, noChange, "need")
			if err == nil && total >= needNoChange {
				fee = total - business
				change = 0
				if fee < noChange {
					return nil, fail(ErrInsufficient, "fee short of no-change minimum")
				}
				decided = true
				break
			}
		}
	}
	if !decided {
		var have uint64
		for _, c := range pool {
			have += c.value
		}
		return nil, fail(ErrInsufficient, "need "+mustFormat(business)+" plus fee, have "+mustFormat(have))
	}
	inputs := make([]string, 0, len(chosen))
	for _, c := range chosen {
		inputs = append(inputs, c.key)
	}
	record := &storedAllocation{
		RequestID: id, RequestHash: hash, Inputs: inputs, Business: business,
		FeeRate: feeRate, Fee: fee, Change: change, Outputs: outputs,
		Version: version, Locktime: locktime, ExternalProofs: map[string]storedProof{},
		ProtocolInputs: []ProtocolInputFact{}, ProtocolFeeCapSats: "", ProtocolFeePolicy: "", State: "held",
	}
	for _, c := range chosen {
		holder := id
		k.utxos[c.key].OccupiedBy = &holder
	}
	k.allocations[id] = record
	k.localRev++
	return k.view(id, record)
}

// Replace atomically swaps the occupation of a held, unsigned allocation.
func (k *FundingKernel) Replace(allocationID string, input RequestInput) (*AllocationView, error) {
	if err := k.assertFundsNotFrozen("replace an allocation"); err != nil {
		return nil, err
	}
	id, err := checkID(allocationID)
	if err != nil {
		return nil, err
	}
	current, ok := k.allocations[id]
	if !ok {
		return nil, fail(ErrAllocationUnknown, "allocation not found")
	}
	if current.State != "held" {
		return nil, fail(ErrAllocationState, "cannot replace from "+current.State)
	}
	if current.SignedTxid != nil {
		return nil, fail(ErrAllocationState, "signed allocation cannot be replaced")
	}
	for _, key := range current.Inputs {
		if row, ok := k.utxos[key]; ok && row.OccupiedBy != nil && *row.OccupiedBy == id {
			row.OccupiedBy = nil
		}
	}
	delete(k.allocations, id)
	input.RequestID = id
	view, err := k.Request(input)
	if err != nil {
		k.allocations[id] = current
		for _, key := range current.Inputs {
			if row, ok := k.utxos[key]; ok && row.SpentBy == nil {
				holder := id
				row.OccupiedBy = &holder
			}
		}
		return nil, err
	}
	return view, nil
}

// RequestProtocol creates a protocol anchor allocation: business may be
// "0" and declared outputs may be empty (pure inbound payouts such as
// refunds declare nothing). No inputs are selected up front: protocol
// inputs are validated and authorized at spend time by
// ValidateProtocolSpend.
func (k *FundingKernel) RequestProtocol(input RequestInput) (*AllocationView, error) {
	if err := k.assertFundsNotFrozen("request protocol funds"); err != nil {
		return nil, err
	}
	id, err := checkID(input.RequestID)
	if err != nil {
		return nil, err
	}
	business, err := ParseAmount(input.BusinessAmount, "business amount")
	if err != nil {
		return nil, err
	}
	feeRate, err := ParseFeeRate(input.FeeRate)
	if err != nil {
		return nil, err
	}
	version := uint32(1)
	if input.HasVersion {
		version = input.Version
	}
	locktime := uint32(0)
	if input.HasLocktime {
		locktime = input.Locktime
	}
	outputs := make([]storedOutput, 0, len(input.Outputs))
	for _, o := range input.Outputs {
		script, err := checkScriptHex(o.ScriptHex, "output script")
		if err != nil {
			return nil, err
		}
		value, err := ParseAmount(o.Value, "output value")
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, storedOutput{ScriptHex: script, Value: value})
	}
	// No business-sum gate here: declared outputs are an exact-match binding
	// list (often funded by external pool inputs); the wallet's net outflow
	// is capped at spend time by business + fee share.
	hash := stableRequestHash(input.BusinessAmount, input.FeeRate, locktime, input.Outputs, version)
	if existing, ok := k.allocations[id]; ok {
		if existing.RequestHash == hash {
			return k.view(id, existing)
		}
		return nil, fail(ErrIDConflict, "same id with different content")
	}
	record := &storedAllocation{
		RequestID: id, RequestHash: hash, Inputs: []string{}, Business: business,
		FeeRate: feeRate, Fee: 0, Change: 0, Outputs: outputs,
		Version: version, Locktime: locktime, ExternalProofs: map[string]storedProof{},
		ProtocolInputs: []ProtocolInputFact{}, ProtocolFeeCapSats: "", ProtocolFeePolicy: "", State: "held",
	}
	k.allocations[id] = record
	k.localRev++
	return k.view(id, record)
}

// Release frees an unsigned, undispatched held allocation.
func (k *FundingKernel) Release(allocationID string) error {
	if err := k.assertFundsNotFrozen("release an allocation"); err != nil {
		return err
	}
	id, err := checkID(allocationID)
	if err != nil {
		return err
	}
	current, ok := k.allocations[id]
	if !ok {
		return fail(ErrAllocationUnknown, "allocation not found")
	}
	if current.State != "held" {
		return fail(ErrAllocationState, "cannot release from "+current.State)
	}
	if current.SignedTxid != nil {
		return fail(ErrAllocationState, "signed allocation cannot be released")
	}
	if err := k.assertNoExternalResponsibility(id); err != nil {
		return err
	}
	for _, key := range current.Inputs {
		if row, ok := k.utxos[key]; ok && row.OccupiedBy != nil && *row.OccupiedBy == id {
			row.OccupiedBy = nil
		}
	}
	current.State = "aborted"
	k.localRev++
	return nil
}

// PrepareInput validates an unsigned template against an allocation.
type PrepareInput struct {
	AllocationID   string
	UnsignedTxHex  string
	ExternalProofs map[string]ExternalProof
}

// PrepareResult reports a validated template.
type PrepareResult struct {
	AllocationID string `json:"allocationId"`
	UnsignedTx   string `json:"unsignedTxHex"`
	FeeSats      string `json:"estimatedFeeSats"`
}

// checkAncestors walks the union of the input dependency DAGs. Repeated
// outpoints/transactions are deduplicated; only a node on the active DFS path
// is a cycle.
func (k *FundingKernel) checkAncestors(outpoints []string) error {
	ancestorTxids := map[string]bool{}
	visitedOutpoints := map[string]bool{}
	activeOutpoints := map[string]bool{}
	activeTxids := map[string]bool{}
	var visit func(string) error
	visit = func(current string) error {
		if activeOutpoints[current] {
			return fail(ErrAncestorConflict, "dependency cycle")
		}
		if visitedOutpoints[current] {
			return nil
		}
		activeOutpoints[current] = true
		row, ok := k.utxos[current]
		if ok && row.Isolated != nil {
			return fail(ErrAncestorConflict, "ancestor "+current+" is isolated")
		}
		if ok && row.CreatingTxid != nil && !row.Confirmed {
			parent := *row.CreatingTxid
			if activeTxids[parent] {
				return fail(ErrAncestorConflict, "dependency cycle")
			}
			ancestorTxids[parent] = true
			if len(ancestorTxids) > k.config.MaxUnconfirmedAncestors {
				return fail(ErrPolicyDenied, "unconfirmed ancestor limit exceeded")
			}
			// A parent is known only from a trusted, settled exact — not from
			// this output's own creatingTxid, and not from a pending/unknown
			// broadcast. `SpentExternal` is provenance, never a conflict flag.
			var parentEntry *OutboxEntry
			for index := range k.outbox {
				entry := &k.outbox[index]
				if entry.TxID == parent && entry.State == "done" {
					parentEntry = entry
					break
				}
			}
			if parentEntry == nil {
				return fail(ErrAncestorUnknown, "parent "+parent+" is unknown")
			}
			parentExact, parseErr := ParseTx(parentEntry.RawTxHex, parent)
			if parseErr != nil {
				return fail(ErrAncestorConflict, "parent "+parent+" exact bytes are invalid")
			}
			_, currentVout, splitErr := splitOutpoint(current)
			if splitErr != nil {
				return splitErr
			}
			if int(currentVout) >= len(parentExact.Outputs) {
				return fail(ErrAncestorConflict, "output "+current+" does not match its parent exact")
			}
			currentOutput := parentExact.Outputs[currentVout]
			if currentOutput.Value != row.Value || strings.ToLower(currentOutput.Script) != row.ScriptHex {
				return fail(ErrAncestorConflict, "output "+current+" does not match its parent exact")
			}
			// "The parent transaction is invalid" and "one of the parent's
			// outputs is unusable" are two different facts and must never be
			// merged into one flag. The dependency direction decides which is
			// which:
			//
			// * INPUT of the parent — upstream. An isolated input, or an input
			//   another spender already claimed, means the parent itself
			//   cannot be trusted: it double-spends or descends from a broken
			//   lineage. The parent and everything below it are refused.
			// * OUTPUT of the parent — downstream. A competing spend of one
			//   output says nothing about the transaction that created it:
			//   only that output (and the descendants that depend on it)
			//   goes out of service, while the parent and its sibling
			//   outputs stay valid. Otherwise one contested output would
			//   freeze unrelated funds. The output level is an explicit allow
			//   list: a reason this version does not know is NOT treated as
			//   output level, it fails closed like a transaction level reason.
			parentInputKeys := map[string]bool{}
			for _, txIn := range parentExact.Inputs {
				parentInputKeys[txIn.OutpointKey] = true
			}
			for key, row := range k.utxos {
				// A row that records the parent as its spender is one of the
				// parent's inputs, even when the exact bytes disagree.
				if !parentInputKeys[key] && (row.SpentBy == nil || *row.SpentBy != parent) {
					continue
				}
				if row.Isolated != nil {
					return fail(ErrAncestorConflict, "parent "+parent+" input "+key+" is isolated")
				}
				if row.SpentBy != nil && *row.SpentBy != parent {
					return fail(ErrAncestorConflict, "parent "+parent+" consumption disagrees at "+key)
				}
			}
			// The parent's own transaction is broken: its change was
			// quarantined because the transaction itself lost the race, or
			// because an ancestor conflicted. The parent and its descendants
			// are invalid.
			for key, row := range k.utxos {
				if row.CreatingTxid == nil || *row.CreatingTxid != parent || row.Isolated == nil {
					continue
				}
				if isTransactionLevelIsolation(*row.Isolated) {
					return fail(ErrAncestorConflict, "parent "+parent+" conflicts at "+key)
				}
				if !isOutputLevelIsolation(*row.Isolated) {
					// An unknown reason: keep it (never silently clear it) and
					// refuse the whole branch until it is understood.
					reason := string([]rune(*row.Isolated))
					if len([]rune(reason)) > 64 {
						reason = string([]rune(reason)[:64])
					}
					return fail(ErrAncestorConflict, "parent "+parent+" carries an unknown isolation reason \""+reason+"\" at "+key)
				}
			}
			activeTxids[parent] = true
			for _, input := range row.ParentInputs {
				if err := visit(input); err != nil {
					return err
				}
			}
			delete(activeTxids, parent)
		}
		delete(activeOutpoints, current)
		visitedOutpoints[current] = true
		return nil
	}
	for _, outpoint := range outpoints {
		if err := visit(outpoint); err != nil {
			return err
		}
	}
	return nil
}

func unsignedSkeleton(rawTxHex string) (string, error) {
	parsed, err := ParseTx(rawTxHex, "")
	if err != nil {
		return "", err
	}
	inputs := make([]UnsignedInput, 0, len(parsed.Inputs))
	for _, input := range parsed.Inputs {
		inputs = append(inputs, UnsignedInput{TxID: input.TxID, Vout: input.Vout, Sequence: input.Sequence, HasSeq: true})
	}
	outputs := make([]UnsignedOutput, 0, len(parsed.Outputs))
	for _, output := range parsed.Outputs {
		outputs = append(outputs, UnsignedOutput{Value: output.Value, Script: output.Script})
	}
	return BuildUnsigned(parsed.Version, parsed.Locktime, inputs, outputs)
}

// Prepare validates the unsigned template against the allocation.
func (k *FundingKernel) Prepare(input PrepareInput) (*PrepareResult, error) {
	if err := k.assertFundsNotFrozen("prepare a signing intent"); err != nil {
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
	parsed, err := ParseTx(input.UnsignedTxHex, "")
	if err != nil {
		return nil, err
	}
	inputKeys := make([]string, 0, len(parsed.Inputs))
	for _, txIn := range parsed.Inputs {
		inputKeys = append(inputKeys, txIn.OutpointKey)
	}
	if len(inputKeys) != len(record.Inputs) {
		return nil, fail(ErrSnapshotInputInvalid, "template inputs do not match allocation")
	}
	sortedHave := SortedOutpointKeys(inputKeys)
	sortedWant := SortedOutpointKeys(record.Inputs)
	for i := range sortedHave {
		if sortedHave[i] != sortedWant[i] {
			return nil, fail(ErrSnapshotInputInvalid, "template inputs do not match allocation")
		}
	}
	for _, key := range inputKeys {
		row, ok := k.utxos[key]
		if !ok || row.SpentBy != nil || row.Isolated != nil || row.Protected || row.PendingParent != nil {
			return nil, fail(ErrSnapshotInputInvalid, "input "+key+" is not spendable")
		}
		if row.OccupiedBy == nil || *row.OccupiedBy != id {
			return nil, fail(ErrAlreadyOccupied, "input "+key+" is not held by this allocation")
		}
		if row.ScriptHex != k.config.WalletScriptHex {
			return nil, fail(ErrUnknownInput, "input "+key+" is not a proven wallet input")
		}
	}
	if err := k.checkAncestors(inputKeys); err != nil {
		return nil, err
	}
	remaining := append([]storedOutput(nil), record.Outputs...)
	var changeSeen uint64
	changeCount := 0
	for _, out := range parsed.Outputs {
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
			continue
		}
		if script == k.config.WalletScriptHex {
			next, err := AddChecked(changeSeen, out.Value, "change")
			if err != nil {
				return nil, err
			}
			changeSeen = next
			changeCount++
			continue
		}
		return nil, fail(ErrPolicyDenied, "template has an unauthorized output")
	}
	if len(remaining) != 0 {
		return nil, fail(ErrPolicyDenied, "template misses authorized business outputs")
	}
	if changeCount > 1 {
		return nil, fail(ErrPolicyDenied, "at most one change output is allowed")
	}
	if changeSeen != record.Change {
		return nil, fail(ErrFeeOutOfRange, "change does not match authorized fee")
	}
	if parsed.Version != record.Version || parsed.Locktime != record.Locktime {
		return nil, fail(ErrPolicyDenied, "version/locktime mismatch")
	}
	skeleton, err := unsignedSkeleton(input.UnsignedTxHex)
	if err != nil {
		return nil, err
	}
	if record.SigningIntentHex != "" && record.SigningIntentHex != skeleton {
		return nil, fail(ErrStateConflict, "transaction differs from frozen signing intent")
	}
	if record.SigningIntentHex == "" {
		record.SigningIntentHex = skeleton
	}
	k.localRev++
	return &PrepareResult{AllocationID: id, UnsignedTx: strings.ToLower(input.UnsignedTxHex), FeeSats: mustFormat(record.Fee)}, nil
}

// CompletePrepareInput carries signed bytes for an allocation.
type CompletePrepareInput struct {
	AllocationID string
	SignedTxHex  string
}

// CompletePrepareResult reports the validated signed transaction.
type CompletePrepareResult struct {
	AllocationID string `json:"allocationId"`
	TxID         string `json:"txid"`
	FeeSats      string `json:"feeSats"`
}

// CompletePrepare validates signed bytes and enqueues the broadcast command.
func (k *FundingKernel) CompletePrepare(input CompletePrepareInput) (*CompletePrepareResult, error) {
	if err := k.assertFundsNotFrozen("complete a wallet spend"); err != nil {
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
	parsed, err := ParseTx(input.SignedTxHex, "")
	if err != nil {
		return nil, err
	}
	for _, txIn := range parsed.Inputs {
		if len(txIn.ScriptSig)/2 > P2PKHInputWeight {
			return nil, fail(ErrSignatureExceedsBound, "input "+txIn.OutpointKey+" script exceeds P2PKH bound")
		}
	}
	if _, err := k.Prepare(PrepareInput{AllocationID: id, UnsignedTxHex: input.SignedTxHex}); err != nil {
		return nil, err
	}
	var totalIn uint64
	for _, key := range record.Inputs {
		row, ok := k.utxos[key]
		if !ok {
			return nil, fail(ErrStateConflict, "allocation input vanished")
		}
		next, err := AddChecked(totalIn, row.Value, "inputs")
		if err != nil {
			return nil, err
		}
		totalIn = next
	}
	var totalOut uint64
	for _, out := range parsed.Outputs {
		next, err := AddChecked(totalOut, out.Value, "outputs")
		if err != nil {
			return nil, err
		}
		totalOut = next
	}
	if totalOut > totalIn {
		return nil, fail(ErrFeeOutOfRange, "outputs exceed inputs")
	}
	actualFee := totalIn - totalOut
	feeFloor, err := EstimateFeeSats(record.FeeRate, uint64(parsed.SizeBytes))
	if err != nil {
		return nil, err
	}
	// The authorized estimate is the ceiling here: change-exactness forces
	// actualFee == record.fee, and the no-change path legitimately absorbs
	// dust above the with-change estimate. Protocol spends (which have no
	// such anchor) additionally enforce the shape-derived ceiling.
	if actualFee < feeFloor {
		return nil, fail(ErrFeeOutOfRange, "fee below actual-size minimum")
	}
	if actualFee > record.Fee {
		return nil, fail(ErrFeeOutOfRange, "fee above authorized estimate")
	}
	txid := parsed.CanonicalTxID
	record.SignedTxid = &txid
	record.Fee = actualFee
	outboxID := id + ":broadcast"
	found := false
	for _, entry := range k.outbox {
		if entry.CommandID == outboxID {
			found = true
			if entry.TxID != txid {
				return nil, fail(ErrStateConflict, "outbox already holds another tx for this allocation")
			}
		}
	}
	if !found {
		holder := id
		k.outbox = append(k.outbox, OutboxEntry{CommandID: outboxID, AllocationID: &holder, TxID: txid, RawTxHex: strings.ToLower(input.SignedTxHex), State: "pending", Delivery: "wallet"})
	}
	k.localRev++
	return &CompletePrepareResult{AllocationID: id, TxID: txid, FeeSats: mustFormat(actualFee)}, nil
}

// BroadcastReport carries a provider result for an outbox command.
type BroadcastReport struct {
	Status   string
	TxID     string
	RawTxHex string
}

// BroadcastResult settles an outbox command.
// MarkDispatching records that an entry was handed to the network port.
// It must be persisted before the broadcast call; a crash afterwards
// recovers as unknown (query or replay the SAME exact tx), never as
// droppable. Idempotent.
func (k *FundingKernel) MarkDispatching(commandID string) error {
	for i := range k.outbox {
		if k.outbox[i].CommandID == commandID {
			if k.outbox[i].Conclusion != nil {
				return fail(ErrAllocationState, "conflicting exact cannot be redispatched")
			}
			if k.outbox[i].State == "pending" || k.outbox[i].State == "unknown" {
				k.outbox[i].State = "dispatching"
				k.localRev++
			}
			return nil
		}
	}
	return fail(ErrAllocationUnknown, "outbox command not found")
}

func (k *FundingKernel) BroadcastResult(commandID string, result BroadcastReport) error {
	var entry *OutboxEntry
	for i := range k.outbox {
		if k.outbox[i].CommandID == commandID {
			entry = &k.outbox[i]
			break
		}
	}
	if entry == nil {
		return fail(ErrAllocationUnknown, "outbox command not found")
	}
	if entry.State == "done" {
		return nil
	}
	if entry.Conclusion != nil {
		if result.Status == "accepted" || result.Status == "already-known" {
			return fail(ErrStateConflict, "accepted receipt contradicts proven input conflict")
		}
		return nil
	}
	txid, err := CheckTxid(result.TxID)
	if err != nil {
		return err
	}
	txid = strings.ToLower(txid)
	if txid != entry.TxID {
		return fail(ErrStateConflict, "broadcast txid does not match outbox")
	}
	switch result.Status {
	case "accepted":
		if err := k.commitOutbox(*entry); err != nil {
			return err
		}
		entry.State = "done"
	case "already-known":
		if result.RawTxHex == "" {
			return fail(ErrPolicyDenied, "already-known requires provider bytes")
		}
		observed, err := ParseTx(result.RawTxHex, entry.TxID)
		if err != nil {
			return err
		}
		local, err := ParseTx(entry.RawTxHex, entry.TxID)
		if err != nil {
			return err
		}
		if observed.CanonicalTxID != local.CanonicalTxID {
			return fail(ErrStateConflict, "provider tx differs from local exact")
		}
		if err := k.commitOutbox(*entry); err != nil {
			return err
		}
		entry.State = "done"
	case "not-dispatched", "unknown":
		entry.State = "unknown"
		k.markUnknown(*entry)
	default:
		return fail(ErrPolicyDenied, "bad broadcast status")
	}
	k.localRev++
	return nil
}

// mergeSelfOutput merges one self output of an exact transaction into the
// outpoint view. The same outpoint/value/script is not a conflict just because
// WoC saw it first: confirmation, protection, occupation and spent tombstones
// are preserved; only the exact parent evidence is filled in. Real
// amount/script/parent conflicts and existing quarantine/conflict reasons stay
// isolated.
func (k *FundingKernel) mergeSelfOutput(txid string, parentInputs []string, out TxOutput, index int) {
	key := txid + ":" + strconv.Itoa(index)
	script := strings.ToLower(out.Script)
	prev, exists := k.utxos[key]
	if !exists {
		holder := txid
		k.utxos[key] = &storedUtxo{
			Value: out.Value, ScriptHex: script, Status: "unconfirmed", Height: 0,
			Source: "local", CreatingTxid: &holder, ParentInputs: append([]string(nil), parentInputs...), Confirmed: false,
			ProtectionSources: []string{},
		}
		return
	}
	if prev.SpentBy != nil {
		return // never resurrect a spent output
	}
	if prev.Protected && prev.Value == 0 && prev.ScriptHex == "" {
		// Protected placeholder registered before the output existed. Adopt
		// the real facts, keep every protection, and clear only the placeholder
		// marker, never a real quarantine/conflict.
		if prev.Isolated != nil && *prev.Isolated == "protected-unknown" {
			prev.Isolated = nil
		}
		holder := txid
		prev.Value = out.Value
		prev.ScriptHex = script
		prev.Status = "unconfirmed"
		prev.Height = 0
		prev.Source = "local"
		prev.CreatingTxid = &holder
		prev.ParentInputs = append([]string(nil), parentInputs...)
		prev.Confirmed = false
		prev.SpentBy = nil
		prev.SpentExternal = false
		prev.PendingParent = nil
		return
	}
	if prev.Value == out.Value && prev.ScriptHex == script {
		if prev.CreatingTxid != nil && *prev.CreatingTxid != txid {
			if prev.Isolated == nil {
				reason := "local-conflict:" + key
				prev.Isolated = &reason
			}
			return
		}
		if prev.Isolated != nil && *prev.Isolated == "protected-unknown" {
			prev.Isolated = nil
		}
		holder := txid
		prev.CreatingTxid = &holder
		prev.ParentInputs = append([]string(nil), parentInputs...)
		prev.PendingParent = nil
		if prev.Source != "woc" {
			prev.Source = "local"
		}
		return
	}
	if prev.Isolated == nil {
		reason := "local-conflict:" + key
		prev.Isolated = &reason
	}
}

// assertOutboxInputsAvailable applies the common spender-conflict rule to the
// ordinary provider/reconcile settlement paths. On conflict it isolates the
// inputs and this transaction's already-visible change, then fails closed
// before outbox/spent/output state is changed.
func (k *FundingKernel) assertOutboxInputsAvailable(entry OutboxEntry, parsed *ParsedTx) error {
	conflicts := []string{}
	for _, txIn := range parsed.Inputs {
		key := txIn.OutpointKey
		row, exists := k.utxos[key]
		if !exists {
			continue
		}
		if row.SpentBy != nil && *row.SpentBy != entry.TxID {
			conflicts = append(conflicts, key)
			continue
		}
		if row.OccupiedBy != nil && (entry.AllocationID == nil || *row.OccupiedBy != *entry.AllocationID) {
			conflicts = append(conflicts, key)
			continue
		}
		if row.Isolated != nil {
			conflicts = append(conflicts, key)
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	for _, key := range conflicts {
		k.isolateConflict(key, entry.TxID)
	}
	for index, out := range parsed.Outputs {
		if strings.ToLower(out.Script) != k.config.WalletScriptHex {
			continue
		}
		key := entry.TxID + ":" + strconv.Itoa(index)
		row, exists := k.utxos[key]
		if !exists || row.SpentBy != nil {
			continue
		}
		if row.Isolated == nil {
			reason := "commit-conflict:" + entry.TxID
			row.Isolated = &reason
		}
		k.isolateDescendants(key, "commit-conflict:"+entry.TxID)
	}
	k.localRev++
	return fail(ErrStateConflict, "outbox input conflicts with a recorded spender; related change is isolated")
}

func (k *FundingKernel) commitOutbox(entry OutboxEntry) error {
	parsed, err := ParseTx(entry.RawTxHex, entry.TxID)
	if err != nil {
		return err
	}
	if err := k.assertOutboxInputsAvailable(entry, parsed); err != nil {
		return err
	}
	for _, txIn := range parsed.Inputs {
		key := txIn.OutpointKey
		if row, ok := k.utxos[key]; ok {
			txid := entry.TxID
			row.SpentBy = &txid
			row.SpentExternal = false
			row.OccupiedBy = nil
		} else {
			txid := entry.TxID
			k.utxos[key] = &storedUtxo{Status: "unconfirmed", Source: "local", SpentBy: &txid}
		}
	}
	parentKeys := make([]string, 0, len(parsed.Inputs))
	for _, txIn := range parsed.Inputs {
		parentKeys = append(parentKeys, txIn.OutpointKey)
	}
	for index, out := range parsed.Outputs {
		if strings.ToLower(out.Script) == k.config.WalletScriptHex {
			k.mergeSelfOutput(entry.TxID, parentKeys, out, index)
		}
	}
	if entry.AllocationID != nil {
		if record, ok := k.allocations[*entry.AllocationID]; ok {
			record.State = "committed"
			txid := entry.TxID
			record.SignedTxid = &txid
		}
	}
	return nil
}

// markUnknown keeps an unknown delivery under outbox responsibility.
func (k *FundingKernel) markUnknown(entry OutboxEntry) {
	if entry.AllocationID == nil {
		return
	}
	if record, ok := k.allocations[*entry.AllocationID]; ok && record.State == "held" {
		txid := entry.TxID
		record.SignedTxid = &txid
	}
}

// OutpointRecord is a read-only view of one outpoint for host adapters.
type OutpointRecord struct {
	Value         string   `json:"value"`
	ScriptHex     string   `json:"scriptHex"`
	Status        string   `json:"status"`
	Source        string   `json:"source"`
	CreatingTxid  *string  `json:"creatingTxid"`
	SpentBy       *string  `json:"spentBy"`
	OccupiedBy    *string  `json:"occupiedBy"`
	Protected     bool     `json:"protected"`
	Isolated      *string  `json:"isolated"`
	PendingParent *string  `json:"pendingParent"`
	ParentInputs  []string `json:"parentInputs"`
	Confirmed     bool     `json:"confirmed"`
}

func copyStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func copyNullableStrings(value []string) []string {
	if value == nil {
		return nil
	}
	return append([]string{}, value...)
}

// Lookup returns a read-only view of one outpoint for host adapters.
func (k *FundingKernel) Lookup(outpoint string) (*OutpointRecord, error) {
	key := strings.ToLower(outpoint)
	if _, _, err := splitOutpoint(key); err != nil {
		return nil, err
	}
	row, ok := k.utxos[key]
	if !ok {
		return nil, fail(ErrUnknownInput, "outpoint "+outpoint+" is unknown")
	}
	return &OutpointRecord{
		Value: mustFormat(row.Value), ScriptHex: row.ScriptHex, Status: row.Status,
		Source: row.Source, CreatingTxid: copyStringPointer(row.CreatingTxid), SpentBy: copyStringPointer(row.SpentBy),
		OccupiedBy: copyStringPointer(row.OccupiedBy), Protected: row.Protected, Isolated: copyStringPointer(row.Isolated),
		PendingParent: copyStringPointer(row.PendingParent), ParentInputs: copyNullableStrings(row.ParentInputs), Confirmed: row.Confirmed,
	}, nil
}

// AbortUnbroadcast aborts an allocation whose signed bytes were never
// broadcast: the outbox holds no non-pending entry for it, so nothing
// external can exist. It releases occupations, drops a pending outbox entry
// for the same bytes, and marks the allocation aborted. Callers must
// guarantee the bytes never left the host through another path.
func (k *FundingKernel) AbortUnbroadcast(allocationID string) error {
	if err := k.assertFundsNotFrozen("abort an allocation"); err != nil {
		return err
	}
	id, err := checkID(allocationID)
	if err != nil {
		return err
	}
	record, ok := k.allocations[id]
	if !ok {
		return fail(ErrAllocationUnknown, "allocation not found")
	}
	if record.State != "held" {
		return fail(ErrAllocationState, "cannot abort from "+record.State)
	}
	outboxID := id + ":broadcast"
	index := -1
	for i, entry := range k.outbox {
		if entry.CommandID == outboxID {
			index = i
			break
		}
	}
	if index >= 0 {
		entry := k.outbox[index]
		if entry.State != "pending" {
			return fail(ErrAllocationState, "broadcast already attempted")
		}
		if record.SignedTxid == nil || entry.TxID != *record.SignedTxid {
			return fail(ErrStateConflict, "outbox does not match allocation")
		}
	}
	// A partial signature that already left the wallet keeps the inputs
	// responsible: an ordinary abort must not erase that consequence.
	if err := k.assertNoExternalResponsibility(id); err != nil {
		return err
	}
	if index >= 0 {
		k.outbox = append(k.outbox[:index], k.outbox[index+1:]...)
	}
	for _, key := range record.Inputs {
		if row, ok := k.utxos[key]; ok && row.OccupiedBy != nil && *row.OccupiedBy == id && row.SpentBy == nil {
			row.OccupiedBy = nil
		}
	}
	record.State = "aborted"
	k.localRev++
	return nil
}

// RetireForumProtocolAllocation explicitly discards a host-proven retired forum
// allocation, including signed/unknown exacts. Never use for ordinary cancellation.
// Observed spent inputs remain spent.
func (k *FundingKernel) RetireForumProtocolAllocation(allocationID string) error {
	if err := k.assertFundsNotFrozen("retire a forum protocol allocation"); err != nil {
		return err
	}
	id, err := checkID(allocationID)
	if err != nil {
		return err
	}
	record, ok := k.allocations[id]
	if !ok {
		return fail(ErrAllocationUnknown, "allocation not found")
	}
	if record.ProtocolBinding == nil {
		return fail(ErrAllocationState, "forum retirement requires a protocol allocation")
	}
	for _, key := range record.Inputs {
		if row, ok := k.utxos[key]; ok && row.OccupiedBy != nil && *row.OccupiedBy == id && row.SpentBy == nil {
			row.OccupiedBy = nil
		}
	}
	kept := k.outbox[:0]
	for _, entry := range k.outbox {
		if entry.AllocationID == nil || *entry.AllocationID != id {
			kept = append(kept, entry)
		}
	}
	k.outbox = kept
	for key, signature := range k.externalSignatures {
		if signature.AllocationID == id {
			delete(k.externalSignatures, key)
		}
	}
	delete(k.allocations, id)
	k.localRev++
	return nil
}

// CancelUnstarted releases a held allocation only after a trusted execution
// journal proved that no signature was ever started. It refuses signed or
// attempted allocations.
func (k *FundingKernel) CancelUnstarted(allocationID string) error {
	if err := k.assertFundsNotFrozen("cancel an unstarted allocation"); err != nil {
		return err
	}
	id, err := checkID(allocationID)
	if err != nil {
		return err
	}
	record, ok := k.allocations[id]
	if !ok {
		return fail(ErrAllocationUnknown, "allocation not found")
	}
	if record.State != "held" {
		return fail(ErrAllocationState, "cannot cancel from "+record.State)
	}
	if record.SignedTxid != nil {
		return fail(ErrAllocationState, "allocation already signed")
	}
	record.SigningIntentDurable = false
	record.SigningIntentHex = ""
	return k.AbortUnbroadcast(id)
}

// DropPending implements the startup policy: drop every never-broadcast
// pending entry and abort unsigned/pending-signed held allocations.
// Dispatching or unknown entries keep their responsibility. Deterministic
// order (sorted ids).
func (k *FundingKernel) DropPending() []string {
	if k.fundsFrozen != nil {
		return []string{}
	}
	ids := make([]string, 0, len(k.allocations))
	for id := range k.allocations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	dropped := []string{}
	for _, id := range ids {
		if record := k.allocations[id]; record != nil && (record.SigningIntentDurable || record.SignedTxid != nil) {
			continue
		}
		if err := k.AbortUnbroadcast(id); err == nil {
			dropped = append(dropped, id)
		}
	}
	return dropped
}

// ImportUnknown records a previously-broadcast exact transaction whose
// result is unknown (legacy unresolved records, protocol refund/close
// broadcasts). Inputs come under this entry's responsibility; self outputs
// are adopted but NOT selectable until the entry settles via
// BroadcastResult/Reconcile.
func (k *FundingKernel) ImportUnknown(txid, rawTxHex string) (string, error) {
	canonical, err := CheckTxid(txid)
	if err != nil {
		return "", err
	}
	canonical = strings.ToLower(canonical)
	parsed, err := ParseTx(rawTxHex, canonical)
	if err != nil {
		return "", err
	}
	commandID := "imported:" + canonical
	for _, entry := range k.outbox {
		if entry.CommandID == commandID {
			if entry.TxID != canonical || !strings.EqualFold(entry.RawTxHex, rawTxHex) {
				return "", fail(ErrStateConflict, "imported command already holds another tx")
			}
			return commandID, nil
		}
	}
	for _, txIn := range parsed.Inputs {
		key := txIn.OutpointKey
		if row, ok := k.utxos[key]; ok {
			if row.SpentBy != nil && *row.SpentBy != canonical {
				return "", fail(ErrStateConflict, "input "+key+" already spent by another tx")
			}
			tomb := canonical
			row.SpentBy = &tomb
			row.SpentExternal = false
			row.OccupiedBy = nil
		} else {
			tomb := canonical
			k.utxos[key] = &storedUtxo{Status: "unconfirmed", Source: "local", SpentBy: &tomb}
		}
	}
	for index, out := range parsed.Outputs {
		if strings.ToLower(out.Script) != k.config.WalletScriptHex {
			continue
		}
		key := canonical + ":" + strconv.Itoa(index)
		if prev, ok := k.utxos[key]; ok {
			if prev.SpentBy != nil {
				continue
			}
			if prev.Source == "local" && prev.Value == out.Value && prev.CreatingTxid != nil && *prev.CreatingTxid == canonical {
				continue
			}
			reason := "local-conflict:" + key
			prev.Isolated = &reason
			continue
		}
		parentInputs := make([]string, 0, len(parsed.Inputs))
		for _, txIn := range parsed.Inputs {
			parentInputs = append(parentInputs, txIn.OutpointKey)
		}
		parent := canonical
		creating := canonical
		k.utxos[key] = &storedUtxo{Value: out.Value, ScriptHex: strings.ToLower(out.Script), Status: "unconfirmed",
			Source: "local", CreatingTxid: &creating, ParentInputs: parentInputs, Confirmed: false, PendingParent: &parent}
	}
	k.outbox = append(k.outbox, OutboxEntry{CommandID: commandID, AllocationID: nil, TxID: canonical, RawTxHex: strings.ToLower(rawTxHex), State: "unknown", Delivery: "wallet"})
	k.localRev++
	return commandID, nil
}

// Reconcile settles an unknown outbox entry against a chain observation.
func (k *FundingKernel) Reconcile(commandID string, found bool, rawTxHex string) (string, error) {
	var entry *OutboxEntry
	for i := range k.outbox {
		if k.outbox[i].CommandID == commandID {
			entry = &k.outbox[i]
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
		return "", fail(ErrAllocationState, "conflicting exact requires input reconciliation")
	}
	if !found {
		return "still-unknown", nil
	}
	if rawTxHex == "" {
		return "", fail(ErrPolicyDenied, "found observation requires bytes")
	}
	observed, err := ParseTx(rawTxHex, entry.TxID)
	if err != nil {
		return "", err
	}
	if observed.CanonicalTxID != entry.TxID {
		return "", fail(ErrStateConflict, "observed tx differs from local exact")
	}
	if err := k.commitOutbox(*entry); err != nil {
		return "", err
	}
	entry.State = "done"
	k.localRev++
	return "committed", nil
}

// ValidateProtocolSpend validates protocol input responsibility and all
// business/fee authorizations before mutating allocation state.
func (k *FundingKernel) ValidateProtocolSpend(allocationID, signedTxHex string, externalProofs map[string]ExternalProof, binding *ProtocolBinding) (string, string, error) {
	if err := k.assertFundsNotFrozen("validate a protocol spend"); err != nil {
		return "", "", err
	}
	id, err := checkID(allocationID)
	if err != nil {
		return "", "", err
	}
	record, ok := k.allocations[id]
	if !ok {
		return "", "", fail(ErrAllocationUnknown, "allocation not found")
	}
	if record.State != "held" {
		return "", "", fail(ErrAllocationState, "cannot spend from "+record.State)
	}
	if binding == nil || (binding.Kind != "refund" && binding.Kind != "close") || binding.PoolID == "" {
		return "", "", fail(ErrPolicyDenied, "valid protocol binding is required")
	}
	parsed, err := ParseTx(signedTxHex, "")
	if err != nil {
		return "", "", err
	}
	if record.SignedTxid != nil && *record.SignedTxid != parsed.CanonicalTxID {
		return "", "", fail(ErrStateConflict, "protocol allocation already binds another transaction")
	}

	walletKeys := []string{}
	inputKeys := make([]string, 0, len(parsed.Inputs))
	walletValues := []uint64{}
	externalValues := []uint64{}
	normalizedProofs := map[string]storedProof{}
	for _, txIn := range parsed.Inputs {
		key := txIn.OutpointKey
		inputKeys = append(inputKeys, key)
		row, rowExists := k.utxos[key]
		proof, hasProof := externalProofs[key]
		var normalized storedProof
		if hasProof {
			value, err := ParseAmount(proof.Value, "proof value")
			if err != nil {
				return "", "", err
			}
			scriptHex, err := checkScriptHex(proof.ScriptHex, "proof script")
			if err != nil {
				return "", "", err
			}
			normalized = storedProof{Value: value, ScriptHex: scriptHex}
		}
		if rowExists && (row.SpentBy != nil || row.Isolated != nil || row.Protected || row.PendingParent != nil) {
			return "", "", fail(ErrSnapshotInputInvalid, "input "+key+" is spent, protected, isolated or has an unknown parent")
		}
		if rowExists && row.OccupiedBy != nil && *row.OccupiedBy != id {
			return "", "", fail(ErrAlreadyOccupied, "input "+key+" is occupied")
		}
		if rowExists && row.ScriptHex == k.config.WalletScriptHex {
			if hasProof {
				return "", "", fail(ErrPolicyDenied, "proof for wallet input "+key+" is not allowed")
			}
			walletKeys = append(walletKeys, key)
			walletValues = append(walletValues, row.Value)
			continue
		}
		if !hasProof {
			return "", "", fail(ErrUnknownInput, "input "+key+" has no trusted proof")
		}
		if normalized.ScriptHex == k.config.WalletScriptHex {
			return "", "", fail(ErrPolicyDenied, "input "+key+" claims the wallet script without kernel proof")
		}
		if rowExists && (row.ScriptHex != normalized.ScriptHex || row.Value != normalized.Value) {
			return "", "", fail(ErrStateConflict, "proof does not match the recorded prevout "+key)
		}
		normalizedProofs[key] = normalized
		externalValues = append(externalValues, normalized.Value)
	}
	if err := k.checkAncestors(walletKeys); err != nil {
		return "", "", err
	}
	var walletIn, externalIn uint64
	for _, value := range walletValues {
		walletIn, err = AddChecked(walletIn, value, "wallet inputs")
		if err != nil {
			return "", "", err
		}
	}
	for _, value := range externalValues {
		externalIn, err = AddChecked(externalIn, value, "external inputs")
		if err != nil {
			return "", "", err
		}
	}
	var totalOut uint64
	var ownOut uint64
	remaining := append([]storedOutput(nil), record.Outputs...)
	for _, out := range parsed.Outputs {
		totalOut, err = AddChecked(totalOut, out.Value, "outputs")
		if err != nil {
			return "", "", err
		}
		outScript := strings.ToLower(out.Script)
		if outScript == k.config.WalletScriptHex {
			ownOut, err = AddChecked(ownOut, out.Value, "own outputs")
			if err != nil {
				return "", "", err
			}
			continue
		}
		found := -1
		for i, declared := range remaining {
			if declared.ScriptHex == outScript && declared.Value == out.Value {
				found = i
				break
			}
		}
		if found < 0 {
			return "", "", fail(ErrPolicyDenied, "protocol tx has an undeclared non-self output")
		}
		remaining = append(remaining[:found], remaining[found+1:]...)
	}
	if len(remaining) != 0 {
		return "", "", fail(ErrPolicyDenied, "protocol tx misses declared outputs")
	}
	totalIn, err := AddChecked(walletIn, externalIn, "inputs")
	if err != nil {
		return "", "", err
	}
	if totalIn < totalOut {
		return "", "", fail(ErrFeeOutOfRange, "protocol tx outputs exceed inputs")
	}
	feeTotal := totalIn - totalOut
	feeFloor, err := EstimateFeeSats(record.FeeRate, uint64(parsed.SizeBytes))
	if err != nil {
		return "", "", err
	}
	scriptLens := make([]int, 0, len(parsed.Outputs))
	for _, out := range parsed.Outputs {
		scriptLens = append(scriptLens, len(out.Script)/2)
	}
	estimate, err := EstimateSignedSize(len(parsed.Inputs), scriptLens)
	if err != nil {
		return "", "", err
	}
	protocolSizeCeiling := uint64(parsed.SizeBytes) + uint64(len(parsed.Inputs))*6
	if protocolSizeCeiling > estimate {
		estimate = protocolSizeCeiling
	}
	feeCeiling, err := EstimateFeeSats(record.FeeRate, estimate)
	if err != nil {
		return "", "", err
	}
	if feeTotal < feeFloor || feeTotal > feeCeiling {
		return "", "", fail(ErrFeeOutOfRange, fmt.Sprintf("protocol fee %d outside requested rate interval [%d,%d]", feeTotal, feeFloor, feeCeiling))
	}
	var walletLoss uint64
	if walletIn > ownOut {
		walletLoss = walletIn - ownOut
	}
	walletCap, err := AddChecked(record.Business, feeCeiling, "wallet cap")
	if err != nil || walletLoss > walletCap {
		return "", "", fail(ErrPolicyDenied, "wallet pays more than business plus fee share")
	}

	outboxID := id + ":broadcast"
	exactHex := strings.ToLower(signedTxHex)
	var existingOutbox *OutboxEntry
	for i := range k.outbox {
		if k.outbox[i].CommandID == outboxID {
			existingOutbox = &k.outbox[i]
			break
		}
	}
	if existingOutbox != nil && (existingOutbox.TxID != parsed.CanonicalTxID || existingOutbox.RawTxHex != exactHex) {
		return "", "", fail(ErrStateConflict, "protocol outbox already holds different exact bytes")
	}

	// Bind every actual input, including proved external inputs, into the same
	// durable responsibility set used by abort, commit and restore.
	for _, key := range inputKeys {
		row, exists := k.utxos[key]
		if !exists {
			proof := normalizedProofs[key]
			holder := id
			k.utxos[key] = &storedUtxo{Value: proof.Value, ScriptHex: proof.ScriptHex, Status: "unconfirmed", Source: "external", OccupiedBy: &holder}
		} else {
			holder := id
			row.OccupiedBy = &holder
		}
	}
	spent := map[string]bool{}
	for _, key := range inputKeys {
		spent[key] = true
	}
	for _, key := range record.Inputs {
		if !spent[key] {
			if row, ok := k.utxos[key]; ok && row.OccupiedBy != nil && *row.OccupiedBy == id {
				row.OccupiedBy = nil
			}
		}
	}
	record.Inputs = append([]string(nil), inputKeys...)
	record.ExternalProofs = normalizedProofs
	txid := parsed.CanonicalTxID
	record.SignedTxid = &txid
	record.ProtocolBinding = &ProtocolBinding{Kind: binding.Kind, PoolID: binding.PoolID}
	if existingOutbox == nil {
		holder := id
		k.outbox = append(k.outbox, OutboxEntry{CommandID: outboxID, AllocationID: &holder, TxID: txid, RawTxHex: exactHex, State: "pending", Delivery: "wallet"})
	}
	k.localRev++
	return txid, mustFormat(feeTotal), nil
}

func (k *FundingKernel) view(id string, record *storedAllocation) (*AllocationView, error) {
	var total uint64
	for _, key := range record.Inputs {
		if row, ok := k.utxos[key]; ok {
			next, err := AddChecked(total, row.Value, "allocation total")
			if err != nil {
				return nil, err
			}
			total = next
		}
	}
	inputs := append([]string{}, record.Inputs...)
	return &AllocationView{
		AllocationID: id, RequestID: record.RequestID, Inputs: inputs,
		TotalInSats: mustFormat(total), BusinessSats: mustFormat(record.Business),
		FeeSats: mustFormat(record.Fee), ChangeSats: mustFormat(record.Change), State: record.State,
	}, nil
}

// AllocationView returns the public allocation state.
func (k *FundingKernel) AllocationView(allocationID string) (*AllocationView, error) {
	id, err := checkID(allocationID)
	if err != nil {
		return nil, err
	}
	record, ok := k.allocations[id]
	if !ok {
		return nil, fail(ErrAllocationUnknown, "allocation not found")
	}
	return k.view(id, record)
}

// OutboxView returns a copy of the outbox.
func (k *FundingKernel) OutboxView() []OutboxEntry {
	out := make([]OutboxEntry, len(k.outbox))
	copy(out, k.outbox)
	for i := range out {
		if out[i].AllocationID != nil {
			id := *out[i].AllocationID
			out[i].AllocationID = &id
		}
		out[i].Conclusion = cloneInputConclusion(out[i].Conclusion)
	}
	return out
}

// SpendableBalance returns the currently selectable total.
func (k *FundingKernel) SpendableBalance() string {
	var total uint64
	for _, c := range k.candidates() {
		total += c.value
	}
	return mustFormat(total)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
