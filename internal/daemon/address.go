package daemon

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	primitives "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
)

func deriveAddress(publicKeyHex string, mainnet bool) (string, error) {
	pub, err := primitives.PublicKeyFromString(publicKeyHex)
	if err != nil {
		return "", fmt.Errorf("公钥无效: %w", err)
	}
	addr, err := script.NewAddressFromPublicKey(pub, mainnet)
	if err != nil {
		return "", fmt.Errorf("派生地址失败: %w", err)
	}
	return addr.AddressString, nil
}

func computeAuth(privateKey []byte, network, publicKeyHex string) string {
	mac := hmac.New(sha256.New, privateKey)
	_, _ = mac.Write([]byte("keymaster.network-identity.v1:" + network + ":" + publicKeyHex))
	return hex.EncodeToString(mac.Sum(nil))
}
