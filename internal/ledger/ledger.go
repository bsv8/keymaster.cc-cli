// Package ledger persists the client fund ledger and transaction outbox for
// one download task.
//
// Transaction bytes are bound to their txid: the same txid can never be
// associated with different raw bytes, and a prepared transaction is only ever
// broadcast as the saved bytes. Pool amounts and committed payments are kept
// per seller pool so a crash cannot silently lose a funding obligation.
package ledger

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

const (
	// Version is the ledger schema version.
	Version = 1

	// Transaction states.
	TxPrepared      = "prepared"
	TxResultUnknown = "result-unknown"
	TxObserved      = "observed"
	TxFailed        = "failed"

	// Pool states.
	PoolFundingPending  = "funding-pending"
	PoolFundingFailed   = "funding-failed"
	PoolOpen            = "open"
	PoolRecoveryPending = "recovery-pending"
	PoolClosed          = "closed"

	// Transaction kinds.
	KindFunding     = "funding"
	KindClose       = "close"
	KindRefund      = "refund"
	KindArbitration = "arbitration"
)

var txidPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Transaction is one outbox entry with its exact raw bytes.
type Transaction struct {
	TxID      string `json:"txid"`
	Kind      string `json:"kind"`
	RawHex    string `json:"rawHex"`
	State     string `json:"state"`
	Attempts  int    `json:"attempts"`
	UpdatedAt string `json:"updatedAt"`
	LastError string `json:"lastError,omitempty"`
}

// Pool tracks one seller fee pool.
type Pool struct {
	ID           string `json:"id"`
	SellerID     string `json:"sellerId"`
	SessionID    string `json:"sessionId"`
	Amount       uint64 `json:"amount"`
	Committed    uint64 `json:"committed"`
	State        string `json:"state"`
	FundingTxID  string `json:"fundingTxid,omitempty"`
	RecoveryTxID string `json:"recoveryTxid,omitempty"`
	RefundTxID   string `json:"refundTxid,omitempty"`
	UpdatedAt    string `json:"updatedAt"`
}

type state struct {
	Version      int                     `json:"version"`
	TaskID       string                  `json:"taskId"`
	Network      string                  `json:"network"`
	SeedHash     string                  `json:"seedHash"`
	Pools        map[string]*Pool        `json:"pools"`
	Transactions map[string]*Transaction `json:"transactions"`
}

// Ledger is one task's persisted fund ledger.
type Ledger struct {
	path  string
	state state
}

// Open loads the ledger at path or creates a new one bound to the given task.
func Open(path, taskID, network, seedHash string) (*Ledger, error) {
	if path == "" || taskID == "" || seedHash == "" || (network != "main" && network != "test") {
		return nil, errors.New("ledger: path, taskId, network and seedHash are required")
	}
	ledger := &Ledger{path: path}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, &ledger.state); err != nil {
			return nil, fmt.Errorf("ledger: invalid ledger file: %w", err)
		}
		if ledger.state.Version != Version {
			return nil, fmt.Errorf("ledger: unsupported version %d", ledger.state.Version)
		}
		if ledger.state.TaskID != taskID || ledger.state.Network != network || ledger.state.SeedHash != seedHash {
			return nil, errors.New("ledger: ledger belongs to another task")
		}
		if ledger.state.Pools == nil {
			ledger.state.Pools = map[string]*Pool{}
		}
		if ledger.state.Transactions == nil {
			ledger.state.Transactions = map[string]*Transaction{}
		}
		return ledger, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("ledger: read ledger: %w", err)
	}
	ledger.state = state{
		Version:      Version,
		TaskID:       taskID,
		Network:      network,
		SeedHash:     seedHash,
		Pools:        map[string]*Pool{},
		Transactions: map[string]*Transaction{},
	}
	if err := ledger.Save(); err != nil {
		return nil, err
	}
	return ledger, nil
}

// Save writes the ledger atomically with 0600 permissions.
func (l *Ledger) Save() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return fmt.Errorf("ledger: create directory: %w", err)
	}
	encoded, err := json.MarshalIndent(l.state, "", "  ")
	if err != nil {
		return fmt.Errorf("ledger: encode ledger: %w", err)
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(l.path), ".ledger-*.tmp")
	if err != nil {
		return fmt.Errorf("ledger: create temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return fmt.Errorf("ledger: write ledger: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, l.path); err != nil {
		return fmt.Errorf("ledger: replace ledger: %w", err)
	}
	return nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Prepare records exact transaction bytes under their txid. Repeated prepares
// with identical bytes are idempotent; different bytes for the same txid are
// rejected.
func (l *Ledger) Prepare(kind, txid, rawHex string) (*Transaction, error) {
	if !txidPattern.MatchString(txid) {
		return nil, fmt.Errorf("ledger: invalid txid %q", txid)
	}
	if !isKind(kind) {
		return nil, fmt.Errorf("ledger: invalid transaction kind %q", kind)
	}
	if rawHex == "" {
		return nil, errors.New("ledger: raw transaction is required")
	}
	if _, err := hex.DecodeString(rawHex); err != nil {
		return nil, fmt.Errorf("ledger: raw transaction is not hex: %w", err)
	}
	if existing, ok := l.state.Transactions[txid]; ok {
		if existing.RawHex != rawHex || existing.Kind != kind {
			return nil, fmt.Errorf("ledger: txid %s is already bound to different bytes", txid)
		}
		return existing, nil
	}
	transaction := &Transaction{TxID: txid, Kind: kind, RawHex: rawHex, State: TxPrepared, UpdatedAt: now()}
	l.state.Transactions[txid] = transaction
	if err := l.Save(); err != nil {
		return nil, err
	}
	return transaction, nil
}

// MarkTransition updates one transaction state. Allowed transitions:
//
//	prepared       -> result-unknown | observed | failed
//	result-unknown -> result-unknown (attempts++) | observed | failed
func (l *Ledger) MarkTransition(txid, next, cause string) error {
	transaction, ok := l.state.Transactions[txid]
	if !ok {
		return fmt.Errorf("ledger: unknown transaction %s", txid)
	}
	switch {
	case transaction.State == TxPrepared && (next == TxResultUnknown || next == TxObserved || next == TxFailed):
	case transaction.State == TxResultUnknown && (next == TxResultUnknown || next == TxObserved || next == TxFailed):
	case transaction.State == next:
	default:
		return fmt.Errorf("ledger: invalid transition %s -> %s", transaction.State, next)
	}
	if next == TxResultUnknown {
		transaction.Attempts++
	}
	transaction.State = next
	transaction.LastError = cause
	transaction.UpdatedAt = now()
	return l.Save()
}

// Unresolved returns prepared and result-unknown transactions, sorted by txid.
func (l *Ledger) Unresolved() []*Transaction {
	result := make([]*Transaction, 0)
	for _, transaction := range l.state.Transactions {
		if transaction.State == TxPrepared || transaction.State == TxResultUnknown {
			result = append(result, transaction)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].TxID < result[j].TxID })
	return result
}

// Transaction returns one transaction.
func (l *Ledger) Transaction(txid string) (*Transaction, bool) {
	transaction, ok := l.state.Transactions[txid]
	return transaction, ok
}

// EnsurePool creates a pool idempotently.
func (l *Ledger) EnsurePool(id, sellerID, sessionID string, amount uint64) error {
	if id == "" || sellerID == "" || sessionID == "" {
		return errors.New("ledger: pool id, sellerId and sessionId are required")
	}
	if existing, ok := l.state.Pools[id]; ok {
		if existing.SellerID != sellerID || existing.SessionID != sessionID || existing.Amount != amount {
			return fmt.Errorf("ledger: pool %s already exists with different parameters", id)
		}
		return nil
	}
	l.state.Pools[id] = &Pool{ID: id, SellerID: sellerID, SessionID: sessionID, Amount: amount, State: PoolFundingPending, UpdatedAt: now()}
	return l.Save()
}

// SetPoolState validates and applies a pool lifecycle transition.
func (l *Ledger) SetPoolState(id, next string) error {
	pool, ok := l.state.Pools[id]
	if !ok {
		return fmt.Errorf("ledger: unknown pool %s", id)
	}
	switch {
	case pool.State == PoolFundingPending && (next == PoolOpen || next == PoolFundingFailed):
	case pool.State == PoolOpen && next == PoolRecoveryPending:
	case pool.State == PoolRecoveryPending && next == PoolClosed:
	case pool.State == next:
	default:
		return fmt.Errorf("ledger: invalid pool transition %s -> %s", pool.State, next)
	}
	pool.State = next
	pool.UpdatedAt = now()
	return l.Save()
}

// CommitPoolPayment records an accepted cumulative payment for the pool.
func (l *Ledger) CommitPoolPayment(id string, sats uint64) error {
	pool, ok := l.state.Pools[id]
	if !ok {
		return fmt.Errorf("ledger: unknown pool %s", id)
	}
	if sats == 0 {
		return errors.New("ledger: payment must be positive")
	}
	if pool.Committed+sats > pool.Amount {
		return fmt.Errorf("ledger: pool %s overspend", id)
	}
	pool.Committed += sats
	pool.UpdatedAt = now()
	return l.Save()
}

// ReconcilePoolCommitted sets the cumulative committed amount of a pool to the
// amount proven by the buyer payment journal. It is idempotent and only ever
// moves forward, so a crash between journal append and ledger update cannot
// charge twice or lose a round.
func (l *Ledger) ReconcilePoolCommitted(id string, sats uint64) error {
	pool, ok := l.state.Pools[id]
	if !ok {
		return fmt.Errorf("ledger: unknown pool %s", id)
	}
	if sats > pool.Amount {
		return fmt.Errorf("ledger: pool %s overspend", id)
	}
	if sats < pool.Committed {
		return fmt.Errorf("ledger: pool %s committed amount would regress", id)
	}
	if sats == pool.Committed {
		return nil
	}
	pool.Committed = sats
	pool.UpdatedAt = now()
	return l.Save()
}

// SetPoolTransaction links a prepared transaction to a pool.
func (l *Ledger) SetPoolTransaction(id, kind, txid string) error {
	pool, ok := l.state.Pools[id]
	if !ok {
		return fmt.Errorf("ledger: unknown pool %s", id)
	}
	if _, ok := l.state.Transactions[txid]; !ok {
		return fmt.Errorf("ledger: transaction %s is not prepared", txid)
	}
	switch kind {
	case KindFunding:
		pool.FundingTxID = txid
	case KindClose:
		pool.RecoveryTxID = txid
	case KindRefund:
		pool.RefundTxID = txid
	default:
		return fmt.Errorf("ledger: invalid pool transaction kind %q", kind)
	}
	pool.UpdatedAt = now()
	return l.Save()
}

// Pools returns a copy of the pool map.
func (l *Ledger) Pools() map[string]Pool {
	result := make(map[string]Pool, len(l.state.Pools))
	for id, pool := range l.state.Pools {
		result[id] = *pool
	}
	return result
}

func isKind(kind string) bool {
	return kind == KindFunding || kind == KindClose || kind == KindRefund || kind == KindArbitration
}
