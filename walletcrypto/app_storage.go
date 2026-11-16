package walletcrypto

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"strings"
)

// AppStorageRoot is the wallet namespace bound to a verified App public key.
func AppStorageRoot(network, walletHex, appHex string) (string, error) {
	if _, err := ValidateAppPublicKeyHex(appHex); err != nil {
		return "", err
	}
	if network != "main" && network != "test" {
		return "", fmt.Errorf("invalid network")
	}
	raw, err := hex.DecodeString(walletHex)
	if err != nil || len(raw) != 33 {
		return "", fmt.Errorf("invalid wallet public key")
	}
	if _, err = secp.ParsePubKey(raw); err != nil {
		return "", err
	}
	return network + "/" + strings.ToLower(walletHex) + "/" + appHex + "/files/", nil
}

func AppStorageScope(root, path string) string {
	sum := sha256.Sum256([]byte(root + path))
	return "storage.encrypted-file.v1|" + hex.EncodeToString(sum[:])
}
