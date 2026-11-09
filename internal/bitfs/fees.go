package bitfs

import (
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv8/BitFSProtocol/protocol"
	mp "github.com/bsv8/MultisigPool/v4/pkg"
)

// PoolSettlementFee uses the canonical SDK opening-state constructor, as the
// browser does. The unsigned probe is never signed, persisted or broadcast.
func PoolSettlementFee(buyer, seller, arbiter protocol.PublicKey, rate protocol.SatoshisPerKilobyte) (uint64, error) {
	if rate < 1 {
		return 0, fmt.Errorf("bitfs: positive miner fee rate required")
	}
	b, err := ec.PublicKeyFromBytes(buyer[:])
	if err != nil {
		return 0, err
	}
	s, err := ec.PublicKeyFromBytes(seller[:])
	if err != nil {
		return 0, err
	}
	a, err := ec.PublicKeyFromBytes(arbiter[:])
	if err != nil {
		return 0, err
	}
	const amount uint64 = 1_000_000_000_000
	id := make([]byte, 32)
	id[0] = 1
	state, err := mp.BuildArbitratedPoolOpeningState(id, 0, amount, mp.ArbitratedPoolRoles{Buyer: b, Seller: s, Arbiter: a}, 900_000, mp.FeeSatPerKB(rate))
	if err != nil {
		return 0, err
	}
	outputs := state.TotalOutputSatoshis()
	if outputs > amount {
		return 0, fmt.Errorf("bitfs: invalid settlement fee probe")
	}
	return amount - outputs, nil
}
