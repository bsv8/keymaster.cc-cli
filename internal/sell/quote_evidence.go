package sell

import (
	"encoding/hex"
	"errors"
	"os"

	"github.com/bsv8/go-bitfs/content"
	"github.com/bsv8/go-bitfs/wire"
)

// 这里只读取未经验证的路由引用定位候选证据；交付前 SDK 仍全量验证
// 报价、授权签名、角色、池ID、截止时间、序号及真实支付容量。
func (s *Seller) quoteForRequest(sessionID string, kind5 []byte) ([]byte, error) {
	artifact, err := wire.ParseAs(wire.ContentRequest, kind5)
	if err != nil {
		return nil, err
	}
	request, err := wire.DecodeContentRequest(artifact)
	if err != nil {
		return nil, err
	}
	authorization, err := content.DecodePaymentAuthorization(request.PaymentAuthorizationCBOR)
	if err != nil {
		return nil, err
	}
	raw, err := s.Sessions.Load("quotes/" + hex.EncodeToString(authorization.FileQuoteTermsID[:]) + ".bin")
	if errors.Is(err, os.ErrNotExist) {
		// 已存在的旧池没有条款索引时，只保留其原始合同证据，不采用最新报价。
		return s.loadQuote(sessionID)
	}
	return raw, err
}
