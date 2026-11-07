package collector

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// SubmissionVersion 是当前唯一的提交版本号。SigVersion 是签名版本。
const (
	SubmissionVersion = 1
	SigVersion        = 1
)

// SignatureDomain 是摘要域前缀，防止 BitFS wire 签名跨协议重放。
const SignatureDomain = "BitFSCollector/submission-v1\n"

// 请求头与端点。SubmissionsPath 是提交端点。
const (
	HeaderPubkey    = "X-BitFSCollector-Pubkey"
	HeaderSignature = "X-BitFSCollector-Signature"
	HeaderSigVer    = "X-BitFSCollector-SigVersion"
	SubmissionsPath = "/v1/submissions"
	ContentType     = "application/gzip"
)

// Record 是一条文件提交记录：身份仅为 SeedHashHex。
type Record struct {
	SeedHashHex   string `json:"seedHashHex"`
	FileName      string `json:"fileName"`
	Description   string `json:"description"`
	FileSizeBytes string `json:"fileSizeBytes"`
	MediaType     string `json:"mediaType"`
}

var (
	hashHexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	uint64Pattern  = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	mediaPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]*/[a-z0-9][a-z0-9!#$&^_.+-]*$`)
)

// ValidateRecord 校验单条记录的字段边界。
func ValidateRecord(r Record) error {
	if !hashHexPattern.MatchString(r.SeedHashHex) {
		return fmt.Errorf("collector: seedHashHex 非法")
	}
	if len(r.FileName) < 1 || len(r.FileName) > 255 || r.FileName == "." || r.FileName == ".." ||
		strings.ContainsAny(r.FileName, "/\\") {
		return fmt.Errorf("collector: fileName 非法")
	}
	for _, c := range r.FileName {
		if c < 0x20 || (c >= 0x7f && c <= 0x9f) {
			return fmt.Errorf("collector: fileName 含控制字符")
		}
	}
	if len(r.Description) > 2048 {
		return fmt.Errorf("collector: description 超出 2048 字节")
	}
	if !uint64Pattern.MatchString(r.FileSizeBytes) {
		return fmt.Errorf("collector: fileSizeBytes 非法")
	}
	if len(r.MediaType) < 1 || len(r.MediaType) > 255 || !mediaPattern.MatchString(r.MediaType) {
		return fmt.Errorf("collector: mediaType 非法")
	}
	return nil
}

// CanonicalJSON 编码跨语言逐字节一致的规范 JSON：键顺序固定、无空白、
// 文件按 SeedHashHex 升序、关闭 HTML 转义。
func CanonicalJSON(records []Record) ([]byte, error) {
	if len(records) == 0 {
		return nil, fmt.Errorf("collector: files 为空")
	}
	sorted := append([]Record(nil), records...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].SeedHashHex < sorted[j].SeedHashHex })
	for i, r := range sorted {
		if err := ValidateRecord(r); err != nil {
			return nil, fmt.Errorf("collector: files[%d]: %w", i, err)
		}
		if i > 0 && sorted[i-1].SeedHashHex == r.SeedHashHex {
			return nil, fmt.Errorf("collector: seedHashHex 重复")
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		Version int      `json:"version"`
		Files   []Record `json:"files"`
	}{Version: SubmissionVersion, Files: sorted}); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// GzipJSON 压缩规范 JSON。不同实现允许输出不同压缩字节。
func GzipJSON(canonical []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(canonical); err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DigestGzip 计算签名摘要：SHA-256(域前缀 + gzip 原始字节)。
func DigestGzip(gzipBytes []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(SignatureDomain))
	h.Write(gzipBytes)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// SubmissionURL 归一化提交地址：已含提交路径（含自定义前缀）则原样使用，
// 否则补上默认路径。兼容只填基地址的旧配置。
func SubmissionURL(endpoint string) string {
	trimmed := strings.TrimSpace(endpoint)
	for strings.HasSuffix(trimmed, "/") {
		trimmed = strings.TrimSuffix(trimmed, "/")
	}
	if strings.HasSuffix(trimmed, SubmissionsPath) {
		return trimmed
	}
	return trimmed + SubmissionsPath
}

// GunzipJSON 解压回规范 JSON（自检用，服务端不解析）。
func GunzipJSON(gzipBytes []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(gzipBytes))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
