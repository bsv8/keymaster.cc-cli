// Package wallet implements the KeyHold wallet envelope used by km.
//
// The inner document is the upstream KeyHold v1 format handled by the official
// Go SDK. The envelope adds the network binding used by Keymaster wallets:
// HMAC-SHA256(private key, "keymaster.network-identity.v1:<network>:<publicKeyHex>").
package wallet

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	keyhold "github.com/bsv8/KeyHold/go/v2"
)

const (
	// Format is the wallet envelope discriminator.
	Format = "keymaster.network-keyhold"
	// Version is the supported envelope version.
	Version = 1
	// BackendSoftware marks software wallet documents.
	BackendSoftware = "software"
)

var (
	networkPattern     = regexp.MustCompile(`^(main|test)$`)
	authenticationExpr = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// NetworkKeyHold is the wallet document stored in config.json.
type NetworkKeyHold struct {
	Format                   string           `json:"format"`
	Version                  int              `json:"version"`
	Backend                  string           `json:"backend"`
	BsvNetwork               string           `json:"bsvNetwork"`
	NetworkAuthenticationHex string           `json:"networkAuthenticationHex"`
	KeyHold                  keyhold.Document `json:"keyHold"`
	// present tracks whether the optional backend field was on the wire.
	present bool
}

// Create encrypts privateKey with password and returns a network-bound wallet
// document. iterations is the optional PBKDF2 override; zero uses the SDK
// recommended value.
func Create(privateKey []byte, network, label, password string, iterations int64) (*NetworkKeyHold, error) {
	if !networkPattern.MatchString(network) {
		return nil, fmt.Errorf("钱包网络无效: %q", network)
	}
	parameters := keyhold.RecommendedParameters()
	if iterations > 0 {
		parameters.KeyDerivation.Iterations = iterations
	}
	raw, err := keyhold.ExportPrivateKey(keyhold.PrivateKeyExportInput{
		PrivateKey: privateKey,
		Password:   password,
		Label:      label,
		Parameters: parameters,
	})
	if err != nil {
		return nil, fmt.Errorf("创建 KeyHold 失败: %w", err)
	}
	document, err := keyhold.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("解析新建 KeyHold 失败: %w", err)
	}
	return &NetworkKeyHold{
		Format:                   Format,
		Version:                  Version,
		Backend:                  BackendSoftware,
		BsvNetwork:               network,
		NetworkAuthenticationHex: Authentication(privateKey, network, document.PublicKeyHex),
		KeyHold:                  document,
		present:                  true,
	}, nil
}

// Parse strictly validates an envelope document. Mirrors the TypeScript
// parser: exact key set, optional software backend, network and hex rules.
func Parse(raw []byte) (*NetworkKeyHold, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("网络钱包文档不是有效 JSON: %w", err)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	joined := strings.Join(keys, ",")
	withBackend := "backend,bsvNetwork,format,keyHold,networkAuthenticationHex,version"
	withoutBackend := "bsvNetwork,format,keyHold,networkAuthenticationHex,version"
	if joined != withBackend && joined != withoutBackend {
		return nil, fmt.Errorf("网络钱包字段集合无效: %s", joined)
	}
	document := &NetworkKeyHold{}
	if err := json.Unmarshal(raw, document); err != nil {
		return nil, fmt.Errorf("网络钱包字段无效: %w", err)
	}
	if document.Format != Format || document.Version != Version {
		return nil, fmt.Errorf("网络钱包格式不受支持")
	}
	if document.Backend == "" {
		document.Backend = BackendSoftware
	}
	if document.Backend != BackendSoftware {
		return nil, fmt.Errorf("网络钱包后端不受支持: %q", document.Backend)
	}
	if !networkPattern.MatchString(document.BsvNetwork) {
		return nil, fmt.Errorf("钱包网络无效: %q", document.BsvNetwork)
	}
	if !authenticationExpr.MatchString(document.NetworkAuthenticationHex) {
		return nil, fmt.Errorf("钱包网络认证值无效")
	}
	inner, err := keyhold.Parse(fields["keyHold"])
	if err != nil {
		return nil, fmt.Errorf("KeyHold 文档无效: %w", err)
	}
	document.KeyHold = inner
	document.present = true
	return document, nil
}

// Unlock decrypts the key and verifies the network binding. The returned
// private key buffer is owned by the caller.
func (d *NetworkKeyHold) Unlock(password string) ([]byte, string, error) {
	result, err := keyhold.Unlock(d.KeyHold, password)
	if err != nil {
		return nil, "", fmt.Errorf("解锁钱包失败: %w", err)
	}
	expected, err := hex.DecodeString(Authentication(result.PrivateKey, d.BsvNetwork, result.PublicKeyHex))
	if err != nil {
		clear(result.PrivateKey)
		return nil, "", fmt.Errorf("钱包网络认证值无效")
	}
	actual, err := hex.DecodeString(d.NetworkAuthenticationHex)
	if err != nil {
		clear(result.PrivateKey)
		return nil, "", fmt.Errorf("钱包网络认证值无效")
	}
	if !hmac.Equal(expected, actual) || result.PublicKeyHex != d.KeyHold.PublicKeyHex {
		clear(result.PrivateKey)
		return nil, "", fmt.Errorf("钱包网络身份认证失败")
	}
	return result.PrivateKey, result.PublicKeyHex, nil
}

// Authentication computes the network binding value.
func Authentication(privateKey []byte, network, publicKeyHex string) string {
	mac := hmac.New(sha256.New, privateKey)
	_, _ = mac.Write([]byte("keymaster.network-identity.v1:" + network + ":" + publicKeyHex))
	return hex.EncodeToString(mac.Sum(nil))
}
