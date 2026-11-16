package connect

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"

	"github.com/bsv8/keymaster.cc-cli/walletcrypto"
	"github.com/fxamacker/cbor/v2"
)

// DeriveAppCipherKey 按 HKDF-SHA256 从钱包秘密材料派生 App 级 cipher 密钥，
// 绑定钱包公钥、网络和已验证 App 公钥；origin、版本与 indexseed 不进入派生。
// 调用方必须传入建连时已验证的 App 公钥，不得接受业务请求自带的公钥声明。
// 唯一真源为 walletcrypto，内部包仅委托以防分叉。
func DeriveAppCipherKey(privKey []byte, walletHex, network, appHex string) ([]byte, error) {
	return walletcrypto.DeriveAppCipherKey(privKey, walletHex, network, appHex)
}

func encodeCipherInner(contentType string, content []byte) ([]byte, error) {
	if len(contentType) > 1024 {
		return nil, fmt.Errorf("contentType 非法")
	}
	return cbor.Marshal([]any{1, contentType, content})
}

func decodeCipherInner(plain []byte) (string, []byte, error) {
	var parts []any
	if err := cbor.Unmarshal(plain, &parts); err != nil || len(parts) != 3 {
		return "", nil, fmt.Errorf("密文内层格式非法")
	}
	version, ok := parts[0].(uint64)
	if !ok || version != 1 {
		return "", nil, fmt.Errorf("密文内层版本非法")
	}
	contentType, ok := parts[1].(string)
	if !ok {
		return "", nil, fmt.Errorf("密文内层 contentType 非法")
	}
	content, ok := parts[2].([]byte)
	if !ok {
		return "", nil, fmt.Errorf("密文内层内容非法")
	}
	return contentType, content, nil
}

// CipherEncrypt 返回 nonce + cipherbytes（AES-256-GCM，官方 CBOR 内层）。
func CipherEncrypt(siteKey []byte, contentType string, plain []byte) (nonce, cipherbytes []byte, err error) {
	if len(siteKey) != 32 {
		return nil, nil, fmt.Errorf("siteKey 非法")
	}
	inner, err := encodeCipherInner(contentType, plain)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		for i := range inner {
			inner[i] = 0
		}
	}()
	block, err := aes.NewCipher(siteKey)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	cipherbytes = gcm.Seal(nil, nonce, inner, nil)
	return nonce, cipherbytes, nil
}

// CipherDecrypt 统一失败为 Decrypt failed，不细分原因；成功时恢复密文内
// 的 contentType 与正文。
func CipherDecrypt(siteKey, nonce, cipherbytes []byte) (content []byte, contentType string, err error) {
	if len(siteKey) != 32 || len(nonce) != 12 {
		return nil, "", fmt.Errorf("Decrypt failed")
	}
	block, err := aes.NewCipher(siteKey)
	if err != nil {
		return nil, "", fmt.Errorf("Decrypt failed")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", fmt.Errorf("Decrypt failed")
	}
	plain, err := gcm.Open(nil, nonce, cipherbytes, nil)
	if err != nil {
		return nil, "", fmt.Errorf("Decrypt failed")
	}
	defer func() {
		for i := range plain {
			plain[i] = 0
		}
	}()
	contentType, content, err = decodeCipherInner(plain)
	if err != nil {
		return nil, "", fmt.Errorf("Decrypt failed")
	}
	// CBOR 解码结果不得与待清零的缓冲区共享底层数组。
	out := append([]byte(nil), content...)
	return out, contentType, nil
}
