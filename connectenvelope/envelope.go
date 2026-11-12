// Package connectenvelope 构造身份凭证与意图签名 CBOR 信封（确定性编码＋紧凑签名）。
//
// 后台业务与跨语言一致性校验共用同一构造，避免两份实现分叉；
// 展示、传输与审批逻辑仍在 internal/connect。
package connectenvelope

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/fxamacker/cbor/v2"
)

// canonicalEnc 为确定性 CBOR（RFC 8949 §4.2.1）：最短编码 + 按字节字典序。
func canonicalEnc(v any) ([]byte, error) {
	opts := cbor.CanonicalEncOptions()
	em, err := opts.EncMode()
	if err != nil {
		return nil, err
	}
	return em.Marshal(v)
}

// SignCompact 对 sha256(envelope) 用钱包私钥做 compact 签名（r||s 各 32 字节）。
func SignCompact(privKey, envelope []byte) ([]byte, error) {
	if len(privKey) != 32 {
		return nil, fmt.Errorf("私钥非法")
	}
	h := sha256.Sum256(envelope)
	priv := secp.PrivKeyFromBytes(privKey)
	sig := ecdsa.Sign(priv, h[:])
	r := sig.R()
	s := sig.S()
	rb := r.Bytes()
	sb := s.Bytes()
	out := make([]byte, 64)
	copy(out[:32], rb[:])
	copy(out[32:], sb[:])
	return out, nil
}

func mustPubBytes(pubHex string) ([]byte, error) {
	b, err := hex.DecodeString(pubHex)
	if err != nil || len(b) != 33 {
		return nil, fmt.Errorf("公钥非法")
	}
	return b, nil
}

// BuildIdentityEnvelope 构造身份凭证 v2 9 项信封：
// [2, requestId, aud, iat, exp, text, pubkeyBytes, claimsProjection, network]。
// claimsProjection 按 claim 名 bytewise 排序；本地无真值来源的 claim 直接省略。
// 当前 CLI 仅暴露空投影（无 key.label 等真值），保持签名可验与语义诚实。
func BuildIdentityEnvelope(requestID, aud string, iat, exp int64, text, pubHex, network string, claims []string) (envelope, sig []byte, resolved map[string]any, err error) {
	pubBytes, err := mustPubBytes(pubHex)
	if err != nil {
		return nil, nil, nil, err
	}
	_ = claims
	projection := make([]any, 0)
	// 按 bytewise 排序（空集保持确定性）。
	sort.Slice(projection, func(i, j int) bool { return false })
	input := []any{
		2, requestID, aud, iat, exp, text, pubBytes, projection, network,
	}
	envelope, err = canonicalEnc(input)
	if err != nil {
		return nil, nil, nil, err
	}
	resolved = map[string]any{}
	return envelope, nil, resolved, nil
}

// BuildIdentitySigned 构造并签名身份信封，返回 envelope 与 compact 签名。
func BuildIdentitySigned(privKey []byte, requestID, aud string, iat, exp int64, text, pubHex, network string, claims []string) (envelope, sig []byte, resolved map[string]any, err error) {
	envelope, _, resolved, err = BuildIdentityEnvelope(requestID, aud, iat, exp, text, pubHex, network, claims)
	if err != nil {
		return nil, nil, nil, err
	}
	sig, err = SignCompact(privKey, envelope)
	if err != nil {
		return nil, nil, nil, err
	}
	return envelope, sig, resolved, nil
}

// BuildIntentSigned 构造并签名意图 v2 10 项信封：
// [2, requestId, aud, iat, exp, text, contentType, contentSha256, pubkeyBytes, network]。
func BuildIntentSigned(privKey []byte, requestID, aud string, iat, exp int64, text, contentType string, content []byte, pubHex, network string) (envelope, sig []byte, err error) {
	pubBytes, err := mustPubBytes(pubHex)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(content)
	input := []any{
		2, requestID, aud, iat, exp, text, contentType, sum[:], pubBytes, network,
	}
	envelope, err = canonicalEnc(input)
	if err != nil {
		return nil, nil, err
	}
	sig, err = SignCompact(privKey, envelope)
	if err != nil {
		return nil, nil, err
	}
	return envelope, sig, nil
}
