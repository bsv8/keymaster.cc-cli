// Package connectorigin 提供 WHATWG URL 对齐的 origin 解析。
//
// 解析算法由标准库 github.com/nlnwa/whatwg-url 实现，与浏览器 new URL 一致；
// 本包仅保留很薄的校验封装：仅 http(s)、拒绝 userinfo、拒绝空 host，strict
// 模式拒绝路径/查询/片段。IDN punycode（含 U+3002 等域名分隔符）、IPv4 简写、
// IPv6 序列化、百分比编码域名、端口越界等全部由标准库判定，不再手写解析分支。
//
// CLI 与市场服务分别依赖同一版本的标准库，各自保留本封装；“唯一真源”指
// 解析算法，不要求市场导入本包。
package connectorigin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	whatwg "github.com/nlnwa/whatwg-url/url"
)

// parseOrigin 经标准库解析并执行薄校验；strict 下拒绝路径/查询/片段。
func parseOrigin(raw string, strict bool) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("invalid url")
	}
	u, err := whatwg.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid url")
	}
	scheme := strings.ToLower(u.Scheme())
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("scheme must be http or https")
	}
	if u.Username() != "" || u.Password() != "" {
		return "", fmt.Errorf("userinfo forbidden")
	}
	host := u.Host()
	if host == "" {
		return "", fmt.Errorf("host missing")
	}
	if strict {
		if path := u.Pathname(); path != "" && path != "/" {
			return "", fmt.Errorf("origin must not contain a path")
		}
		if u.Query() != "" || u.Fragment() != "" {
			return "", fmt.Errorf("origin must not contain query or fragment")
		}
	}
	return scheme + "://" + host, nil
}

// CanonicalOrigin 校验并规范化可信 origin 输入。
func CanonicalOrigin(raw string) (string, error) {
	return parseOrigin(raw, true)
}

// ParseEntryOrigin 从应用入口 URL 推导 canonical origin。
func ParseEntryOrigin(raw string) (string, error) {
	return parseOrigin(raw, false)
}

// OriginKey 返回 originKey = lowercase hex SHA256(UTF8(canonicalOrigin))。
func OriginKey(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// StorageScope 构造路径绑定的加密 scope。
func StorageScope(root, path string) string {
	sum := sha256.Sum256([]byte(root + path))
	return "storage.encrypted-file.v1|" + hex.EncodeToString(sum[:])
}
