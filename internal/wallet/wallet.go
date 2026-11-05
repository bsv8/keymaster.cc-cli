// Package wallet signs and submits transactions using a memory-only UTXO pool.
// Transaction results belong to this caller, not to the allocation component.
package wallet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv8/keymaster.cc-cli/internal/woc"
	manager "github.com/bsv8/keymaster.cc-utxo-manager/utxo"
)

type Chain interface {
	ListUnspent(context.Context, string) ([]woc.UTXO, error)
	Broadcast(context.Context, string) (string, error)
}
type Options struct {
	Network              string
	PrivateKey           []byte
	FeeRateSatoshisPerKb int64
}
type Output struct {
	Satoshis      uint64
	LockingScript *script.Script
}
type SpendResult struct {
	TxID               string
	RawHex             string
	Inputs             []woc.UTXO
	Fee                uint64
	Change             uint64
	Address            string
	AllocationID       string
	Transaction        *transaction.Transaction
	preparedRaw        string
	preparedBy         *Wallet
	preparedAllocation string
}
type Wallet struct {
	mu            sync.Mutex
	chain         Chain
	unlocker      *p2pkh.P2PKH
	address       *script.Address
	lockingScript *script.Script
	lockingHex    string
	feeRate       int64
	pool          *manager.FundingKernel
	revision      int64
	initialized   bool
	closed        bool
	accepted      map[string]bool
}

func New(chain Chain, options Options) (*Wallet, error) {
	if chain == nil {
		return nil, errors.New("wallet: chain port is required")
	}
	if options.Network != "main" && options.Network != "test" {
		return nil, fmt.Errorf("wallet: unsupported network %q", options.Network)
	}
	if len(options.PrivateKey) != 32 || options.FeeRateSatoshisPerKb <= 0 {
		return nil, errors.New("wallet: key and positive fee rate required")
	}
	key, _ := ec.PrivateKeyFromBytes(options.PrivateKey)
	if key == nil {
		return nil, errors.New("wallet: invalid private key")
	}
	address, err := script.NewAddressFromPublicKey(key.PubKey(), options.Network == "main")
	if err != nil {
		return nil, err
	}
	locking, err := p2pkh.Lock(address)
	if err != nil {
		return nil, err
	}
	unlocker, err := p2pkh.Unlock(key, nil)
	if err != nil {
		return nil, err
	}
	pool, err := manager.NewFundingKernel(manager.Scope{Owner: hex.EncodeToString(key.PubKey().Compressed()), Network: options.Network}, manager.ManagerConfig{WalletScriptHex: locking.String(), DustThresholdSats: "1"})
	if err != nil {
		return nil, err
	}
	return &Wallet{chain: chain, unlocker: unlocker, address: address, lockingScript: locking, lockingHex: locking.String(), feeRate: options.FeeRateSatoshisPerKb, pool: pool, accepted: map[string]bool{}}, nil
}
func (w *Wallet) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.pool = nil
	w.accepted = nil
	return nil
}
func (w *Wallet) live() error {
	if w.closed {
		return errors.New("wallet: closed")
	}
	return nil
}
func (w *Wallet) Address() string               { return w.address.AddressString }
func (w *Wallet) LockingScript() *script.Script { return w.lockingScript }
func newAllocationID() string {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	return "w-" + hex.EncodeToString(nonce[:])
}
func (w *Wallet) refreshLocked(ctx context.Context) error {
	rows, err := w.chain.ListUnspent(ctx, w.Address())
	if err != nil {
		return err
	}
	snapshot := make([]manager.SnapshotUtxo, 0, len(rows))
	for _, u := range rows {
		status := "confirmed"
		if u.Height <= 0 {
			status = "unconfirmed"
		}
		snapshot = append(snapshot, manager.SnapshotUtxo{TxID: u.TxID, Vout: u.Vout, Value: strconv.FormatUint(u.Value, 10), ScriptHex: w.lockingHex, Status: status, Height: u.Height})
	}
	if _, err := w.pool.ApplySnapshot(manager.SnapshotInput{Generation: 1, Revision: w.revision + 1, Utxos: snapshot}); err != nil {
		return err
	}
	w.revision++
	w.initialized = true
	return nil
}

// Refresh explicitly updates the chain observation; it is not a submission step.
func (w *Wallet) Refresh(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.live(); err != nil {
		return err
	}
	return w.refreshLocked(ctx)
}
func (w *Wallet) Available(ctx context.Context) (uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.live(); err != nil {
		return 0, err
	}
	if !w.initialized {
		if err := w.refreshLocked(ctx); err != nil {
			return 0, err
		}
	}
	return manager.ParseAmount(w.pool.Funds().AvailableSats)
}
func (w *Wallet) Build(ctx context.Context, outputs []Output) (*SpendResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.live(); err != nil {
		return nil, err
	}
	if !w.initialized {
		if err := w.refreshLocked(ctx); err != nil {
			return nil, err
		}
	}
	declared := []manager.RequestOutput{}
	var target uint64
	for _, o := range outputs {
		if o.LockingScript == nil {
			return nil, errors.New("wallet: output script required")
		}
		var err error
		target, err = manager.AddChecked(target, o.Satoshis)
		if err != nil {
			return nil, err
		}
		declared = append(declared, manager.RequestOutput{ScriptHex: o.LockingScript.String(), Value: strconv.FormatUint(o.Satoshis, 10)})
	}
	if target == 0 {
		return nil, errors.New("wallet: positive output amount required")
	}
	id := newAllocationID()
	material, err := w.pool.Request(manager.RequestInput{RequestID: id, BusinessAmount: strconv.FormatUint(target, 10), FeeRate: strconv.FormatInt(w.feeRate, 10), Outputs: declared})
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*SpendResult, error) { return nil, errors.Join(err, w.pool.Release(id)) }
	tx := transaction.NewTransaction()
	inputs := []woc.UTXO{}
	view := material.Allocation
	for _, row := range material.Inputs {
		value, err := manager.ParseAmount(row.Value)
		if err != nil {
			return fail(err)
		}
		u := woc.UTXO{TxID: row.TxID, Vout: row.Vout, Value: value, Height: row.Height}
		inputs = append(inputs, u)
		if err := tx.AddInputFrom(u.TxID, u.Vout, w.lockingHex, u.Value, w.unlocker); err != nil {
			return fail(err)
		}
	}
	for _, o := range outputs {
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: o.Satoshis, LockingScript: o.LockingScript})
	}
	change, _ := manager.ParseAmount(view.ChangeSats)
	if change > 0 {
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: change, LockingScript: w.lockingScript, Change: true})
	}
	if err := tx.Sign(); err != nil {
		return fail(err)
	}
	fee, _ := manager.ParseAmount(view.FeeSats)
	return &SpendResult{TxID: tx.TxID().String(), RawHex: tx.Hex(), Inputs: inputs, Fee: fee, Change: change, Address: w.Address(), AllocationID: id, Transaction: tx, preparedRaw: tx.Hex(), preparedBy: w, preparedAllocation: id}, nil
}

// BroadcastPrepared owns all three outcomes. Every success actively injects own
// outputs BEFORE returning; unknown network errors never release the allocation.
func (w *Wallet) BroadcastPrepared(ctx context.Context, kind string, p *SpendResult) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.live(); err != nil {
		return "", err
	}
	if p == nil {
		return "", errors.New("wallet: prepared transaction required")
	}
	if p.preparedBy != w || p.preparedRaw == "" || p.preparedRaw != p.RawHex || p.preparedAllocation != p.AllocationID {
		return "", errors.New("wallet: transaction was not prepared by this wallet")
	}
	parsed, err := manager.ParseTx(p.RawHex, p.TxID)
	if err != nil {
		return "", err
	}
	if parsed.CanonicalTxID != p.TxID {
		return "", errors.New("wallet: prepared txid does not match exact bytes")
	}
	if w.accepted[p.TxID] {
		return p.TxID, nil
	}
	if p.AllocationID != "" {
		if err := w.pool.AssertAllocationUsable(p.AllocationID); err != nil {
			return "", err
		}
	}
	txid, err := w.chain.Broadcast(ctx, p.RawHex)
	if err != nil {
		var rejection *woc.BroadcastError
		if errors.As(err, &rejection) && rejection.Receipt.Status == "failed" && p.AllocationID != "" {
			if releaseErr := w.pool.Release(p.AllocationID); releaseErr != nil {
				return "", errors.Join(err, releaseErr)
			}
		}
		return "", err
	}
	if !strings.EqualFold(txid, p.TxID) {
		return "", fmt.Errorf("wallet: provider txid mismatch: %s", txid)
	}
	// The caller owns transaction bytes; the pool receives only UTXO changes.
	accepted := manager.AcceptedTransaction{TxID: p.TxID}
	for _, input := range parsed.Inputs {
		accepted.SpentOutpoints = append(accepted.SpentOutpoints, input.OutpointKey)
	}
	for vout, output := range parsed.Outputs {
		accepted.Outputs = append(accepted.Outputs, manager.AcceptedOutput{Vout: uint32(vout), Value: strconv.FormatUint(output.Value, 10), ScriptHex: output.Script})
	}
	if err := w.pool.AcceptTransaction(accepted); err != nil {
		return "", err
	}
	w.accepted[p.TxID] = true
	w.initialized = true
	return txid, nil
}

type ProtocolProof struct {
	Value     uint64
	ScriptHex string
}
type ProtocolBinding struct {
	Kind   string
	PoolID string
}
type ProtocolRequest struct {
	Business   string
	FeeRate    string
	Declared   []Output
	SignedHex  string
	Proofs     map[string]ProtocolProof
	Binding    ProtocolBinding
	FeeCapSats string
	FeePolicy  string
}

// PrepareProtocol accepts protocol-only exact transactions. Wallet funding must
// be selected and frozen by Request in Build before signing, never retroactively.
// Protocol inputs need no wallet allocation; success still injects own outputs.
func (w *Wallet) PrepareProtocol(ctx context.Context, kind string, req ProtocolRequest) (*SpendResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.live(); err != nil {
		return nil, err
	}
	tx, err := transaction.NewTransactionFromHex(req.SignedHex)
	if err != nil {
		return nil, err
	}
	inputs := []woc.UTXO{}
	var total uint64
	for _, in := range tx.Inputs {
		key := in.SourceTXID.String() + ":" + strconv.FormatUint(uint64(in.SourceTxOutIndex), 10)
		proof, ok := req.Proofs[key]
		if !ok || proof.ScriptHex == "" {
			return nil, fmt.Errorf("wallet: protocol input proof required: %s", key)
		}
		if strings.EqualFold(proof.ScriptHex, w.lockingHex) {
			return nil, errors.New("wallet: wallet input requires prior manager allocation")
		}
		inputs = append(inputs, woc.UTXO{TxID: in.SourceTXID.String(), Vout: in.SourceTxOutIndex, Value: proof.Value})
		total, err = manager.AddChecked(total, proof.Value)
		if err != nil {
			return nil, err
		}
	}
	if total < tx.TotalOutputSatoshis() {
		return nil, errors.New("wallet: protocol outputs exceed inputs")
	}
	fee := total - tx.TotalOutputSatoshis()
	return &SpendResult{TxID: tx.TxID().String(), RawHex: req.SignedHex, Inputs: inputs, Fee: fee, Address: w.Address(), Transaction: tx, preparedRaw: req.SignedHex, preparedBy: w}, nil
}
func (w *Wallet) Spend(ctx context.Context, kind string, outputs []Output) (*SpendResult, error) {
	p, err := w.Build(ctx, outputs)
	if err != nil {
		return nil, err
	}
	if _, err = w.BroadcastPrepared(ctx, kind, p); err != nil {
		return nil, err
	}
	return p, nil
}
func (w *Wallet) Release(p *SpendResult) error {
	if p == nil || p.AllocationID == "" {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.live(); err != nil {
		return err
	}
	return w.pool.Release(p.AllocationID)
}
