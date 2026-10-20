// This file owns the per-pool persisted state that survives restarts.
//
// Layout under the task store (client/<seed-hash>/):
//
//	pools/<poolId>/meta.json           immutable pool metadata + opening proof
//	pools/<poolId>/payments.jsonl      append-only Kind 5/6/7 payment journal
//	pools/<poolId>/kind5-<authId>.bin  exact content request, saved before send
//	pools/<poolId>/kind6-<authId>.bin  exact content delivery
//	pools/<poolId>/kind7-<authId>.bin  exact buyer payment update
//	pools/<poolId>/close-binding.json  last local payment state bound to close
//	pools/<poolId>/kind12-close-request.bin
//	pools/<poolId>/kind13-close-response.bin
//	pools/<poolId>/close-transaction.bin
//
// The journal is the only source of truth for round progression: on load every
// pair is re-verified through the SDK and must form a continuous sequence with
// non-decreasing cumulative seller amounts. Nothing derived is trusted.
package bitfs

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bsv8/go-bitfs/buyer"
	"github.com/bsv8/go-bitfs/content"
	"github.com/bsv8/go-bitfs/pool"
	"github.com/bsv8/go-bitfs/wire"

	"github.com/bsv8/keymaster.cc-cli/internal/store"
)

const (
	poolMetaName         = "meta.json"
	poolJournalName      = "payments.jsonl"
	poolCloseBindingName = "close-binding.json"
	poolKind12Name       = "kind12-close-request.bin"
	poolKind13Name       = "kind13-close-response.bin"
	poolCloseTxName      = "close-transaction.bin"
)

var poolIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// PoolMeta is the immutable per-pool metadata needed to rebuild the buyer
// state after a restart: the exact quote, the completed opening proof and the
// identifiers the ledger was opened with.
type PoolMeta struct {
	PoolID         string `json:"poolId"`
	SellerID       string `json:"sellerId"`
	SessionID      string `json:"sessionId"`
	AmountSatoshis uint64 `json:"amountSatoshis"`
	SeedHashHex    string `json:"seedHashHex,omitempty"`
	QuoteRawHex    string `json:"quoteRawHex"`
	OpeningCBORHex string `json:"openingCborHex"`
	ExpiryLockTime uint32 `json:"expiryLockTime"`
}

// PaymentJournalEntry records one completed content round. The exact Kind 5
// and Kind 7 bytes are stored under the recorded paths and re-verified on
// load; the recorded derived values are cross-checked, never trusted.
type PaymentJournalEntry struct {
	AuthorizationID      string `json:"authorizationId"`
	PaymentSequence      uint32 `json:"paymentSequence"`
	SellerAmountSatoshis uint64 `json:"sellerAmountSatoshis"`
	Kind5Path            string `json:"kind5"`
	Kind6Path            string `json:"kind6"`
	Kind7Path            string `json:"kind7"`
	RecordedAt           string `json:"recordedAt"`
}

// PoolState is the current buyer view of one pool: the evidence package for
// the next protocol step plus the derived cumulative values.
type PoolState struct {
	Meta                 *PoolMeta
	Opening              *pool.OpeningProof
	Evidence             buyer.BuyerPoolEvidence
	PaymentSequence      uint32
	SellerAmountSatoshis uint64
	AuthorizationID      string
	Journal              []PaymentJournalEntry
}

// validatePoolID accepts the scheduler's hierarchical ids ("task/pool/1") but
// never a path that could escape the pool directory or alias another pool.
func validatePoolID(poolID string) error {
	if len(poolID) == 0 || len(poolID) > 200 || !poolIDPattern.MatchString(poolID) {
		return fmt.Errorf("bitfs: invalid pool id %q", poolID)
	}
	for _, segment := range strings.Split(poolID, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("bitfs: invalid pool id %q", poolID)
		}
	}
	return nil
}

func poolPath(poolID, name string) string {
	return "pools/" + poolID + "/" + name
}

func kindName(prefix, authorizationID string) string {
	return prefix + "-" + authorizationID + ".bin"
}

// savePoolMeta writes the pool metadata once. A later write must be identical;
// the file is replaced atomically because it is derived state, but semantic
// differences are rejected instead of silently overwriting.
func savePoolMeta(s *store.Store, meta PoolMeta) error {
	if err := validatePoolID(meta.PoolID); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("bitfs: encode pool meta: %w", err)
	}
	encoded = append(encoded, '\n')
	name := poolPath(meta.PoolID, poolMetaName)
	if existing, err := s.Load(name); err == nil {
		var current PoolMeta
		if err := json.Unmarshal(existing, &current); err != nil {
			return fmt.Errorf("bitfs: existing pool meta is corrupt: %w", err)
		}
		if current != meta {
			return fmt.Errorf("bitfs: pool %s already has different metadata", meta.PoolID)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.Save(name, encoded)
}

// LoadPoolMeta reads and fully validates one pool's metadata.
func LoadPoolMeta(s *store.Store, poolID string) (*PoolMeta, error) {
	if s == nil {
		return nil, errors.New("bitfs: store is required")
	}
	if err := validatePoolID(poolID); err != nil {
		return nil, err
	}
	raw, err := s.Load(poolPath(poolID, poolMetaName))
	if err != nil {
		return nil, fmt.Errorf("bitfs: load pool meta: %w", err)
	}
	var meta PoolMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("bitfs: decode pool meta: %w", err)
	}
	if meta.PoolID != poolID {
		return nil, fmt.Errorf("bitfs: pool meta belongs to %s", meta.PoolID)
	}
	if _, err := meta.OpeningProof(); err != nil {
		return nil, err
	}
	return &meta, nil
}

// QuoteRaw returns the exact Kind 1 quote bytes.
func (m *PoolMeta) QuoteRaw() ([]byte, error) {
	raw, err := hex.DecodeString(m.QuoteRawHex)
	if err != nil || len(raw) == 0 {
		return nil, errors.New("bitfs: pool meta quote bytes are invalid")
	}
	return raw, nil
}

// OpeningProof decodes and validates the persisted opening proof.
func (m *PoolMeta) OpeningProof() (*pool.OpeningProof, error) {
	raw, err := hex.DecodeString(m.OpeningCBORHex)
	if err != nil || len(raw) == 0 {
		return nil, errors.New("bitfs: pool meta opening bytes are invalid")
	}
	opening, err := pool.DecodeOpeningProof(raw)
	if err != nil {
		return nil, fmt.Errorf("bitfs: decode pool opening: %w", err)
	}
	if err := pool.ValidateOpeningProof(opening); err != nil {
		return nil, fmt.Errorf("bitfs: validate pool opening: %w", err)
	}
	return opening, nil
}

func loadJournal(s *store.Store, poolID string) ([]PaymentJournalEntry, error) {
	raw, err := s.Load(poolPath(poolID, poolJournalName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	entries := make([]PaymentJournalEntry, 0)
	for lineNumber, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry PaymentJournalEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("bitfs: pool %s journal line %d is corrupt: %w", poolID, lineNumber+1, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func appendJournal(s *store.Store, poolID string, entry PaymentJournalEntry) error {
	name := poolPath(poolID, poolJournalName)
	path := filepath.Join(s.Root(), filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("bitfs: create pool directory: %w", err)
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("bitfs: encode journal entry: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("bitfs: open pool journal: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("bitfs: append pool journal: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("bitfs: sync pool journal: %w", err)
	}
	return nil
}

func decodeKind5(raw []byte) (*content.SignedContentRequest, *content.PaymentAuthorization, error) {
	artifact, err := wire.ParseAs(wire.ContentRequest, raw)
	if err != nil {
		return nil, nil, err
	}
	request, err := wire.DecodeContentRequest(artifact)
	if err != nil {
		return nil, nil, err
	}
	authorization, err := content.DecodePaymentAuthorization(request.PaymentAuthorizationCBOR)
	if err != nil {
		return nil, nil, err
	}
	return request, authorization, nil
}

// AuthorizationIDOfKind5 recomputes the authorization id from exact Kind 5
// bytes; persisted names and journal entries are only accepted after this
// matches.
func AuthorizationIDOfKind5(raw []byte) (string, error) {
	artifact, err := wire.ParseAs(wire.ContentRequest, raw)
	if err != nil {
		return "", err
	}
	request, err := wire.DecodeContentRequest(artifact)
	if err != nil {
		return "", err
	}
	id, err := content.PaymentAuthorizationID(request.PaymentAuthorizationCBOR)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// LoadPoolState rebuilds the newest provable buyer state from the store: the
// opening proof plus the continuous Kind 5/7 journal. A gap, a replayed
// sequence, a regressed amount or a byte-level mismatch between recorded paths
// and derived values is an error, never a guess.
func LoadPoolState(s *store.Store, poolID string) (*PoolState, error) {
	meta, err := LoadPoolMeta(s, poolID)
	if err != nil {
		return nil, err
	}
	opening, err := meta.OpeningProof()
	if err != nil {
		return nil, err
	}
	state := &PoolState{
		Meta:    meta,
		Opening: opening,
		Evidence: buyer.BuyerPoolEvidence{
			Opening: opening,
		},
	}
	initialSequence, initialAmount, err := buyer.InspectPool(state.Evidence)
	if err != nil {
		return nil, fmt.Errorf("bitfs: inspect initial pool state: %w", err)
	}
	state.PaymentSequence = uint32(initialSequence)
	state.SellerAmountSatoshis = uint64(initialAmount)

	entries, err := loadJournal(s, poolID)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].PaymentSequence != entries[j].PaymentSequence {
			return entries[i].PaymentSequence < entries[j].PaymentSequence
		}
		return entries[i].AuthorizationID < entries[j].AuthorizationID
	})
	previousAmount := uint64(initialAmount)
	for index, entry := range entries {
		if entry.Kind5Path == "" || entry.Kind7Path == "" {
			return nil, fmt.Errorf("bitfs: pool %s journal entry %d has no evidence paths", poolID, index+1)
		}
		rawKind5, err := s.Load(entry.Kind5Path)
		if err != nil {
			return nil, fmt.Errorf("bitfs: load journal kind5: %w", err)
		}
		rawKind7, err := s.Load(entry.Kind7Path)
		if err != nil {
			return nil, fmt.Errorf("bitfs: load journal kind7: %w", err)
		}
		derivedID, err := AuthorizationIDOfKind5(rawKind5)
		if err != nil {
			return nil, fmt.Errorf("bitfs: pool %s journal entry %d has invalid kind5: %w", poolID, index+1, err)
		}
		if derivedID != entry.AuthorizationID {
			return nil, fmt.Errorf("bitfs: pool %s journal entry %d authorization id mismatch", poolID, index+1)
		}
		evidence := buyer.BuyerPoolEvidence{
			Opening: opening,
			LatestBuyerPayment: &buyer.BuyerPaymentEvidence{
				RawKind5: rawKind5,
				RawKind7: rawKind7,
			},
		}
		derivedSequence, derivedAmount, err := buyer.InspectPool(evidence)
		if err != nil {
			return nil, fmt.Errorf("bitfs: pool %s journal entry %d does not verify: %w", poolID, index+1, err)
		}
		if uint32(derivedSequence) != entry.PaymentSequence || uint64(derivedAmount) != entry.SellerAmountSatoshis {
			return nil, fmt.Errorf("bitfs: pool %s journal entry %d recorded state does not match its evidence", poolID, index+1)
		}
		if uint32(derivedSequence) != uint32(initialSequence)+uint32(index)+1 {
			return nil, fmt.Errorf("bitfs: pool %s payment journal is not a continuous sequence", poolID)
		}
		if uint64(derivedAmount) < previousAmount {
			return nil, fmt.Errorf("bitfs: pool %s payment journal regresses the seller amount", poolID)
		}
		previousAmount = uint64(derivedAmount)
		state.PaymentSequence = uint32(derivedSequence)
		state.SellerAmountSatoshis = uint64(derivedAmount)
		state.AuthorizationID = entry.AuthorizationID
		state.Evidence = evidence
	}
	state.Journal = entries
	return state, nil
}

// PendingContentRequest returns the exact Kind 5 saved for a pool but not yet
// recorded as delivered. At most one request may be pending: it is replayed on
// retry instead of being re-signed, so a network timeout can never produce a
// second authorization for the same batch.
func PendingContentRequest(s *store.Store, poolID string) ([]byte, error) {
	if s == nil {
		return nil, errors.New("bitfs: store is required")
	}
	if err := validatePoolID(poolID); err != nil {
		return nil, err
	}
	entries, err := loadJournal(s, poolID)
	if err != nil {
		return nil, err
	}
	delivered := make(map[string]bool, len(entries))
	for _, entry := range entries {
		delivered[entry.AuthorizationID] = true
	}
	names, err := s.List("pools/" + poolID + "/")
	if err != nil {
		return nil, err
	}
	abandoned := make(map[string]bool)
	for _, name := range names {
		base := filepath.Base(filepath.FromSlash(name))
		if strings.HasPrefix(base, "abandoned-") {
			abandoned[strings.TrimPrefix(base, "abandoned-")] = true
		}
	}
	var pending []byte
	for _, name := range names {
		base := filepath.Base(filepath.FromSlash(name))
		if !strings.HasPrefix(base, "kind5-") || !strings.HasSuffix(base, ".bin") {
			continue
		}
		authorizationID := strings.TrimSuffix(strings.TrimPrefix(base, "kind5-"), ".bin")
		if delivered[authorizationID] || abandoned[authorizationID] {
			continue
		}
		if pending != nil {
			return nil, fmt.Errorf("bitfs: pool %s has more than one pending request", poolID)
		}
		raw, err := s.Load(name)
		if err != nil {
			return nil, err
		}
		pending = raw
	}
	return pending, nil
}

// abandonPending marks an expired, never-journaled authorization so it is not
// replayed again. The exact Kind 5 evidence stays on disk for audit.
func abandonPending(s *store.Store, poolID, authorizationID string) error {
	return s.Save(poolPath(poolID, "abandoned-"+authorizationID), []byte("expired\n"))
}

func recordedAt() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// contentHashesOfKind5 decodes the ordered content hashes of exact Kind 5.
func contentHashesOfKind5(kind5 []byte) ([][]byte, error) {
	_, authorization, err := decodeKind5(kind5)
	if err != nil {
		return nil, err
	}
	return content.DecodeContentHashes(authorization.ContentHashesCBOR)
}
