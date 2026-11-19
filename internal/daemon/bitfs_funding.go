package daemon

import (
	"context"
	"errors"
	"sync"

	"github.com/bsv-blockchain/go-sdk/script"
	bitfswallet "github.com/bsv8/keymaster.cc-bitfs/wallet"
	bitfswoc "github.com/bsv8/keymaster.cc-bitfs/woc"
	"github.com/bsv8/keymaster.cc-cli/internal/wallet"
	"github.com/bsv8/keymaster.cc-cli/internal/woc"
)

// bitfsFunding keeps the wallet's exact prepared transaction and allocation
// binding in the host wallet while exposing its protocol data to BitFS.
type bitfsFunding struct {
	wallet *wallet.Wallet
	spends sync.Map
}

func (p *bitfsFunding) BuildWithFeeRate(ctx context.Context, outputs []bitfswallet.Output, feeRate int64) (*bitfswallet.SpendResult, error) {
	spend, err := p.wallet.BuildWithFeeRate(ctx, walletOutputs(outputs), feeRate)
	return p.remember(spend, err)
}

func (p *bitfsFunding) LockingScript() *script.Script {
	return p.wallet.LockingScript()
}

func (p *bitfsFunding) PrepareProtocol(ctx context.Context, kind string, in bitfswallet.ProtocolRequest) (*bitfswallet.SpendResult, error) {
	proofs := make(map[string]wallet.ProtocolProof, len(in.Proofs))
	for outpoint, proof := range in.Proofs {
		proofs[outpoint] = wallet.ProtocolProof(proof)
	}
	spend, err := p.wallet.PrepareProtocol(ctx, kind, wallet.ProtocolRequest{
		Business: in.Business, FeeRate: in.FeeRate, Declared: walletOutputs(in.Declared),
		SignedHex: in.SignedHex, Proofs: proofs, Binding: wallet.ProtocolBinding(in.Binding),
		FeeCapSats: in.FeeCapSats, FeePolicy: in.FeePolicy,
	})
	return p.remember(spend, err)
}

func (p *bitfsFunding) remember(spend *wallet.SpendResult, err error) (*bitfswallet.SpendResult, error) {
	if err != nil {
		return nil, err
	}
	result := &bitfswallet.SpendResult{
		TxID: spend.TxID, RawHex: spend.RawHex, Transaction: spend.Transaction,
		Fee: spend.Fee, Change: spend.Change, Address: spend.Address, AllocationID: spend.AllocationID,
	}
	for _, input := range spend.Inputs {
		result.Inputs = append(result.Inputs, bitfswoc.UTXO(input))
	}
	p.spends.Store(result, spend)
	return result, nil
}

func (p *bitfsFunding) Release(spend *bitfswallet.SpendResult) error {
	native, ok := p.spends.LoadAndDelete(spend)
	if !ok {
		return nil
	}
	return p.wallet.Release(native.(*wallet.SpendResult))
}

func (p *bitfsFunding) BroadcastPrepared(ctx context.Context, kind string, spend *bitfswallet.SpendResult) (string, error) {
	native, ok := p.spends.LoadAndDelete(spend)
	if !ok {
		return "", errors.New("BitFS 交易未由此钱包准备")
	}
	txid, err := p.wallet.BroadcastPrepared(ctx, kind, native.(*wallet.SpendResult))
	var receipt *woc.BroadcastError
	if errors.As(err, &receipt) {
		return txid, errors.Join(err, &bitfswoc.BroadcastError{Receipt: bitfswoc.BroadcastReceipt(receipt.Receipt)})
	}
	return txid, err
}

func walletOutputs(outputs []bitfswallet.Output) []wallet.Output {
	converted := make([]wallet.Output, len(outputs))
	for index, output := range outputs {
		converted[index] = wallet.Output(output)
	}
	return converted
}

type bitfsCloseChain struct{ *woc.Client }

func (c bitfsCloseChain) TransactionRaw(ctx context.Context, txid string) ([]byte, error) {
	raw, err := c.Client.TransactionRaw(ctx, txid)
	if errors.Is(err, woc.ErrNotFound) {
		return nil, bitfswoc.ErrNotFound
	}
	return raw, err
}
