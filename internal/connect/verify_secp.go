package connect

import (
	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

func verifyCompact(pub, digest, sig []byte) bool {
	pk, err := secp.ParsePubKey(pub)
	if err != nil {
		return false
	}
	if len(sig) != 64 || len(digest) != 32 {
		return false
	}
	var r, s secp.ModNScalar
	r.SetBytes((*[32]byte)(sig[:32]))
	s.SetBytes((*[32]byte)(sig[32:]))
	signature := ecdsa.NewSignature(&r, &s)
	return signature.Verify(digest, pk)
}
