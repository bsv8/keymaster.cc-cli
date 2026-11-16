// Package walletcrypto owns wallet secret derivation.
package walletcrypto

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"golang.org/x/crypto/hkdf"
	"strings"
)

// AppCipherContext 与 TS 侧 APP_CIPHER_CONTEXT 一致。
const AppCipherContext = "keymaster.connect.cipher.v1"

// DeriveAppCipherKey 按 HKDF-SHA256 从钱包秘密材料派生 App 级 cipher 密钥，
// 绑定钱包公钥、网络和已验证 App 公钥；origin、版本与 indexseed 不进入派生。
func DeriveAppCipherKey(privKey []byte, walletHex, network, appHex string) ([]byte, error) {
	if _, err := ValidateAppPublicKeyHex(appHex); err != nil {
		return nil, err
	}
	if len(privKey) != 32 {
		return nil, fmt.Errorf("私钥非法")
	}
	if len(walletHex) != 66 {
		return nil, fmt.Errorf("钱包公钥非法")
	}
	if _, err := hex.DecodeString(walletHex); err != nil {
		return nil, fmt.Errorf("钱包公钥非法")
	}
	raw, _ := hex.DecodeString(strings.ToLower(walletHex))
	if _, err := secp.ParsePubKey(raw); err != nil {
		return nil, fmt.Errorf("钱包公钥不在曲线上")
	}
	if network != "main" && network != "test" {
		return nil, fmt.Errorf("network 非法")
	}
	info := strings.ToLower(walletHex) + "\x00" + network + "\x00" + appHex
	r := hkdf.New(sha256.New, privKey, []byte(AppCipherContext), []byte(info))
	key := make([]byte, 32)
	if _, err := r.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// ValidateAppPublicKeyHex gates wallet App bindings; origin is never an App identity.
func ValidateAppPublicKeyHex(appHex string) (string, error) {
	if len(appHex) != 66 || appHex != strings.ToLower(appHex) || (appHex[:2] != "02" && appHex[:2] != "03") {
		return "", fmt.Errorf("App 公钥非法")
	}
	appRaw, err := hex.DecodeString(appHex)
	if err != nil {
		return "", err
	}
	if _, err := secp.ParsePubKey(appRaw); err != nil {
		return "", err
	}
	return appHex, nil
}
