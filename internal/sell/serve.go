// Seller session loop: one BitFS conversation over an exact-artifact stream.
package sell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/bsv8/BitFSProtocol/content"
	bitfsdebug "github.com/bsv8/BitFSProtocol/debug"
	"github.com/bsv8/BitFSProtocol/wire"
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
func (s *Seller) Serve(ctx context.Context, sessionID string, session Session) (failure error) {
	started := time.Now()
	bitfsdebug.Log("keymaster.cc-cli", "seller.session.start", func() any { return map[string]any{"sessionId": sessionID} })
	defer func() {
		bitfsdebug.Log("keymaster.cc-cli", "seller.session.finish", func() any {
			return map[string]any{"sessionId": sessionID, "elapsedMs": float64(time.Since(started).Microseconds()) / 1000, "error": failure}
		})
	}()
	if session == nil {
		return errors.New("sell: session is required")
	}
	session = &debugSession{Session: session, sessionID: sessionID}
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
		recordID, err := s.poolRecordForArtifact(sessionID, artifact)
		if err != nil {
			return err
		}
		switch artifact.Kind() {
		case wire.RefundPresignRequest:
			kind3, err := s.Presign(ctx, recordID, artifact.Bytes())
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
			if err := s.VerifyFunding(ctx, recordID, artifact.Bytes()); err != nil {
				return err
			}
		case wire.ContentRequest:
			kind6, err := s.PrepareDelivery(ctx, recordID, artifact.Bytes())
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
			if _, err := s.CompletePayment(ctx, recordID, artifact.Bytes()); err != nil {
				return err
			}
		case wire.PoolCloseRequest:
			kind13, err := s.CompleteClose(ctx, recordID, artifact.Bytes())
			if err != nil {
				var pending *CloseBroadcastPendingError
				if !errors.As(err, &pending) {
					return err
				}
				bitfsdebug.Log("keymaster.cc-cli", "seller.close.pending", func() any {
					return map[string]any{"sessionId": recordID, "txid": pending.TxID, "error": pending.Cause, "settled": false}
				})
			}
			response, err := wire.ParseAs(wire.PoolCloseResponse, kind13)
			if err != nil {
				return err
			}
			if err := session.Write(response); err != nil {
				return err
			}
			// 一个池结清不关闭连接；后续报文继续按自己的池引用路由。
		default:
			return fmt.Errorf("sell: unexpected artifact kind %d", artifact.Kind())
		}
	}
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
