package connect

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

const domainV1 = "keymaster-app-identity:v1"

var (
	pubkeyRe = regexp.MustCompile(`^(02|03)[0-9a-f]{64}$`)
	appIDRe  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,61}[a-z0-9])?$`)
	hex64Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// VerifiedIdentity 为已验证 proof 的不可变 snapshot。
type VerifiedIdentity struct {
	Version            int      `json:"version"`
	PublisherPublicKey string   `json:"publisherPublicKeyHex"`
	AppID              string   `json:"appId"`
	AppName            string   `json:"appName"`
	IdentityDigestHex  string   `json:"identityDigestHex"`
	Requirements       []string `json:"requirements"`
}

// ProofV1 为外部 App 身份 proof。
type ProofV1 struct {
	Version            int      `json:"version"`
	PublisherPublicKey string   `json:"publisherPublicKey"`
	App                AppMeta  `json:"app"`
	Requirements       []string `json:"requirements"`
	Signature          string   `json:"signature"`
}

// AppMeta 为 App 元信息。
type AppMeta struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func canonicalJSON(v any) (string, error) {
	switch t := v.(type) {
	case nil, bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		// 标量直接用标准 JSON 编码（RFC 8785 下整数与有限浮点数的
		// 规范形式与 encoding/json 输出一致；NaN/Inf 由 Marshal 报错）。
		b, err := json.Marshal(t)
		return string(b), err
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			s, err := canonicalJSON(e)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, p := range parts {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(p)
		}
		buf.WriteByte(']')
		return buf.String(), nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			s, err := canonicalJSON(t[k])
			if err != nil {
				return "", err
			}
			buf.WriteString(s)
		}
		buf.WriteByte('}')
		return buf.String(), nil
	default:
		// structs 经 JSON 往返转为 map 再规范化。
		raw, err := json.Marshal(t)
		if err != nil {
			return "", err
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", err
		}
		return canonicalJSON(m)
	}
}

func identityDigest(p ProofV1) ([]byte, error) {
	payload := map[string]any{
		"version":            1,
		"publisherPublicKey": p.PublisherPublicKey,
		"app": map[string]any{
			"id":          p.App.ID,
			"name":        p.App.Name,
			"description": p.App.Description,
		},
		"requirements": toAnySlice(p.Requirements),
	}
	canon, err := canonicalJSON(payload)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(domainV1))
	h.Write([]byte{0})
	h.Write([]byte(canon))
	return h.Sum(nil), nil
}

func toAnySlice(s []string) []any {
	out := make([]any, 0, len(s))
	for _, v := range s {
		out = append(out, v)
	}
	return out
}

// VerifyProof 验证外部签名 proof，返回可持久化的 digest snapshot。
func VerifyProof(raw json.RawMessage) (VerifiedIdentity, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return VerifiedIdentity{}, fmt.Errorf("appIdentity 必须为对象")
	}
	// 精确字段检查。
	want := []string{"version", "publisherPublicKey", "app", "requirements", "signature"}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sort.Strings(want)
	if len(keys) != len(want) {
		return VerifiedIdentity{}, fmt.Errorf("appIdentity 字段非法")
	}
	for i := range keys {
		if keys[i] != want[i] {
			return VerifiedIdentity{}, fmt.Errorf("appIdentity 字段非法")
		}
	}
	var p ProofV1
	if err := json.Unmarshal(raw, &p); err != nil {
		return VerifiedIdentity{}, fmt.Errorf("appIdentity 非法")
	}
	if p.Version != 1 || !pubkeyRe.MatchString(p.PublisherPublicKey) {
		return VerifiedIdentity{}, fmt.Errorf("publisherPublicKey 非法")
	}
	if !appIDRe.MatchString(p.App.ID) {
		return VerifiedIdentity{}, fmt.Errorf("app.id 非法")
	}
	if len(p.App.Name) == 0 || len(p.App.Name) > 120 {
		return VerifiedIdentity{}, fmt.Errorf("app.name 非法")
	}
	if len(p.App.Description) > 500 {
		return VerifiedIdentity{}, fmt.Errorf("app.description 非法")
	}
	allowed := map[string]bool{"private-key": true, "storage": true}
	seen := map[string]bool{}
	prev := ""
	for _, r := range p.Requirements {
		if !allowed[r] || seen[r] {
			return VerifiedIdentity{}, fmt.Errorf("requirements 非法")
		}
		seen[r] = true
		if prev != "" && prev >= r {
			return VerifiedIdentity{}, fmt.Errorf("requirements 必须排序唯一")
		}
		prev = r
	}
	if len(p.Signature) != 128 {
		return VerifiedIdentity{}, fmt.Errorf("signature 非法")
	}
	sig, err := hex.DecodeString(p.Signature)
	if err != nil {
		return VerifiedIdentity{}, fmt.Errorf("signature 非法")
	}
	pub, err := hex.DecodeString(p.PublisherPublicKey)
	if err != nil {
		return VerifiedIdentity{}, fmt.Errorf("publisherPublicKey 非法")
	}
	digest, err := identityDigest(p)
	if err != nil {
		return VerifiedIdentity{}, err
	}
	if !verifySecp256k1Compact(pub, digest, sig) {
		return VerifiedIdentity{}, fmt.Errorf("appIdentity 签名无效")
	}
	return VerifiedIdentity{
		Version:            1,
		PublisherPublicKey: p.PublisherPublicKey,
		AppID:              p.App.ID,
		AppName:            p.App.Name,
		IdentityDigestHex:  hex.EncodeToString(digest),
		Requirements:       append([]string(nil), p.Requirements...),
	}, nil
}

func verifySecp256k1Compact(pub, digest, sig []byte) bool {
	// 使用 decred/secp256k1 v4 验证 compact 签名（64 字节 r||s）。
	// 公钥为 33 字节压缩格式，digest 为 32 字节。
	if len(pub) != 33 || len(digest) != 32 || len(sig) != 64 {
		return false
	}
	// 延迟导入避免循环：调用方在 verify_secp.go 实现。
	return verifyCompact(pub, digest, sig)
}

// IsValidSnapshot 校验已验证 proof 的不可变 snapshot。
func IsValidSnapshot(v VerifiedIdentity) bool {
	if v.Version != 1 || !appIDRe.MatchString(v.AppID) || v.AppName == "" || len(v.AppName) > 120 {
		return false
	}
	if !pubkeyRe.MatchString(v.PublisherPublicKey) || !hex64Re.MatchString(v.IdentityDigestHex) {
		return false
	}
	return true
}
