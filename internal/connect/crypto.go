package connect

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/crypto/hkdf"
)

const (
	encFormat = "keymaster.storage.encrypted-file"
	encMIME   = "application/vnd.keymaster.encrypted-file+json"
)

// ScopeFor 构造路径绑定的加密 scope：storage.encrypted-file.v1|<sha256(root+path)>。
// root 为 <network>/<wallet>/<app>/files/，与 cc 的 connectS3Root 一致。
func ScopeFor(network, wallet, app, path string) (string, error) {
	clean := strings.TrimPrefix(strings.TrimSpace(path), "/")
	if clean == "" {
		return "", fmt.Errorf("路径非法")
	}
	root := network + "/" + strings.ToLower(wallet) + "/" + app + "/files/"
	sum := sha256.Sum256([]byte(root + clean))
	return "storage.encrypted-file.v1|" + hex.EncodeToString(sum[:]), nil
}

func deriveKey(privKey []byte, pubHex, scope string) ([]byte, error) {
	if len(privKey) != 32 {
		return nil, fmt.Errorf("私钥长度非法")
	}
	info := strings.ToLower(pubHex) + "\x00" + scope
	r := hkdf.New(sha256.New, privKey, []byte("keymaster.vault.local-secret.v3"), []byte(info))
	key := make([]byte, 32)
	if _, err := r.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

func aadFor(scope string, salt []byte) []byte {
	prefix := []byte("keymaster:local-secret:v3|" + scope)
	out := make([]byte, 0, len(prefix)+1+len(salt))
	out = append(out, prefix...)
	out = append(out, 0)
	out = append(out, salt...)
	return out
}

// Seal 按 cc 格式打包并加密：4B 元数据长度 + 元数据 JSON + 明文。
func Seal(privKey []byte, pubHex, scope string, plaintext []byte, contentType string) (envelope []byte, mime string, err error) {
	if len(plaintext) > 16<<20 {
		return nil, "", fmt.Errorf("对象过大")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	meta, _ := json.Marshal(map[string]string{"contentType": contentType})
	if len(meta) > 4096 {
		return nil, "", fmt.Errorf("元数据过大")
	}
	packed := make([]byte, 4+len(meta)+len(plaintext))
	packed[0] = byte(len(meta) >> 24)
	packed[1] = byte(len(meta) >> 16)
	packed[2] = byte(len(meta) >> 8)
	packed[3] = byte(len(meta))
	copy(packed[4:], meta)
	copy(packed[4+len(meta):], plaintext)
	key, err := deriveKey(privKey, pubHex, scope)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		for i := range key {
			key[i] = 0
		}
	}()
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, "", err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", err
	}
	ct := gcm.Seal(nil, nonce, packed, aadFor(scope, salt))
	for i := range packed {
		packed[i] = 0
	}
	sealed := map[string]any{"version": 3, "keySource": "active-key-hkdf-v1", "saltHex": hex.EncodeToString(salt), "nonceHex": hex.EncodeToString(nonce), "ciphertextHex": hex.EncodeToString(ct)}
	env, _ := json.Marshal(map[string]any{"format": encFormat, "version": 1, "sealed": sealed})
	return env, encMIME, nil
}

// Open 按 cc 格式校验并解密，仅同一钱包可解密。
func Open(privKey []byte, pubHex, scope string, envelope []byte) (plaintext []byte, contentType string, err error) {
	var env struct {
		Format  string `json:"format"`
		Version int    `json:"version"`
		Sealed  struct {
			Version       int    `json:"version"`
			KeySource     string `json:"keySource"`
			SaltHex       string `json:"saltHex"`
			NonceHex      string `json:"nonceHex"`
			CiphertextHex string `json:"ciphertextHex"`
		} `json:"sealed"`
	}
	if err := json.Unmarshal(envelope, &env); err != nil {
		return nil, "", fmt.Errorf("加密对象无法打开")
	}
	if env.Format != encFormat || env.Version != 1 || env.Sealed.Version != 3 || env.Sealed.KeySource != "active-key-hkdf-v1" {
		return nil, "", fmt.Errorf("加密对象无法打开")
	}
	salt, err1 := hex.DecodeString(env.Sealed.SaltHex)
	nonce, err2 := hex.DecodeString(env.Sealed.NonceHex)
	ct, err3 := hex.DecodeString(env.Sealed.CiphertextHex)
	if err1 != nil || err2 != nil || err3 != nil || len(salt) != 16 || len(nonce) != 12 || len(ct) < 32 {
		return nil, "", fmt.Errorf("加密对象无法打开")
	}
	key, err := deriveKey(privKey, pubHex, scope)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		for i := range key {
			key[i] = 0
		}
	}()
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, "", fmt.Errorf("加密对象无法打开")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", fmt.Errorf("加密对象无法打开")
	}
	packed, err := gcm.Open(nil, nonce, ct, aadFor(scope, salt))
	if err != nil {
		return nil, "", fmt.Errorf("加密对象无法打开")
	}
	defer func() {
		for i := range packed {
			packed[i] = 0
		}
	}()
	if len(packed) < 4 {
		return nil, "", fmt.Errorf("加密对象无法打开")
	}
	mlen := int(packed[0])<<24 | int(packed[1])<<16 | int(packed[2])<<8 | int(packed[3])
	if mlen < 0 || mlen > 4096 || 4+mlen > len(packed) {
		return nil, "", fmt.Errorf("加密对象无法打开")
	}
	var meta struct {
		ContentType string `json:"contentType"`
	}
	if err := json.Unmarshal(packed[4:4+mlen], &meta); err != nil || meta.ContentType == "" {
		return nil, "", fmt.Errorf("加密对象无法打开")
	}
	plain := append([]byte(nil), packed[4+mlen:]...)
	if len(plain) > 16<<20 {
		return nil, "", fmt.Errorf("加密对象无法打开")
	}
	if !bytes.Contains([]byte(meta.ContentType), []byte{}) {
		_ = plain
	}
	return plain, meta.ContentType, nil
}
