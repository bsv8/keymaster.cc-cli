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
	preparedTxID       string
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
	revoked       map[string]bool
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
	pool, err := manager.NewFundingKernel(manager.Scope{Owner: hex.EncodeToString(key.PubKey().Compressed()), Network: options.Network}, manager.ManagerConfig{DustThresholdSats: "1"})
	if err != nil {
		return nil, err
	}
	return &Wallet{chain: chain, unlocker: unlocker, address: address, lockingScript: locking, lockingHex: locking.String(), feeRate: options.FeeRateSatoshisPerKb, pool: pool, accepted: map[string]bool{}, revoked: map[string]bool{}}, nil
}
func (w *Wallet) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.pool = nil
	w.accepted = nil
	w.revoked = nil
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
		snapshot = append(snapshot, manager.SnapshotUtxo{TxID: u.TxID, Vout: u.Vout, Value: strconv.FormatUint(u.Value, 10), Status: status, Height: u.Height})
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
	declared := []int{}
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
		declared = append(declared, len(o.LockingScript.Bytes()))
	}
	if target == 0 {
		return nil, errors.New("wallet: positive output amount required")
	}
	id := newAllocationID()
	material, err := w.pool.Request(manager.RequestInput{RequestID: id, BusinessAmount: strconv.FormatUint(target, 10), FeeRate: strconv.FormatInt(w.feeRate, 10), OutputScriptLengths: declared})
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*SpendResult, error) { return nil, errors.Join(err, w.pool.Release(id)) }
	tx := transaction.NewTransaction()
	inputs := []woc.UTXO{}
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
	change, _ := manager.ParseAmount(material.ChangeSats)
	if change > 0 {
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: change, LockingScript: w.lockingScript, Change: true})
	}
	if err := tx.Sign(); err != nil {
		return fail(err)
	}
	fee, _ := manager.ParseAmount(material.FeeSats)
	return &SpendResult{TxID: tx.TxID().String(), RawHex: tx.Hex(), Inputs: inputs, Fee: fee, Change: change, Address: w.Address(), AllocationID: id, Transaction: tx, preparedRaw: tx.Hex(), preparedBy: w, preparedAllocation: id, preparedTxID: tx.TxID().String()}, nil
}

// BroadcastPrepared owns all three outcomes. Every success actively injects own
// outputs BEFORE returning; unknown network errors never release the allocation.
// Cancelled preparations stay cancelled: Release records the Wallet-saved
// allocation identity, and any copy of that preparation is rejected here.
// Revocation is keyed by allocation, not TxID: rebuilding the same payment
// yields the same TxID with a new allocation and must remain submittable.
func (w *Wallet) BroadcastPrepared(ctx context.Context, kind string, p *SpendResult) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.live(); err != nil {
		return "", err
	}
	if p == nil {
		return "", errors.New("wallet: prepared transaction required")
	}
	if p.preparedBy != w || p.preparedRaw == "" || p.preparedRaw != p.RawHex || p.preparedAllocation != p.AllocationID || p.preparedTxID == "" || p.preparedTxID != p.TxID {
		return "", errors.New("wallet: transaction was not prepared by this wallet")
	}
	if p.preparedAllocation != "" && w.revoked[p.preparedAllocation] {
		return "", errors.New("wallet: transaction was not prepared by this wallet")
	}
	parsed, err := manager.ParseTx(p.RawHex, p.TxID)
	if err != nil {
		return "", err
	}
	if parsed.CanonicalTxID != p.TxID {
		return "", errors.New("wallet: prepared txid does not match exact bytes")
	}
	if w.accepted[p.preparedTxID] {
		return p.TxID, nil
	}
	txid, err := w.chain.Broadcast(ctx, p.RawHex)
	if err != nil {
		var rejection *woc.BroadcastError
		if errors.As(err, &rejection) && rejection.Receipt.Status == "failed" && p.AllocationID != "" {
			if releaseErr := w.pool.Release(p.AllocationID); releaseErr != nil {
				return "", errors.Join(err, releaseErr)
			}
			w.revoked[p.preparedAllocation] = true
		}
		return "", err
	}
	if !strings.EqualFold(txid, p.TxID) {
		return "", fmt.Errorf("wallet: provider txid mismatch: %s", txid)
	}
	// The caller owns transaction bytes; the pool receives only new outputs.
	// Successful input allocations stay locked and must not be released.
	utxos := []manager.P2pkhUtxo{}
	for vout, output := range parsed.Outputs {
		if output.Script == w.lockingHex && output.Value != 0 {
			utxos = append(utxos, manager.P2pkhUtxo{TxID: p.TxID, Vout: uint32(vout), Value: strconv.FormatUint(output.Value, 10)})
		}
	}
	if len(utxos) > 0 {
		if err := w.pool.InjectP2pkhUtxos(utxos); err != nil {
			return "", err
		}
	}
	w.accepted[p.preparedTxID] = true
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
	return &SpendResult{TxID: tx.TxID().String(), RawHex: req.SignedHex, Inputs: inputs, Fee: fee, Address: w.Address(), Transaction: tx, preparedRaw: req.SignedHex, preparedBy: w, preparedTxID: tx.TxID().String()}, nil
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
	if p.preparedBy != w || p.preparedRaw == "" || p.preparedRaw != p.RawHex || p.preparedAllocation != p.AllocationID || p.preparedTxID == "" || p.preparedTxID != p.TxID {
		return errors.New("wallet: transaction was not prepared by this wallet")
	}
	if err := w.pool.Release(p.AllocationID); err != nil {
		return err
	}
	w.revoked[p.preparedAllocation] = true
	return nil
}

// Process-wide wallet sharing: at most one live Wallet per owner/network/fee
// rate in this process. Separate instances would run independent FundingKernel
// allocation pools, so two concurrent downloads (or a seller plus a buyer)
// for the same key could allocate the same outpoint to two unbroadcast
// transactions. A shared instance serializes all allocation through one pool
// (every method already holds w.mu), so concurrent downloads can never
// double-allocate. The first holder's chain client is kept; WoC clients are
// stateless and equivalent per network.
var (
	sharedMu    sync.Mutex
	sharedByKey = map[string]*sharedWallet{}
)

type sharedWallet struct {
	wallet *Wallet
	refs   int
}

// Shared returns the process-wide wallet for these options and a release
// function. The caller must call release when done; the underlying wallet
// closes when the last holder releases. Different fee rates get different
// instances so one caller's rate never leaks into another's funding.
func Shared(chain Chain, options Options) (*Wallet, func(), error) {
	if len(options.PrivateKey) != 32 {
		return nil, nil, errors.New("wallet: key is required")
	}
	key, _ := ec.PrivateKeyFromBytes(options.PrivateKey)
	if key == nil {
		return nil, nil, errors.New("wallet: invalid private key")
	}
	id := hex.EncodeToString(key.PubKey().Compressed()) + "|" + options.Network + "|" + strconv.FormatInt(options.FeeRateSatoshisPerKb, 10)
	sharedMu.Lock()
	if entry, ok := sharedByKey[id]; ok {
		entry.refs++
		w := entry.wallet
		sharedMu.Unlock()
		released := false
		return w, func() {
			sharedMu.Lock()
			defer sharedMu.Unlock()
			if released {
				return
			}
			released = true
			entry.refs--
			if entry.refs <= 0 {
				delete(sharedByKey, id)
				_ = w.Close()
			}
		}, nil
	}
	w, err := New(chain, options)
	if err != nil {
		sharedMu.Unlock()
		return nil, nil, err
	}
	entry := &sharedWallet{wallet: w, refs: 1}
	sharedByKey[id] = entry
	sharedMu.Unlock()
	released := false
	return w, func() {
		sharedMu.Lock()
		defer sharedMu.Unlock()
		if released {
			return
		}
		released = true
		entry.refs--
		if entry.refs <= 0 {
			delete(sharedByKey, id)
			_ = w.Close()
		}
	}, nil
}
