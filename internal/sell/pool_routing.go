package sell

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/bsv8/go-bitfs/content"
	"github.com/bsv8/go-bitfs/pool"
	"github.com/bsv8/go-bitfs/wire"
)

// 报价记录属于连接，池记录属于退款模板。连接更换不改写原池证据。
func (s *Seller) loadQuote(sessionID string) ([]byte, error) {
	meta, err := s.loadMeta(sessionID)
	if err != nil {
		return nil, err
	}
	source := sessionID
	if meta.QuoteSessionID != "" {
		source = meta.QuoteSessionID
	}
	return s.Sessions.Load(sessionPath(source, sessionQuoteName))
}

func refundReference(artifact wire.Artifact) (pool.RefundTemplateTxID, error) {
	switch artifact.Kind() {
	case wire.RefundPresignRequest:
		request, err := wire.DecodeRefundPresignRequest(artifact)
		if err != nil {
			return pool.RefundTemplateTxID{}, err
		}
		return pool.DeriveRefundTemplateTxIDFromRequest(request)
	case wire.FundingTransactionDelivery:
		delivery, err := wire.DecodeFundingTransactionDelivery(artifact)
		if err != nil {
			return pool.RefundTemplateTxID{}, err
		}
		return delivery.RefundTemplateTxID, nil
	case wire.ContentRequest:
		request, err := wire.DecodeContentRequest(artifact)
		if err != nil {
			return pool.RefundTemplateTxID{}, err
		}
		authorization, err := content.DecodePaymentAuthorization(request.PaymentAuthorizationCBOR)
		if err != nil {
			return pool.RefundTemplateTxID{}, err
		}
		var id pool.RefundTemplateTxID
		copy(id[:], authorization.RefundTemplateTxID)
		return id, nil
	case wire.PoolCloseRequest:
		request, err := wire.DecodePoolCloseRequest(artifact)
		if err != nil {
			return pool.RefundTemplateTxID{}, err
		}
		return request.RefundTemplateTxID, nil
	default:
		return pool.RefundTemplateTxID{}, fmt.Errorf("sell: artifact kind %d has no pool reference", artifact.Kind())
	}
}

// 新记录直接按退款模板寻址；旧记录只按实际 Kind 2 查找，不按文件或报价猜池。
func (s *Seller) findPoolRecord(id pool.RefundTemplateTxID) (string, error) {
	target := "p-" + hex.EncodeToString(id[:])
	exists, err := s.Sessions.Exists(sessionPath(target, sessionMetaName))
	if err != nil {
		return "", err
	}
	if exists {
		return target, nil
	}
	names, err := s.Sessions.List("sessions/")
	if err != nil {
		return "", err
	}
	found := ""
	for _, name := range names {
		if !strings.HasSuffix(name, "/"+sessionKind2Name) {
			continue
		}
		raw, err := s.Sessions.Load(name)
		if err != nil {
			return "", err
		}
		artifact, err := wire.ParseAs(wire.RefundPresignRequest, raw)
		if err != nil {
			return "", err
		}
		candidate, err := refundReference(artifact)
		if err != nil {
			return "", err
		}
		if candidate != id {
			continue
		}
		record := strings.TrimSuffix(strings.TrimPrefix(name, "sessions/"), "/"+sessionKind2Name)
		if found != "" && found != record {
			return "", errors.New("sell: conflicting persisted records for one pool")
		}
		found = record
	}
	return found, nil
}

func (s *Seller) paymentPoolRecord(artifact wire.Artifact) (string, error) {
	update, err := wire.DecodePaymentUpdate(artifact)
	if err != nil {
		return "", err
	}
	suffix := "/" + deliveriesDir + "/" + hex.EncodeToString(update.PaymentAuthorizationID[:]) + "/kind5.bin"
	names, err := s.Sessions.List("sessions/")
	if err != nil {
		return "", err
	}
	found := ""
	for _, name := range names {
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		record := strings.TrimSuffix(strings.TrimPrefix(name, "sessions/"), suffix)
		if found != "" && found != record {
			return "", errors.New("sell: conflicting persisted authorization owners")
		}
		found = record
	}
	if found == "" {
		return "", errors.New("sell: payment has no saved authorization")
	}
	return found, nil
}

// 只负责外部报文的寻址与连接身份边界；后续角色工作流仍完整验签、验货与验金额。
func (s *Seller) poolRecordForArtifact(connectionID string, artifact wire.Artifact) (string, error) {
	originRaw, err := s.loadQuote(connectionID)
	if err != nil {
		return "", err
	}
	origin, err := verifySavedQuote(originRaw)
	if err != nil {
		return "", err
	}
	var record string
	if artifact.Kind() == wire.PaymentUpdate {
		record, err = s.paymentPoolRecord(artifact)
	} else {
		var id pool.RefundTemplateTxID
		id, err = refundReference(artifact)
		if err != nil {
			return "", err
		}
		record, err = s.findPoolRecord(id)
		if err != nil {
			return "", err
		}
		if record == "" {
			if artifact.Kind() != wire.RefundPresignRequest {
				return "", errors.New("sell: unknown pool reference")
			}
			request, err := wire.DecodeRefundPresignRequest(artifact)
			if err != nil {
				return "", err
			}
			if !bytes.Equal(request.BuyerPublicKey, origin.BuyerPublicKey()) || !bytes.Equal(request.SellerPublicKey, origin.SellerPublicKey()) {
				return "", errors.New("sell: opening roles differ from the connection identity")
			}
			meta, err := s.loadMeta(connectionID)
			if err != nil {
				return "", err
			}
			record = "p-" + hex.EncodeToString(id[:])
			meta.SessionID = record
			meta.QuoteSessionID = connectionID
			if err := s.saveMeta(meta); err != nil {
				return "", err
			}
		}
	}
	if err != nil {
		return "", err
	}
	savedRaw, err := s.loadQuote(record)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("sell: pool quote evidence is missing")
	}
	if err != nil {
		return "", err
	}
	saved, err := verifySavedQuote(savedRaw)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(saved.BuyerPublicKey(), origin.BuyerPublicKey()) || !bytes.Equal(saved.SellerPublicKey(), origin.SellerPublicKey()) {
		return "", errors.New("sell: pool belongs to another connection identity")
	}
	return record, nil
}
