// Buyer arbitration: exact Kind 10 content retrieval and exact Kind 11
// verification. Retrieval requests are persisted before they leave the
// process, and a network retry replays the saved request instead of minting a
// new nonce.
package bitfs

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/bsv8/go-bitfs/buyer"
)

// RetrievalPreparation is the persisted exact Kind 10.
type RetrievalPreparation struct {
	PoolID          string
	AuthorizationID string
	Kind10          []byte
	Replayed        bool
}

// RetrievalInput carries the exact request/response pair to verify.
type RetrievalInput struct {
	PoolID          string
	AuthorizationID string
	Kind10          []byte
	Kind11          []byte
	Seed            []byte
}

// RetrievalResult is the verified outcome; unavailable is a signed protocol
// result, not a failure.
type RetrievalResult struct {
	Available       bool
	UnavailableCode uint64
	Payloads        [][]byte
}

// PrepareRetrieval builds exact Kind 10 for one recorded authorization and
// persists it. A previously saved request for the same authorization is
// replayed byte for byte.
func (t *BuyerTask) PrepareRetrieval(ctx context.Context, poolID, authorizationID string) (*RetrievalPreparation, error) {
	if t.Signer == nil || t.Store == nil {
		return nil, errors.New("bitfs: store and signer are required")
	}
	state, err := LoadPoolState(t.Store, poolID)
	if err != nil {
		return nil, err
	}
	kind5Path := poolPath(poolID, kindName("kind5", authorizationID))
	kind5, err := t.Store.Load(kind5Path)
	if err != nil {
		return nil, fmt.Errorf("bitfs: recorded kind5 is required: %w", err)
	}
	quoteRaw, err := state.Meta.QuoteRaw()
	if err != nil {
		return nil, err
	}
	requestPath := poolPath(poolID, "retrieval-"+authorizationID+".bin")
	if saved, err := t.Store.Load(requestPath); err == nil {
		return &RetrievalPreparation{PoolID: poolID, AuthorizationID: authorizationID, Kind10: saved, Replayed: true}, nil
	}
	artifact, err := buyer.RequestArbitratedContent(ctx, buyer.RetrievalRequestInput{
		Pool:          state.Evidence,
		Authorization: buyer.BuyerAuthorizationEvidence{RawKind1: quoteRaw, RawKind5: kind5},
	}, t.Signer)
	if err != nil {
		return nil, fmt.Errorf("bitfs: request arbitrated content: %w", err)
	}
	kind10 := artifact.Bytes()
	if err := t.Store.SaveExact(requestPath, kind10); err != nil {
		return nil, err
	}
	return &RetrievalPreparation{PoolID: poolID, AuthorizationID: authorizationID, Kind10: kind10}, nil
}

// CompleteRetrieval verifies exact Kind 11 against the saved request. On the
// available branch the retrieved payloads are staged by content hash exactly
// like a normal delivery.
func (t *BuyerTask) CompleteRetrieval(ctx context.Context, input RetrievalInput) (*RetrievalResult, error) {
	if t.Signer == nil || t.Store == nil {
		return nil, errors.New("bitfs: store and signer are required")
	}
	if len(input.Kind10) == 0 || len(input.Kind11) == 0 {
		return nil, errors.New("bitfs: kind10 and kind11 are required")
	}
	state, err := LoadPoolState(t.Store, input.PoolID)
	if err != nil {
		return nil, err
	}
	kind5, err := t.Store.Load(poolPath(input.PoolID, kindName("kind5", input.AuthorizationID)))
	if err != nil {
		return nil, fmt.Errorf("bitfs: recorded kind5 is required: %w", err)
	}
	quoteRaw, err := state.Meta.QuoteRaw()
	if err != nil {
		return nil, err
	}
	result, err := buyer.VerifyArbitratedContent(ctx, buyer.ArbitratedContentInput{
		Authorization:        buyer.BuyerAuthorizationEvidence{RawKind1: quoteRaw, RawKind5: kind5},
		Pool:                 state.Evidence,
		RetrievalRequestRaw:  input.Kind10,
		RetrievalResponseRaw: input.Kind11,
		Seed:                 input.Seed,
	})
	if err != nil {
		return nil, fmt.Errorf("bitfs: verify arbitrated content: %w", err)
	}
	if !result.Available {
		return &RetrievalResult{Available: false, UnavailableCode: uint64(result.UnavailableReason)}, nil
	}
	hashes, err := contentHashesOfKind5(kind5)
	if err != nil {
		return nil, err
	}
	if len(hashes) != len(result.Payloads) {
		return nil, errors.New("bitfs: retrieved payload count does not match the authorization")
	}
	for index, hash := range hashes {
		if err := t.Store.SaveExact("staging/"+hex.EncodeToString(hash), result.Payloads[index]); err != nil {
			return nil, err
		}
	}
	return &RetrievalResult{Available: true, Payloads: result.Payloads}, nil
}
