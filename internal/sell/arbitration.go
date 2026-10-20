// Seller arbitration: exact Kind 8 claims for delivered batches and exact
// Kind 9 completion. Claims are persisted by claim id before they leave the
// process.
package sell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bsv8/go-bitfs/seller"
)

// ArbitrationClaim is the persisted exact Kind 8.
type ArbitrationClaim struct {
	SessionID       string
	AuthorizationID string
	ClaimID         string
	Kind8           []byte
	Replayed        bool
}

type claimRecord struct {
	AuthorizationID string `json:"authorizationId"`
	ClaimID         string `json:"claimId"`
}

// PrepareArbitrationClaim signs exact Kind 8 for a batch the seller already
// delivered. A previously signed claim for the same authorization is reused.
func (s *Seller) PrepareArbitrationClaim(ctx context.Context, sessionID, authorizationID string) (*ArbitrationClaim, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if err := validSessionID(sessionID); err != nil {
		return nil, err
	}
	recordPath := sessionPath(sessionID, deliveriesDir+"/"+authorizationID+"/record.json")
	recordRaw, err := s.Sessions.Load(recordPath)
	if err != nil {
		return nil, fmt.Errorf("sell: no saved delivery for arbitration: %w", err)
	}
	var record deliveryRecord
	if err := json.Unmarshal(recordRaw, &record); err != nil {
		return nil, fmt.Errorf("sell: delivery record is corrupt: %w", err)
	}
	claimPath := sessionPath(sessionID, deliveriesDir+"/"+authorizationID+"/kind8.bin")
	claimIDPath := sessionPath(sessionID, deliveriesDir+"/"+authorizationID+"/claim.json")
	if saved, err := s.Sessions.Load(claimPath); err == nil {
		claimIDRaw, err := s.Sessions.Load(claimIDPath)
		if err != nil {
			return nil, err
		}
		var stored claimRecord
		if err := json.Unmarshal(claimIDRaw, &stored); err != nil {
			return nil, err
		}
		return &ArbitrationClaim{SessionID: sessionID, AuthorizationID: authorizationID, ClaimID: stored.ClaimID, Kind8: saved, Replayed: true}, nil
	}
	kind5, err := s.Sessions.Load(record.Kind5Path)
	if err != nil {
		return nil, err
	}
	kind6, err := s.Sessions.Load(record.Kind6Path)
	if err != nil {
		return nil, err
	}
	poolEvidence, err := s.loadPool(sessionID)
	if err != nil {
		return nil, err
	}
	artifact, claimID, err := seller.PrepareArbitration(ctx, s.Facts(), seller.PrepareArbitrationInput{
		Pool: *poolEvidence, RequestRaw: kind5, DeliveryRaw: kind6,
	}, s.Signer)
	if err != nil {
		return nil, fmt.Errorf("sell: prepare arbitration: %w", err)
	}
	kind8 := artifact.Bytes()
	encoded, err := json.Marshal(claimRecord{AuthorizationID: authorizationID, ClaimID: claimID.String()})
	if err != nil {
		return nil, err
	}
	if err := s.Sessions.SaveExact(claimPath, kind8); err != nil {
		return nil, err
	}
	if err := s.Sessions.SaveExact(claimIDPath, encoded); err != nil {
		return nil, err
	}
	return &ArbitrationClaim{SessionID: sessionID, AuthorizationID: authorizationID, ClaimID: claimID.String(), Kind8: kind8}, nil
}

// CompleteArbitratedPayment completes the seller's arbitrated payout from
// exact Kind 8/9. The complete transaction is persisted; broadcasting is the
// application's decision.
func (s *Seller) CompleteArbitratedPayment(ctx context.Context, sessionID string, kind8, kind9 []byte) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if err := validSessionID(sessionID); err != nil {
		return nil, err
	}
	if len(kind8) == 0 || len(kind9) == 0 {
		return nil, errors.New("sell: kind8 and kind9 are required")
	}
	completeRaw, err := seller.CompleteArbitratedPayment(ctx, s.Facts(), seller.CompleteArbitratedPaymentInput{
		RequestRaw:  kind8,
		ResponseRaw: kind9,
	}, s.Signer)
	if err != nil {
		return nil, fmt.Errorf("sell: complete arbitrated payment: %w", err)
	}
	if err := s.Sessions.Save(sessionPath(sessionID, "arbitrated-payment.bin"), completeRaw); err != nil {
		return nil, err
	}
	return completeRaw, nil
}
