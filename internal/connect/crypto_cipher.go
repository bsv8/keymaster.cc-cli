package connect

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/fxamacker/cbor/v2"
	"golang.org/x/crypto/hkdf"
)

// DeriveCipherKey 按 HKDF-SHA256 从钱包秘密材料派生 cipher 密钥，
// 绑定钱包公钥、网络、来源与 App，避免公开信息可算出密钥。
//
//	IKM  = 32 字节钱包私钥
//	salt = "keymaster.connect.cipher.v1"
//	info = pubkeyLower + "\0" + network + "\0" + origin + "\0" + appPubkey
//
// 与公开信息直接做 HMAC 的做法不同：仅持有公钥、网络与来源的观察者无法
// 推导出密钥，密文因此具有真正的保密性。派生细节见 parity 验密向量。
func DeriveCipherKey(privKey []byte, pubHex, network, origin, appPubkey string) ([]byte, error) {
	if len(privKey) != 32 {
		return nil, fmt.Errorf("私钥非法")
	}
	if _, err := hex.DecodeString(pubHex); err != nil || len(pubHex) != 66 {
		return nil, fmt.Errorf("公钥非法")
	}
	info := strings.ToLower(pubHex) + "\x00" + network + "\x00" + origin + "\x00" + appPubkey
	r := hkdf.New(sha256.New, privKey, []byte("keymaster.connect.cipher.v1"), []byte(info))
	key := make([]byte, 32)
	if _, err := r.Read(key); err != nil {
		return nil, err
	}
	return key, nil
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
