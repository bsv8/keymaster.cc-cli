package daemon

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv8/keymaster.cc-bitfs/seller"
	"github.com/bsv8/keymaster.cc-cli/internal/catalog"
	"github.com/bsv8/keymaster.cc-cli/internal/wallet"
	"github.com/bsv8/keymaster.cc-cli/internal/woc"
)

type bitfsCatalog struct{ *catalog.Catalog }

func (c bitfsCatalog) Preferred(ctx context.Context, hash string) (*seller.CatalogEntry, error) {
	entry, err := c.Catalog.Preferred(ctx, hash)
	if err != nil || entry == nil {
		return nil, err
	}
	return &seller.CatalogEntry{
		Path: entry.Path, RelativePath: entry.RelativePath, Name: entry.Name,
		Size: entry.Size, SeedHashHex: entry.SeedHashHex, BlockCount: entry.BlockCount, Seed: entry.Seed,
	}, nil
}

func (c bitfsCatalog) ReadBlock(ctx context.Context, entry *seller.CatalogEntry, hash []byte) ([]byte, error) {
	return c.Catalog.ReadBlock(ctx, &catalog.Entry{
		Path: entry.Path, RelativePath: entry.RelativePath, Name: entry.Name,
		Size: entry.Size, SeedHashHex: entry.SeedHashHex, BlockCount: entry.BlockCount, Seed: entry.Seed,
	}, hash)
}

func submitBitfsSaleClose(ctx context.Context, funding *wallet.Wallet, input seller.CloseSubmission) error {
	rawHex := hex.EncodeToString(input.Raw)
	sources := [][]byte{input.FundingRaw}
	if len(input.LatestPaymentRaw) != 0 {
		sources = append(sources, input.LatestPaymentRaw)
	}
	proofs, err := wallet.BindInputs(rawHex, sources)
	if err != nil {
		return err
	}
	closeTx, err := transaction.NewTransactionFromBytes(input.Raw)
	if err != nil {
		return err
	}
	selfScript := funding.LockingScript().String()
	var declared []wallet.Output
	for _, output := range closeTx.Outputs {
		if output.LockingScript == nil {
			return errors.New("BitFS 关池输出缺少脚本")
		}
		if !strings.EqualFold(output.LockingScript.String(), selfScript) {
			declared = append(declared, wallet.Output{Satoshis: output.Satoshis, LockingScript: output.LockingScript})
		}
	}
	prepared, err := funding.PrepareProtocol(ctx, "pool-close", wallet.ProtocolRequest{
		Business: "0", FeeRate: strconv.FormatUint(input.MinerFeeRate, 10), Declared: declared,
		SignedHex: rawHex, Proofs: proofs, Binding: wallet.ProtocolBinding{Kind: "close", PoolID: input.SessionID},
	})
	if err != nil {
		return err
	}
	_, err = funding.BroadcastPrepared(ctx, "pool-close", prepared)
	var receipt *woc.BroadcastError
	if errors.As(err, &receipt) && receipt.Receipt.Status == "unknown" {
		return &seller.CloseBroadcastPendingError{TxID: input.TxID, Cause: err}
	}
	return err
}
