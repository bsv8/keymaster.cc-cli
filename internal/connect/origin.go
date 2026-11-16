package connect

import (
	"github.com/bsv8/keymaster.cc-cli/connectorigin"
	"github.com/bsv8/keymaster.cc-cli/walletcrypto"
)

// parseOriginInternal 委托公共 WHATWG 实现；保留内部测试接缝。
func parseOriginInternal(raw string, strict bool) (string, error) {
	if strict {
		return connectorigin.CanonicalOrigin(raw)
	}
	return connectorigin.ParseEntryOrigin(raw)
}

// CanonicalOrigin 校验并规范化可信 origin 输入。
func CanonicalOrigin(raw string) (string, error) {
	return connectorigin.CanonicalOrigin(raw)
}

// ParseEntryOrigin 从应用入口 URL 推导 canonical origin。
func ParseEntryOrigin(raw string) (string, error) {
	return connectorigin.ParseEntryOrigin(raw)
}

// OriginKey 返回 originKey = lowercase hex SHA256(UTF8(canonicalOrigin))。
func OriginKey(canonical string) string {
	return connectorigin.OriginKey(canonical)
}

// StorageRoot 返回 <network>/<walletPublicKeyLower>/<appPublicKeyHex>/files/。
func StorageRoot(network, walletHex, canonical string) (string, error) {
	return walletcrypto.AppStorageRoot(network, walletHex, canonical)
}

// StorageScope 构造路径绑定的加密 scope。
func StorageScope(root, path string) string {
	return walletcrypto.AppStorageScope(root, path)
}
