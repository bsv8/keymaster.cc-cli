// Package wallet implements the P2PKH wallet used by km.
//
// Coin selection follows the reference behaviour: a fresh unspent snapshot,
// inputs added one at a time with the miner fee re-estimated for the actual
// input count, and change returned to the wallet. In v1 there is no maximum
// fee-rate guard and input occupation is released once the broadcast is
// accepted. Unresolved broadcast results are appended to the client-level
// unresolved history instead of blocking new work.
package wallet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	"github.com/bsv8/keymaster.cc-cli/internal/state"
	"github.com/bsv8/keymaster.cc-cli/internal/woc"
)

const (
	baseTransactionBytes = 10
	p2pkhInputBytes      = 148
	p2pkhOutputBytes     = 34
	dustThreshold        = 1
)

// Chain is the wallet's chain port: a fresh unspent snapshot and broadcast of
// exact raw bytes.
type Chain interface {
	ListUnspent(ctx context.Context, address string) ([]woc.UTXO, error)
	Broadcast(ctx context.Context, rawHex string) (string, error)
}

// Options configures a wallet.
type Options struct {
	// Network is "main" or "test" and only affects address encoding.
	Network string
	// PrivateKey is the 32-byte secp256k1 private key.
	PrivateKey []byte
	// FeeRateSatoshisPerKb is the miner fee rate; must be positive.
	FeeRateSatoshisPerKb int64
	// UnresolvedDir receives records whose broadcast result is unknown.
	// Empty disables recording.
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
	// Transaction is the signed in-memory transaction (source outputs are
	// attached, which the wire bytes do not carry).
	Transaction *transaction.Transaction
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

	mu       sync.Mutex
	occupied map[string]bool
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
	return &Wallet{
		chain:         chain,
		privateKey:    privateKey,
		unlocker:      unlocker,
		address:       address,
		lockingScript: lockingScript,
		lockingHex:    lockingScript.String(),
		feeRate:       options.FeeRateSatoshisPerKb,
		unresolvedDir: options.UnresolvedDir,
		occupied:      map[string]bool{},
	}, nil
}

// Address returns the wallet's P2PKH address.
func (w *Wallet) Address() string { return w.address.AddressString }

// Available returns the spendable satoshis that are currently unspent and not
// occupied by an in-flight spend. It is a snapshot for host budgeting, not a
// reservation.
func (w *Wallet) Available(ctx context.Context) (uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	utxos, err := w.chain.ListUnspent(ctx, w.Address())
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, utxo := range utxos {
		if w.occupied[inputKey(utxo.TxID, utxo.Vout)] {
			continue
		}
		total += utxo.Value
	}
	return total, nil
}

// LockingScript returns the wallet's own P2PKH locking script, used to
// identify outputs paying the wallet in arbitrary transactions.
func (w *Wallet) LockingScript() *script.Script { return w.lockingScript }

// Build selects inputs and signs a transaction paying outputs. It never
// broadcasts: callers persist the exact bytes first and broadcast later with
// BroadcastPrepared. Selected inputs stay occupied until the broadcast result
// is known.
func (w *Wallet) Build(ctx context.Context, outputs []Output) (*SpendResult, error) {
	if len(outputs) == 0 {
		return nil, errors.New("wallet: at least one output is required")
	}
	var target uint64
	for _, output := range outputs {
		if output.LockingScript == nil {
			return nil, errors.New("wallet: output locking script is required")
		}
		target += output.Satoshis
	}
	if target == 0 {
		return nil, errors.New("wallet: output amounts must be positive")
	}

	// Serialize spends so concurrent callers cannot select the same UTXOs.
	w.mu.Lock()
	defer w.mu.Unlock()

	utxos, err := w.chain.ListUnspent(ctx, w.Address())
	if err != nil {
		return nil, err
	}
	available := make([]woc.UTXO, 0, len(utxos))
	for _, utxo := range utxos {
		if w.occupied[inputKey(utxo.TxID, utxo.Vout)] {
			continue
		}
		available = append(available, utxo)
	}
	chosen, fee, change, err := w.selectInputs(available, target, len(outputs))
	if err != nil {
		return nil, err
	}
	var total uint64
	for _, utxo := range chosen {
		total += utxo.Value
	}

	tx := transaction.NewTransaction()
	for _, utxo := range chosen {
		if err := tx.AddInputFrom(utxo.TxID, utxo.Vout, w.lockingHex, utxo.Value, w.unlocker); err != nil {
			return nil, fmt.Errorf("wallet: add input: %w", err)
		}
	}
	for _, output := range outputs {
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: output.Satoshis, LockingScript: output.LockingScript})
	}
	if change > 0 {
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: change, LockingScript: w.lockingScript, Change: true})
	}
	if err := tx.Sign(); err != nil {
		return nil, fmt.Errorf("wallet: sign transaction: %w", err)
	}
	if actualFee := total - tx.TotalOutputSatoshis(); actualFee != fee {
		return nil, fmt.Errorf("wallet: fee mismatch: computed %d actual %d", fee, actualFee)
	}
	rawHex := tx.Hex()
	txid := tx.TxID().String()
	result := &SpendResult{TxID: txid, RawHex: rawHex, Inputs: chosen, Fee: fee, Change: change, Address: w.Address(), Transaction: tx}
	for _, utxo := range chosen {
		w.occupied[inputKey(utxo.TxID, utxo.Vout)] = true
	}
	return result, nil
}

// BroadcastPrepared broadcasts the exact bytes of a previously built
// transaction and then releases the input occupation (a push result ends the
// occupation in v1). Unknown or failed results are recorded in the unresolved
// history.
func (w *Wallet) BroadcastPrepared(ctx context.Context, kind string, prepared *SpendResult) (string, error) {
	if prepared == nil {
		return "", errors.New("wallet: prepared transaction is required")
	}
	broadcastTxID, err := w.chain.Broadcast(ctx, prepared.RawHex)
	w.release(prepared)
	if err != nil {
		w.recordUnresolved(kind, prepared.TxID, prepared.RawHex, err)
		return "", fmt.Errorf("wallet: broadcast: %w", err)
	}
	if broadcastTxID != prepared.TxID {
		w.recordUnresolved(kind, prepared.TxID, prepared.RawHex, fmt.Errorf("provider returned txid %s", broadcastTxID))
		return "", fmt.Errorf("wallet: provider txid %s does not match local txid %s", broadcastTxID, prepared.TxID)
	}
	return broadcastTxID, nil
}

// BroadcastRaw broadcasts exact external transaction bytes (for example an
// SDK-built pool refund) and records unknown results in the unresolved
// history. No wallet inputs are involved, so nothing is released here.
func (w *Wallet) BroadcastRaw(ctx context.Context, kind, rawHex string) (string, error) {
	if rawHex == "" {
		return "", errors.New("wallet: raw transaction is required")
	}
	parsed, err := transaction.NewTransactionFromHex(rawHex)
	if err != nil {
		return "", fmt.Errorf("wallet: parse raw transaction: %w", err)
	}
	txid := parsed.TxID().String()
	broadcastTxID, err := w.chain.Broadcast(ctx, rawHex)
	if err != nil {
		w.recordUnresolved(kind, txid, rawHex, err)
		return "", fmt.Errorf("wallet: broadcast: %w", err)
	}
	if broadcastTxID != txid {
		w.recordUnresolved(kind, txid, rawHex, fmt.Errorf("provider returned txid %s", broadcastTxID))
		return "", fmt.Errorf("wallet: provider txid %s does not match local txid %s", broadcastTxID, txid)
	}
	return txid, nil
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

// Release frees the inputs of a prepared-but-never-broadcast spend. Callers
// must not use it after a broadcast was attempted (the broadcast releases).
func (w *Wallet) Release(prepared *SpendResult) {
	if prepared == nil {
		return
	}
	w.release(prepared)
}

func (w *Wallet) release(prepared *SpendResult) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, utxo := range prepared.Inputs {
		delete(w.occupied, inputKey(utxo.TxID, utxo.Vout))
	}
}

func inputKey(txid string, vout uint32) string {
	return fmt.Sprintf("%s:%d", txid, vout)
}

// selectInputs adds UTXOs until the target and the estimated fee are covered.
// The fee is re-estimated for the actual input count; leftover value below the
// change threshold is absorbed into the fee.
func (w *Wallet) selectInputs(utxos []woc.UTXO, target uint64, outputCount int) ([]woc.UTXO, uint64, uint64, error) {
	ordered := make([]woc.UTXO, len(utxos))
	copy(ordered, utxos)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Value != ordered[j].Value {
			return ordered[i].Value > ordered[j].Value
		}
		if ordered[i].TxID != ordered[j].TxID {
			return ordered[i].TxID < ordered[j].TxID
		}
		return ordered[i].Vout < ordered[j].Vout
	})
	chosen := make([]woc.UTXO, 0, len(ordered))
	var total uint64
	for index, utxo := range ordered {
		chosen = append(chosen, utxo)
		total += utxo.Value
		withChange := w.estimateFee(len(chosen), outputCount+1)
		if total >= target+withChange+dustThreshold {
			return chosen, withChange, total - target - withChange, nil
		}
		if index == len(ordered)-1 {
			withoutChange := w.estimateFee(len(chosen), outputCount)
			if total >= target+withoutChange {
				return chosen, total - target, 0, nil
			}
		}
	}
	return nil, 0, 0, fmt.Errorf("wallet: insufficient funds: need %d sats plus fee", target)
}

func (w *Wallet) estimateFee(inputs, outputs int) uint64 {
	size := int64(baseTransactionBytes + inputs*p2pkhInputBytes + outputs*p2pkhOutputBytes)
	return uint64((w.feeRate*size + 999) / 1000)
}

func (w *Wallet) recordUnresolved(kind, txid, rawHex string, cause error) {
	if w.unresolvedDir == "" {
		return
	}
	_ = state.Append(w.unresolvedDir, state.Unresolved{
		RecordedAt: time.Now().UTC(),
		Kind:       kind,
		TxID:       txid,
		RawTxHex:   rawHex,
		Reason:     cause.Error(),
	})
}
