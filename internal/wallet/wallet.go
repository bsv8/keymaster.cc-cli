// Package wallet implements the P2PKH wallet used by km.
//
// Selection, occupation and fee validation are decided by the deterministic
// UTXO manager (keymaster.cc-cli/utxo, contract utm/1). The wallet is a
// thin trusted adapter: WoC snapshots in, real signing, exact-bytes
// broadcast, same SpendResult shape as before. Standard P2PKH flows produce
// byte-identical fees to the previous implementation (10+148n+34m with
// largest-first); the behavioral changes are the mandated ones:
//
//   - unknown broadcast results keep inputs occupied under the outbox
//     responsibility instead of releasing them;
//   - adopted outputs of unsettled broadcasts are not selectable;
//   - manager state persists in the unresolved directory, legacy unresolved
//     records are imported once, and never-broadcast pendings are dropped on
//     startup (same restart semantics as the old memory-only occupation).
package wallet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	manager "github.com/bsv8/keymaster.cc-cli/utxo"

	"github.com/bsv8/keymaster.cc-cli/internal/state"
	"github.com/bsv8/keymaster.cc-cli/internal/woc"
)

// Chain is the wallet's chain port: a fresh unspent snapshot and broadcast of
// exact raw bytes.
type Chain interface {
	ListUnspent(ctx context.Context, address string) ([]woc.UTXO, error)
	Broadcast(ctx context.Context, rawHex string) (string, error)
}

// txFetcher is the optional read-only observation port used by Recover. The
// production WoC client implements it; chains without it skip settling.
type txFetcher interface {
	TransactionRaw(ctx context.Context, txid string) ([]byte, error)
}

// Options configures a wallet.
type Options struct {
	// Network is "main" or "test" and only affects address encoding.
	Network string
	// PrivateKey is the 32-byte secp256k1 private key.
	PrivateKey []byte
	// FeeRateSatoshisPerKb is the miner fee rate; must be positive.
	FeeRateSatoshisPerKb int64
	// UnresolvedDir receives records whose broadcast result is unknown and
	// the manager state blob. Empty disables recording and persistence.
	UnresolvedDir string
}

// Output is one payment target.
type Output struct {
	Satoshis      uint64
	LockingScript *script.Script
}

// SpendResult describes a built (and possibly broadcast) wallet transaction.
type SpendResult struct {
	TxID    string
	RawHex  string
	Inputs  []woc.UTXO
	Fee     uint64
	Change  uint64
	Address string
	// AllocationID links the spend to its manager allocation for
	// release/abort. Empty for externally built transactions.
	AllocationID string
	// Transaction is the signed in-memory transaction (source outputs are
	// attached, which the wire bytes do not carry).
	Transaction *transaction.Transaction
}

// sharedCore is the manager state for one wallet instance, persisted to
// the unresolved directory so restarts keep unknown responsibility.
// A directory is held by at most one wallet process-wide (registry) and
// one process system-wide (flock); see sharedCoreFor/Close.
type sharedCore struct {
	mu          sync.Mutex
	kernel      *manager.FundingKernel
	rev         int64
	statePath   string
	results     map[string]string
	lockFile    *os.File
	degraded    bool
	degradedErr error
	key         string
}

// Wallet is a single-key P2PKH wallet.
type Wallet struct {
	chain         Chain
	privateKey    *ec.PrivateKey
	unlocker      *p2pkh.P2PKH
	address       *script.Address
	lockingScript *script.Script
	lockingHex    string
	feeRate       int64
	unresolvedDir string

	core *sharedCore
	// localMu serializes every operation with Close; core.mu serializes manager state.
	localMu           sync.Mutex
	closed            bool
	conclusionRunning map[string]bool
}

// New creates a wallet from a serialized private key.
func New(chain Chain, options Options) (*Wallet, error) {
	if chain == nil {
		return nil, errors.New("wallet: chain port is required")
	}
	if options.Network != "main" && options.Network != "test" {
		return nil, fmt.Errorf("wallet: unsupported network %q", options.Network)
	}
	if len(options.PrivateKey) != 32 {
		return nil, errors.New("wallet: private key must be 32 bytes")
	}
	if options.FeeRateSatoshisPerKb <= 0 {
		return nil, errors.New("wallet: fee rate must be positive")
	}
	privateKey, _ := ec.PrivateKeyFromBytes(options.PrivateKey)
	if privateKey == nil {
		return nil, errors.New("wallet: invalid private key")
	}
	address, err := script.NewAddressFromPublicKey(privateKey.PubKey(), options.Network == "main")
	if err != nil {
		return nil, fmt.Errorf("wallet: derive address: %w", err)
	}
	lockingScript, err := p2pkh.Lock(address)
	if err != nil {
		return nil, fmt.Errorf("wallet: build locking script: %w", err)
	}
	unlocker, err := p2pkh.Unlock(privateKey, nil)
	if err != nil {
		return nil, fmt.Errorf("wallet: build unlocker: %w", err)
	}
	scope := manager.Scope{Owner: hex.EncodeToString(privateKey.PubKey().Compressed()), Network: options.Network}
	w := &Wallet{
		chain:         chain,
		privateKey:    privateKey,
		unlocker:      unlocker,
		address:       address,
		lockingScript: lockingScript,
		lockingHex:    lockingScript.String(),
		feeRate:       options.FeeRateSatoshisPerKb,
		unresolvedDir: options.UnresolvedDir,
	}
	core, err := sharedCoreFor(scope, lockingScript.String(), options.UnresolvedDir)
	if err != nil {
		return nil, err
	}
	w.core = core
	// Import legacy unresolved records once: they keep their responsibility
	// inside the manager instead of living only in the history file.
	if options.UnresolvedDir != "" {
		if err := w.importLegacy(); err != nil {
			_ = w.Close()
			return nil, fmt.Errorf("wallet: import unresolved history: %w", err)
		}
	}
	return w, nil
}

var (
	registryMu sync.Mutex
	registry   = map[string]*sharedCore{}
)

func sharedCoreFor(scope manager.Scope, walletScript, dir string) (*sharedCore, error) {
	if dir == "" {
		kernel, err := manager.NewFundingKernel(scope, manager.ManagerConfig{
			WalletScriptHex: walletScript, DustThresholdSats: "1", MaxUnconfirmedAncestors: 32,
		})
		if err != nil {
			return nil, err
		}
		return &sharedCore{kernel: kernel}, nil
	}
	key := dir + "\x00" + scope.Owner + "\x00" + scope.Network
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[key]; ok {
		return nil, fmt.Errorf("wallet: directory %s is already held by another wallet instance; close it first", dir)
	}
	// Cross-process single writer: the lock file lives as long as the core.
	lockFile, err := os.OpenFile(filepath.Join(dir, "manager.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		if mkErr := os.MkdirAll(dir, 0700); mkErr != nil {
			return nil, mkErr
		}
		lockFile, err = os.OpenFile(filepath.Join(dir, "manager.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
	}
	if err := flockExclusive(lockFile); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("wallet: directory %s is held by another process: %w", dir, err)
	}
	statePath := filepath.Join(dir, "manager.json")
	kernel, err := manager.NewFundingKernel(scope, manager.ManagerConfig{
		WalletScriptHex: walletScript, DustThresholdSats: "1", MaxUnconfirmedAncestors: 32,
	})
	if err != nil {
		_ = lockFile.Close()
		return nil, err
	}
	core := &sharedCore{kernel: kernel, statePath: statePath, lockFile: lockFile, key: key}
	if blob, err := readFileStrict(statePath); err != nil {
		_ = lockFile.Close()
		return nil, err
	} else if blob != nil {
		if len(blob) == 0 {
			_ = flockRelease(lockFile)
			_ = lockFile.Close()
			return nil, fmt.Errorf("wallet: manager state file is empty")
		}
		stateBlob, results, decodeErr := decodeManagerFile(blob)
		if decodeErr != nil {
			_ = flockRelease(lockFile)
			_ = lockFile.Close()
			return nil, decodeErr
		}
		core.results = results
		restored, err := manager.Restore(stateBlob, scope)
		if err != nil {
			_ = flockRelease(lockFile)
			_ = lockFile.Close()
			return nil, fmt.Errorf("wallet: restore manager state: %w", err)
		}
		restored.DropPending()
		core.kernel = restored
		core.rev = restored.SnapshotRevision()
		if err := core.saveLocked(); err != nil {
			_ = lockFile.Close()
			return nil, err
		}
	}
	registry[key] = core
	return core, nil
}

// Close revokes this handle after every in-flight wallet operation has left its
// critical section, then releases the process and directory writer locks.
func (w *Wallet) Close() error {
	if w == nil || w.core == nil {
		return nil
	}
	w.localMu.Lock()
	defer w.localMu.Unlock()
	w.core.mu.Lock()
	defer w.core.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.core.key != "" {
		registryMu.Lock()
		if current, ok := registry[w.core.key]; ok && current == w.core {
			delete(registry, w.core.key)
		}
		registryMu.Unlock()
	}
	if w.core.lockFile != nil {
		if err := flockRelease(w.core.lockFile); err != nil {
			return err
		}
		if err := w.core.lockFile.Close(); err != nil {
			return err
		}
		w.core.lockFile = nil
	}
	return nil
}

func (c *sharedCore) saveLocked() error {
	if c.statePath == "" {
		return nil
	}
	blob, err := c.kernel.Serialize()
	if err != nil {
		c.degraded = true
		c.degradedErr = err
		return err
	}
	return c.writeManagerBlobLocked(blob, "", "")
}

func (w *Wallet) checkLiveLocked() error {
	if w.closed {
		return errors.New("wallet: wallet handle is closed")
	}
	return w.core.checkLive()
}

// persistCandidateLocked writes a detached state before publishing it to the
// live kernel. Any failed write leaves the existing responsibility unchanged.
func (c *sharedCore) persistCandidateLocked(candidate *manager.FundingKernel) error {
	blob, err := candidate.Serialize()
	if err != nil {
		c.degraded = true
		c.degradedErr = err
		return err
	}
	if err := c.writeManagerBlobLocked(blob, "", ""); err != nil {
		return err
	}
	if err := c.kernel.ReplaceFrom(candidate); err != nil {
		c.degraded = true
		c.degradedErr = err
		return err
	}
	c.degraded = false
	c.degradedErr = nil
	return nil
}

func (c *sharedCore) checkLive() error {
	if c.degraded {
		cause := "unknown persistence error"
		if c.degradedErr != nil {
			cause = c.degradedErr.Error()
		}
		return fmt.Errorf("wallet: persistence degraded (%s); run Recover", cause)
	}
	return nil
}

// importLegacy moves pre-manager unresolved records under manager
// responsibility. Records the kernel already tracks (it writes the same
// history file) are skipped.
func (w *Wallet) importLegacy() error {
	records, err := state.List(w.unresolvedDir)
	if err != nil {
		return err
	}
	w.core.mu.Lock()
	defer w.core.mu.Unlock()
	candidate, err := w.core.kernel.Clone()
	if err != nil {
		return err
	}
	known := map[string]string{}
	for _, entry := range candidate.OutboxView() {
		known[strings.ToLower(entry.TxID)] = strings.ToLower(entry.RawTxHex)
	}
	for _, record := range records {
		if record.TxID == "" && record.RawTxHex == "" {
			// Other unresolved records (for example BitFS task journals) are
			// not exact broadcast transactions owned by the wallet manager.
			continue
		}
		if record.TxID == "" || record.RawTxHex == "" {
			return fmt.Errorf("unresolved record %q has incomplete transaction evidence", record.Kind)
		}
		key := strings.ToLower(record.TxID)
		if prior, exists := known[key]; exists {
			if prior != strings.ToLower(record.RawTxHex) {
				return fmt.Errorf("unresolved tx %s conflicts with manager exact bytes", record.TxID)
			}
			continue
		}
		if _, err := candidate.ImportUnknown(record.TxID, record.RawTxHex); err != nil {
			return fmt.Errorf("import unresolved tx %s: %w", record.TxID, err)
		}
		known[key] = strings.ToLower(record.RawTxHex)
	}
	return w.core.persistCandidateLocked(candidate)
}

// Address returns the wallet's P2PKH address.
func (w *Wallet) Address() string { return w.address.AddressString }

// LockingScript returns the wallet's own P2PKH locking script.
func (w *Wallet) LockingScript() *script.Script { return w.lockingScript }

func (w *Wallet) feeRateString() string {
	return strconv.FormatInt(w.feeRate, 10)
}

// snapshotLocked refreshes the manager from a live WoC listing.
func (w *Wallet) snapshotLocked(ctx context.Context) (map[string]woc.UTXO, error) {
	utxos, err := w.chain.ListUnspent(ctx, w.Address())
	if err != nil {
		return nil, err
	}
	w.core.rev++
	rows := make([]manager.SnapshotUtxo, 0, len(utxos))
	cache := make(map[string]woc.UTXO, len(utxos))
	for _, utxo := range utxos {
		key := fmt.Sprintf("%s:%d", utxo.TxID, utxo.Vout)
		cache[key] = utxo
		status := "confirmed"
		if utxo.Height <= 0 {
			status = "unconfirmed"
		}
		rows = append(rows, manager.SnapshotUtxo{
			TxID: utxo.TxID, Vout: utxo.Vout, Value: strconv.FormatUint(utxo.Value, 10),
			ScriptHex: w.lockingHex, Status: status, Height: utxo.Height,
		})
	}
	if _, err := w.core.kernel.ApplySnapshot(manager.SnapshotInput{Generation: 1, Revision: w.core.rev, Utxos: rows}); err != nil {
		return nil, err
	}
	if err := w.core.saveLocked(); err != nil {
		return nil, err
	}
	return cache, nil
}

// Available returns the spendable satoshis from the manager projection. It is
// a snapshot for host budgeting, not a reservation.
func (w *Wallet) Available(ctx context.Context) (uint64, error) {
	w.localMu.Lock()
	defer w.localMu.Unlock()
	w.core.mu.Lock()
	defer w.core.mu.Unlock()
	if err := w.checkLiveLocked(); err != nil {
		return 0, err
	}
	if _, err := w.snapshotLocked(ctx); err != nil {
		return 0, err
	}
	balance, err := manager.ParseAmount(w.core.kernel.SpendableBalance(), "balance")
	if err != nil {
		return 0, err
	}
	return balance, nil
}

func newAllocationID() string {
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	return "w-" + hex.EncodeToString(nonce[:])
}

// Build selects inputs and signs a transaction paying outputs. It never
// broadcasts: callers persist the exact bytes first and broadcast later with
// BroadcastPrepared. Selected inputs stay occupied in the manager until the
// broadcast settles or the allocation is aborted.
func (w *Wallet) Build(ctx context.Context, outputs []Output) (*SpendResult, error) {
	if len(outputs) == 0 {
		return nil, errors.New("wallet: at least one output is required")
	}
	var target uint64
	for _, output := range outputs {
		if output.LockingScript == nil {
			return nil, errors.New("wallet: output locking script is required")
		}
		var err error
		target, err = manager.AddChecked(target, output.Satoshis, "outputs")
		if err != nil {
			return nil, err
		}
	}
	if target == 0 {
		return nil, errors.New("wallet: output amounts must be positive")
	}

	w.localMu.Lock()
	defer w.localMu.Unlock()
	w.core.mu.Lock()
	defer w.core.mu.Unlock()
	if err := w.checkLiveLocked(); err != nil {
		return nil, err
	}

	cache, err := w.snapshotLocked(ctx)
	if err != nil {
		return nil, err
	}
	managerOutputs := make([]manager.RequestOutput, 0, len(outputs))
	for _, output := range outputs {
		managerOutputs = append(managerOutputs, manager.RequestOutput{
			ScriptHex: output.LockingScript.String(), Value: strconv.FormatUint(output.Satoshis, 10),
		})
	}
	allocationID := newAllocationID()
	candidate, err := w.core.kernel.Clone()
	if err != nil {
		return nil, err
	}
	view, err := candidate.Request(manager.RequestInput{
		RequestID: allocationID, BusinessAmount: strconv.FormatUint(target, 10),
		FeeRate: w.feeRateString(), Outputs: managerOutputs,
	})
	if err != nil {
		return nil, mapManagerError(err)
	}
	chosen := make([]woc.UTXO, 0, len(view.Inputs))
	unsignedInputs := make([]manager.UnsignedInput, 0, len(view.Inputs))
	for _, key := range view.Inputs {
		cached, ok := cache[key]
		if !ok {
			// Local outputs (change, adopted proceeds) are not in the WoC
			// listing: resolve value and script from the kernel instead.
			looked, lookupErr := candidate.Lookup(key)
			if lookupErr != nil {
				return nil, fmt.Errorf("wallet: snapshot input %s vanished", key)
			}
			value, parseErr := manager.ParseAmount(looked.Value, "input value")
			if parseErr != nil {
				return nil, parseErr
			}
			parts := strings.Split(key, ":")
			vout, voutErr := strconv.ParseUint(parts[1], 10, 32)
			if voutErr != nil {
				return nil, voutErr
			}
			cached = woc.UTXO{TxID: parts[0], Vout: uint32(vout), Value: value, Height: 0}
		}
		chosen = append(chosen, cached)
		unsignedInputs = append(unsignedInputs, manager.UnsignedInput{
			TxID: cached.TxID, Vout: cached.Vout, Sequence: transaction.DefaultSequenceNumber, HasSeq: true,
		})
	}
	unsignedOutputs := make([]manager.UnsignedOutput, 0, len(outputs)+1)
	for _, output := range outputs {
		unsignedOutputs = append(unsignedOutputs, manager.UnsignedOutput{Value: output.Satoshis, Script: output.LockingScript.String()})
	}
	change, err := manager.ParseAmount(view.ChangeSats, "change")
	if err != nil {
		return nil, err
	}
	if change > 0 {
		unsignedOutputs = append(unsignedOutputs, manager.UnsignedOutput{Value: change, Script: w.lockingHex})
	}
	unsignedHex, err := manager.BuildUnsigned(1, 0, unsignedInputs, unsignedOutputs)
	if err != nil {
		return nil, err
	}
	if _, err := candidate.Prepare(manager.PrepareInput{AllocationID: allocationID, UnsignedTxHex: unsignedHex}); err != nil {
		return nil, mapManagerError(err)
	}
	if err := candidate.MarkSigningIntentDurable(allocationID); err != nil {
		return nil, mapManagerError(err)
	}
	if err := w.core.persistCandidateLocked(candidate); err != nil {
		return nil, fmt.Errorf("wallet: persist signing intent: %w", err)
	}
	abortIntent := func(cause error) error {
		cleanup, cloneErr := w.core.kernel.Clone()
		if cloneErr != nil {
			return errors.Join(cause, cloneErr)
		}
		if abortErr := cleanup.AbortUnbroadcast(allocationID); abortErr != nil {
			return errors.Join(cause, abortErr)
		}
		if persistErr := w.core.persistCandidateLocked(cleanup); persistErr != nil {
			return errors.Join(cause, persistErr)
		}
		return cause
	}

	tx := transaction.NewTransaction()
	for _, utxo := range chosen {
		if err := tx.AddInputFrom(utxo.TxID, utxo.Vout, w.lockingHex, utxo.Value, w.unlocker); err != nil {
			return nil, abortIntent(fmt.Errorf("wallet: add input: %w", err))
		}
	}
	for _, output := range outputs {
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: output.Satoshis, LockingScript: output.LockingScript})
	}
	if change > 0 {
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: change, LockingScript: w.lockingScript, Change: true})
	}
	if err := tx.Sign(); err != nil {
		return nil, abortIntent(fmt.Errorf("wallet: sign transaction: %w", err))
	}
	var total uint64
	for _, utxo := range chosen {
		total += utxo.Value
	}
	expectedFee, err := manager.ParseAmount(view.FeeSats, "fee")
	if err != nil {
		return nil, abortIntent(err)
	}
	if actualFee := total - tx.TotalOutputSatoshis(); actualFee != expectedFee {
		return nil, abortIntent(fmt.Errorf("wallet: fee mismatch: computed %d actual %d", expectedFee, actualFee))
	}
	rawHex := tx.Hex()
	txid := tx.TxID().String()
	completed, err := w.core.kernel.Clone()
	if err != nil {
		return nil, abortIntent(err)
	}
	if _, err := completed.CompletePrepare(manager.CompletePrepareInput{AllocationID: allocationID, SignedTxHex: rawHex}); err != nil {
		return nil, abortIntent(mapManagerError(err))
	}
	if err := w.core.persistCandidateLocked(completed); err != nil {
		return nil, abortIntent(fmt.Errorf("wallet: persist signed transaction: %w", err))
	}
	fee, _ := manager.ParseAmount(view.FeeSats, "fee")
	return &SpendResult{TxID: txid, RawHex: rawHex, Inputs: chosen, Fee: fee, Change: change,
		Address: w.Address(), AllocationID: allocationID, Transaction: tx}, nil
}

// BroadcastPrepared broadcasts only the persisted outbox bytes. Dispatch and
// each settlement are persisted on detached candidates before publication.
func (w *Wallet) BroadcastPrepared(ctx context.Context, kind string, prepared *SpendResult) (string, error) {
	if prepared == nil {
		return "", errors.New("wallet: prepared transaction is required")
	}
	w.localMu.Lock()
	defer w.localMu.Unlock()
	w.core.mu.Lock()
	defer w.core.mu.Unlock()
	if err := w.checkLiveLocked(); err != nil {
		return "", err
	}
	commandID := prepared.commandID()
	var exactHex, exactTxID, exactState string
	for _, entry := range w.core.kernel.OutboxView() {
		if entry.CommandID == commandID {
			exactHex, exactTxID, exactState = entry.RawTxHex, entry.TxID, entry.State
			break
		}
	}
	if exactHex == "" {
		return "", fmt.Errorf("wallet: no persisted outbox entry for %s", commandID)
	}
	if !strings.EqualFold(exactHex, prepared.RawHex) || !strings.EqualFold(exactTxID, prepared.TxID) {
		return "", errors.New("wallet: prepared bytes/txid do not match persisted outbox entry")
	}
	if exactState == "done" {
		return exactTxID, nil
	}
	if exactState == "failed" {
		result, err := manager.RetryExact(w.core.kernel, manager.Ports{}, prepared.AllocationID)
		if err != nil {
			return "", err
		}
		return "", &InputConflictError{Result: result}
	}
	dispatch, err := w.core.kernel.Clone()
	if err != nil {
		return "", err
	}
	if err := dispatch.MarkDispatching(commandID); err != nil {
		return "", err
	}
	if err := w.core.persistCandidateLocked(dispatch); err != nil {
		return "", fmt.Errorf("wallet: persist dispatch intent: %w", err)
	}
	diagnostic := ""
	settle := func(report manager.BroadcastReport) error {
		candidate, err := w.core.kernel.Clone()
		if err != nil {
			return err
		}
		if err := candidate.BroadcastResult(commandID, report); err != nil {
			return err
		}
		blob, err := candidate.Serialize()
		if err != nil {
			return err
		}
		result, err := json.Marshal(report)
		if err != nil {
			return err
		}
		if err := (managerStore{core: w.core, diagnostic: &diagnostic}).RecordResult(commandID, string(result), blob); err != nil {
			return err
		}
		return w.core.kernel.ReplaceFrom(candidate)
	}
	outcome, broadcastErr := (contextBroadcaster{ctx: ctx, chain: w.chain, diagnostic: &diagnostic}).Broadcast(exactHex)
	broadcastTxID := outcome.TxID
	if broadcastErr != nil {
		if err := settle(manager.BroadcastReport{Status: "unknown", TxID: exactTxID}); err != nil {
			return "", fmt.Errorf("wallet: broadcast: %v; persist result: %w", broadcastErr, err)
		}
		var conflict *woc.BroadcastError
		if prepared.AllocationID != "" && errors.As(broadcastErr, &conflict) && conflict.Receipt.Status == "conflict" {
			observer := inputObserverFor(ctx, w.chain, &diagnostic)
			if observer != nil {
				result, err := manager.ReconcileInputs(w.core.kernel, manager.Ports{Store: managerStore{core: w.core, diagnostic: &diagnostic}, InputObserver: observer}, prepared.AllocationID)
				if err != nil {
					return "", fmt.Errorf("wallet: reconcile conflict: %w", err)
				}
				if result.Delivery == "accepted" {
					return result.TxID, nil
				}
				if result.Delivery == "failed" {
					return "", &InputConflictError{Result: result}
				}
			}
		}
		if err := w.recordUnresolved(kind, exactTxID, exactHex, broadcastErr); err != nil {
			return "", fmt.Errorf("wallet: broadcast: %v; record unresolved: %w", broadcastErr, err)
		}
		return "", fmt.Errorf("wallet: broadcast: %w", broadcastErr)
	}
	if !strings.EqualFold(broadcastTxID, exactTxID) {
		mismatch := fmt.Errorf("provider returned txid %s", broadcastTxID)
		if err := settle(manager.BroadcastReport{Status: "unknown", TxID: exactTxID}); err != nil {
			return "", fmt.Errorf("wallet: txid mismatch; persist result: %w", err)
		}
		if err := w.recordUnresolved(kind, exactTxID, exactHex, mismatch); err != nil {
			return "", fmt.Errorf("wallet: txid mismatch; record unresolved: %w", err)
		}
		return "", fmt.Errorf("wallet: provider txid %s does not match local txid %s", broadcastTxID, exactTxID)
	}
	report := manager.BroadcastReport{Status: "accepted", TxID: exactTxID}
	if outcome.Status == "already-known" {
		report.Status = "already-known"
		report.RawTxHex = exactHex
	}
	if err := settle(report); err != nil {
		return "", fmt.Errorf("wallet: persist accepted result: %w", err)
	}
	return broadcastTxID, nil
}

// ProtocolProof is trusted prevout evidence for one non-wallet input of a
// protocol transaction, bound by the caller to SDK-verified pool records
// (exact funding/payment raw bytes), never self-asserted by the tx itself.
type ProtocolProof struct {
	Value     uint64
	ScriptHex string
}

// ProtocolBinding marks the protocol role of a submit.
type ProtocolBinding struct {
	Kind   string
	PoolID string
}

// ProtocolRequest is an explicit protocol submit: business cap, fee policy,
// declared non-self outputs, signed bytes, per-input proofs and binding.
type ProtocolRequest struct {
	Business  string
	FeeRate   string
	Declared  []Output
	SignedHex string
	Proofs    map[string]ProtocolProof
	Binding   ProtocolBinding
	// FeeCapSats optionally caps the miner fee of this transaction. When it
	// is empty the wallet derives the same rate-based ceiling the shared
	// manager uses for protocol spends.
	FeeCapSats string
	// FeePolicy is "rate" (the fee must be at least rate x size) or "exact"
	// (the business approved a fixed fee; only the ceiling applies).
	FeePolicy string
}

// PrepareProtocol validates a protocol-signed transaction against an
// explicit business anchor and persists the exact bytes. It is the ONLY
// path for protocol spends (refunds, closes); there is no generic
// broadcast-exact entry point. Broadcast with BroadcastPrepared.
//
// The manager owns the money rules: every actual input is classified
// (wallet / protected / external) against the kernel's own facts, the miner
// fee is bounded by the requested rate and the wallet's net outflow by the
// business cap. The exact signed bytes then become a durable dispatch
// command with the input responsibility attached.
func (w *Wallet) PrepareProtocol(ctx context.Context, kind string, req ProtocolRequest) (*SpendResult, error) {
	if req.SignedHex == "" {
		return nil, errors.New("wallet: signed transaction is required")
	}
	if req.Binding.Kind != "refund" && req.Binding.Kind != "close" {
		return nil, fmt.Errorf("wallet: bad protocol kind %q", req.Binding.Kind)
	}
	w.localMu.Lock()
	defer w.localMu.Unlock()
	w.core.mu.Lock()
	defer w.core.mu.Unlock()
	if err := w.checkLiveLocked(); err != nil {
		return nil, err
	}
	declared := make([]manager.RequestOutput, 0, len(req.Declared))
	for _, output := range req.Declared {
		if output.LockingScript == nil {
			return nil, errors.New("wallet: declared output locking script is required")
		}
		declared = append(declared, manager.RequestOutput{
			ScriptHex: output.LockingScript.String(), Value: strconv.FormatUint(output.Satoshis, 10),
		})
	}
	parsed, err := transaction.NewTransactionFromHex(req.SignedHex)
	if err != nil {
		return nil, err
	}
	feePolicy, feeCap, err := protocolFeeBounds(req, parsed)
	if err != nil {
		return nil, err
	}
	if recovered, err := w.recoverProtocolPreparationLocked(req, parsed, declared, feePolicy, feeCap); recovered != nil || err != nil {
		return recovered, err
	}
	if _, err := w.snapshotLocked(ctx); err != nil {
		return nil, err
	}
	allocationID := newAllocationID()
	candidate, err := w.core.kernel.Clone()
	if err != nil {
		return nil, err
	}
	anchor, err := candidate.RequestProtocol(manager.RequestInput{
		RequestID: allocationID, BusinessAmount: req.Business, FeeRate: req.FeeRate,
		Outputs: declared,
	})
	if err != nil {
		return nil, mapManagerError(err)
	}
	_ = anchor
	proofs := make(map[string]manager.ExternalProof, len(req.Proofs))
	for key, proof := range req.Proofs {
		proofs[key] = manager.ExternalProof{Value: strconv.FormatUint(proof.Value, 10), ScriptHex: proof.ScriptHex}
	}
	// Classify every actual input against the kernel's own facts: a wallet
	// input must be a proven self output the manager knows, everything else
	// needs a trusted prevout proof.
	facts := make([]manager.ProtocolInputFact, 0, len(parsed.Inputs))
	for _, in := range parsed.Inputs {
		key := fmt.Sprintf("%s:%d", strings.ToLower(in.SourceTXID.String()), in.SourceTxOutIndex)
		proof, hasProof := req.Proofs[key]
		looked, lookupErr := candidate.Lookup(key)
		if lookupErr == nil && strings.EqualFold(looked.ScriptHex, w.lockingHex) {
			if looked.Protected {
				return nil, fmt.Errorf("wallet: protocol input %s is protected", key)
			}
			value, parseErr := manager.ParseAmount(looked.Value, "input value")
			if parseErr != nil {
				return nil, parseErr
			}
			facts = append(facts, manager.ProtocolInputFact{
				Outpoint: key, Class: manager.ProtocolInputWallet, Value: strconv.FormatUint(value, 10),
				ScriptHex: w.lockingHex,
			})
			continue
		}
		if !hasProof {
			return nil, fmt.Errorf("wallet: protocol input %s has no trusted prevout proof", key)
		}
		if strings.EqualFold(proof.ScriptHex, w.lockingHex) {
			return nil, fmt.Errorf("wallet: protocol input %s claims the wallet script without kernel proof", key)
		}
		facts = append(facts, manager.ProtocolInputFact{
			Outpoint: key, Class: manager.ProtocolInputExternal,
			Value: strconv.FormatUint(proof.Value, 10), ScriptHex: strings.ToLower(proof.ScriptHex),
		})
	}
	skeleton, err := manager.UnsignedSkeleton(req.SignedHex)
	if err != nil {
		return nil, err
	}
	if _, err := candidate.PrepareProtocolIntent(manager.ProtocolIntentInput{
		AllocationID: allocationID, UnsignedTxHex: skeleton, Inputs: facts,
		FeeCapSats: feeCap, FeePolicy: feePolicy,
		Binding:        &manager.ProtocolBinding{Kind: req.Binding.Kind, PoolID: req.Binding.PoolID},
		ExternalProofs: proofs,
	}); err != nil {
		return nil, mapManagerError(err)
	}
	if err := candidate.MarkSigningIntentDurable(allocationID); err != nil {
		return nil, mapManagerError(err)
	}
	completed, err := candidate.CompleteProtocol(manager.CompleteProtocolInput{
		AllocationID: allocationID, SignedTxHex: req.SignedHex, Delivery: "wallet",
	})
	if err != nil {
		return nil, mapManagerError(err)
	}
	txid, fee := completed.TxID, completed.FeeSats
	inputs := make([]woc.UTXO, 0, len(parsed.Inputs))
	for _, in := range parsed.Inputs {
		prevTxid := in.SourceTXID.String()
		key := fmt.Sprintf("%s:%d", prevTxid, in.SourceTxOutIndex)
		value := uint64(0)
		if proof, ok := req.Proofs[key]; ok {
			value = proof.Value
		} else if looked, lookupErr := candidate.Lookup(key); lookupErr == nil {
			parsedValue, parseErr := manager.ParseAmount(looked.Value, "input value")
			if parseErr != nil {
				return nil, parseErr
			}
			value = parsedValue
		}
		inputs = append(inputs, woc.UTXO{TxID: prevTxid, Vout: in.SourceTxOutIndex, Value: value})
	}
	feeAmount, err := manager.ParseAmount(fee, "protocol fee")
	if err != nil {
		return nil, err
	}
	if err := w.core.persistCandidateLocked(candidate); err != nil {
		// The caller supplied signed bytes before this call; retain the full
		// responsibility in memory and poison the wallet rather than releasing.
		_ = w.core.kernel.ReplaceFrom(candidate)
		return nil, fmt.Errorf("wallet: persist protocol responsibility: %w", err)
	}
	return &SpendResult{TxID: txid, RawHex: strings.ToLower(req.SignedHex), Inputs: inputs,
		Fee: feeAmount, Change: 0, Address: w.Address(), AllocationID: allocationID, Transaction: parsed}, nil
}

// Spend builds, signs and broadcasts one transaction.
func (w *Wallet) Spend(ctx context.Context, kind string, outputs []Output) (*SpendResult, error) {
	prepared, err := w.Build(ctx, outputs)
	if err != nil {
		return nil, err
	}
	if _, err := w.BroadcastPrepared(ctx, kind, prepared); err != nil {
		return nil, err
	}
	return prepared, nil
}

// Release aborts a prepared-but-never-broadcast spend so a retry can select
// the inputs again. It is a no-op once the spend was broadcast, committed or
// already aborted.
func (w *Wallet) Release(prepared *SpendResult) error {
	if prepared == nil || prepared.AllocationID == "" {
		return nil
	}
	w.localMu.Lock()
	defer w.localMu.Unlock()
	w.core.mu.Lock()
	defer w.core.mu.Unlock()
	if err := w.checkLiveLocked(); err != nil {
		return err
	}
	candidate, err := w.core.kernel.Clone()
	if err != nil {
		return err
	}
	if err := candidate.AbortUnbroadcast(prepared.AllocationID); err != nil {
		return err
	}
	return w.core.persistCandidateLocked(candidate)
}

// Recover settles unknown outbox entries against the chain: entries observed
// with the same exact bytes commit, the rest keep their responsibility.
// Never-broadcast pendings were already dropped at startup.
func (w *Wallet) Recover(ctx context.Context) (recovered int, pending []string, err error) {
	w.localMu.Lock()
	defer w.localMu.Unlock()
	w.core.mu.Lock()
	defer w.core.mu.Unlock()
	if w.closed {
		return 0, nil, errors.New("wallet: wallet handle is closed")
	}
	fetcher, ok := w.chain.(txFetcher)
	if !ok {
		return 0, nil, errors.New("wallet: chain transaction observer is unavailable")
	}
	candidate, err := w.core.kernel.Clone()
	if err != nil {
		return 0, nil, err
	}
	for _, entry := range candidate.OutboxView() {
		if entry.State == "done" || entry.State == "failed" {
			continue
		}
		raw, fetchErr := fetcher.TransactionRaw(ctx, entry.TxID)
		if fetchErr != nil {
			pending = append(pending, entry.TxID)
			continue
		}
		result, reconcileErr := candidate.Reconcile(entry.CommandID, true, hex.EncodeToString(raw))
		if reconcileErr != nil {
			return recovered, pending, reconcileErr
		}
		if result == "committed" {
			recovered++
		} else {
			pending = append(pending, entry.TxID)
		}
	}
	if err := w.core.persistCandidateLocked(candidate); err != nil {
		return recovered, pending, err
	}
	return recovered, pending, nil
}

func (w *Wallet) recordUnresolved(kind, txid, rawHex string, cause error) error {
	if w.unresolvedDir == "" {
		return nil
	}
	if err := state.Append(w.unresolvedDir, state.Unresolved{
		RecordedAt: time.Now().UTC(),
		Kind:       kind,
		TxID:       txid,
		RawTxHex:   rawHex,
		Reason:     cause.Error(),
	}); err != nil {
		w.core.degraded = true
		w.core.degradedErr = err
		return err
	}
	return nil
}

func (r *SpendResult) commandID() string {
	if r.AllocationID == "" {
		return "imported:" + r.TxID
	}
	return r.AllocationID + ":broadcast"
}

// mapManagerError keeps manager failures as wallet errors without inventing
// new public error types; the coded reason stays in the message.
func mapManagerError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("wallet: manager: %w", err)
}
