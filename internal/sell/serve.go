// Seller session loop: one BitFS conversation over an exact-artifact stream.
package sell

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bsv8/go-bitfs/content"
	"github.com/bsv8/go-bitfs/wire"
)

// Session is the minimal exact-artifact stream the seller needs.
type Session interface {
	Read() (wire.Artifact, error)
	Write(wire.Artifact) error
	Close() error
}

// Serve handles one seller session until the buyer closes it or an error
// occurs. The quote must already be persisted for sessionID (CreateQuote) and
// the session must have been dialed by the seller after hearing a demand.
func (s *Seller) Serve(ctx context.Context, sessionID string, session Session) error {
	if session == nil {
		return errors.New("sell: session is required")
	}
	defer session.Close()
	// The seller always speaks first: its exact Kind 1 quote opens the session.
	quoteRaw, err := s.Sessions.Load(sessionPath(sessionID, sessionQuoteName))
	if err != nil {
		return fmt.Errorf("sell: session quote is missing: %w", err)
	}
	quote, err := wire.ParseAs(wire.FileQuote, quoteRaw)
	if err != nil {
		return err
	}
	if err := session.Write(quote); err != nil {
		return fmt.Errorf("sell: send quote: %w", err)
	}
	for {
		artifact, err := session.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("sell: read artifact: %w", err)
		}
		switch artifact.Kind() {
		case wire.RefundPresignRequest:
			kind3, err := s.Presign(ctx, sessionID, artifact.Bytes())
			if err != nil {
				return err
			}
			response, err := wire.ParseAs(wire.RefundPresignResponse, kind3)
			if err != nil {
				return err
			}
			if err := session.Write(response); err != nil {
				return err
			}
		case wire.FundingTransactionDelivery:
			if err := s.VerifyFunding(ctx, sessionID, artifact.Bytes()); err != nil {
				return err
			}
		case wire.ContentRequest:
			kind6, err := s.PrepareDelivery(ctx, sessionID, artifact.Bytes())
			if err != nil {
				return err
			}
			delivery, err := wire.ParseAs(wire.ContentDelivery, kind6)
			if err != nil {
				return err
			}
			if err := session.Write(delivery); err != nil {
				return err
			}
		case wire.PaymentUpdate:
			if _, err := s.CompletePayment(ctx, sessionID, artifact.Bytes()); err != nil {
				return err
			}
		case wire.PoolCloseRequest:
			kind13, err := s.CompleteClose(ctx, sessionID, artifact.Bytes())
			if err != nil {
				return err
			}
			response, err := wire.ParseAs(wire.PoolCloseResponse, kind13)
			if err != nil {
				return err
			}
			if err := session.Write(response); err != nil {
				return err
			}
			// The close settles the sale. One seller session serves exactly one
			// pool; the buyer requests a fresh quote/session for the next pool.
			// Do not slam the stream shut right after the response: the buyer
			// still processes the close answer.
			done := make(chan struct{})
			go func() {
				_, _ = session.Read()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			return nil
		default:
			return fmt.Errorf("sell: unexpected artifact kind %d", artifact.Kind())
		}
	}
}

// ResumableSession is a still-open seller session that can be served again
// after a transport disconnect.
type ResumableSession struct {
	SessionID   string
	SeedHashHex string
}

// FindResumableSession returns a session for the same buyer and seed whose
// funding is verified and which has no negotiated close yet. It scans the
// persisted session metadata so a restarted seller can resume a purchase.
func (s *Seller) FindResumableSession(buyerPublicKeyHex, seedHashHex string) (*ResumableSession, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	names, err := s.Sessions.List("sessions/")
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if !strings.HasSuffix(name, "/"+sessionMetaName) {
			continue
		}
		sessionID := strings.TrimSuffix(strings.TrimPrefix(name, "sessions/"), "/"+sessionMetaName)
		meta, err := s.loadMeta(sessionID)
		if err != nil || meta.SeedHashHex != seedHashHex {
			continue
		}
		if exists, _ := s.Sessions.Exists(sessionPath(sessionID, sessionKind13Name)); exists {
			continue
		}
		if exists, _ := s.Sessions.Exists(sessionPath(sessionID, sessionFundingRawName)); !exists {
			continue
		}
		quoteRaw, err := s.Sessions.Load(sessionPath(sessionID, sessionQuoteName))
		if err != nil {
			continue
		}
		verified, err := verifySavedQuote(quoteRaw)
		if err != nil || !strings.EqualFold(hex.EncodeToString(verified.BuyerPublicKey()), buyerPublicKeyHex) {
			continue
		}
		return &ResumableSession{SessionID: sessionID, SeedHashHex: meta.SeedHashHex}, nil
	}
	return nil, nil
}

// verifySavedQuote validates a persisted quote as existing contract evidence;
// resume must not be blocked by the quote's wall-clock expiry.
func verifySavedQuote(raw []byte) (*content.VerifiedQuote, error) {
	artifact, err := wire.ParseAs(wire.FileQuote, raw)
	if err != nil {
		return nil, err
	}
	quote, err := wire.DecodeFileQuote(artifact)
	if err != nil {
		return nil, err
	}
	return content.VerifyQuoteEvidence(quote)
}
