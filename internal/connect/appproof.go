package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// 钱包主动启动解析 App 身份的网络上限：issue 本身是 short 超时调用，
// 解析失败直接拒绝签发，不进入审批队列，不静默回退成未绑定启动。
const (
	appProofFetchTimeout = 8 * time.Second
	appProofMaxBody      = 1 << 20
)

// FetchAppProof 由钱包主动启动时确定用户授权的 App 身份：
// GET 应用精确 origin 根文档，提取 keymaster-app:* 发布元数据，
// 用现有 VerifyProof 校验签名，返回不可变的已验证身份快照。
// 同一发布机制既是 Demo 入口 HTML 的 proof 来源，也是集成验收解析 proof 的位置，
// 不引入第二套认证协议。
func FetchAppProof(ctx context.Context, origin string) (VerifiedIdentity, error) {
	u, err := parseExactHTTPOrigin(origin)
	if err != nil {
		return VerifiedIdentity{}, err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, appProofFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, u+"/", nil)
	if err != nil {
		return VerifiedIdentity{}, fmt.Errorf("应用身份证明获取失败: %v", err)
	}
	req.Header.Set("Accept", "text/html")
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		// 启动目标跳转后无法确认用户授权的仍是同一应用，拒绝而非跟随。
		return fmt.Errorf("应用身份证明不跟随跳转")
	}}
	resp, err := client.Do(req)
	if err != nil {
		return VerifiedIdentity{}, fmt.Errorf("应用身份证明获取失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return VerifiedIdentity{}, fmt.Errorf("应用身份证明获取失败: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, appProofMaxBody+1))
	if err != nil {
		return VerifiedIdentity{}, fmt.Errorf("应用身份证明获取失败: %v", err)
	}
	if len(body) > appProofMaxBody {
		return VerifiedIdentity{}, fmt.Errorf("应用身份证明过大")
	}
	raw, err := extractAppProof(body)
	if err != nil {
		return VerifiedIdentity{}, err
	}
	verified, err := VerifyProof(raw)
	if err != nil {
		return VerifiedIdentity{}, fmt.Errorf("应用身份证明无效: %v", err)
	}
	return verified, nil
}

// parseExactHTTPOrigin 只接受精确 HTTP(S) origin，不接受路径、查询或 opaque 来源。
func parseExactHTTPOrigin(origin string) (string, error) {
	rest, ok := strings.CutPrefix(origin, "http://")
	if !ok {
		rest, ok = strings.CutPrefix(origin, "https://")
		if !ok {
			return "", fmt.Errorf("应用地址必须为精确 HTTP(S) origin")
		}
	}
	if rest == "" || strings.ContainsAny(rest, "/?#@") {
		return "", fmt.Errorf("应用地址必须为精确 HTTP(S) origin")
	}
	return origin, nil
}

// extractAppProof 从 HTML 文档提取 keymaster-app:* 发布元数据并组装 proof。
// 使用标准 HTML 解析器读取真实属性值（含实体解码），与浏览器 DOM 读取一致：
// 引号内的 `>`、注释与脚本中的伪标签都按 HTML 语义处理，不计入。
// 单值字段必须恰好出现一次且非空（去首尾空格即非法，与 Demo 入口 proof
// 读取一致）；未知 keymaster-app:* 名称直接拒绝。
func extractAppProof(body []byte) (json.RawMessage, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("应用身份证明解析失败: %v", err)
	}
	singles := map[string]string{}
	seenSingle := map[string]int{}
	requirements := []string{}
	var walk func(*html.Node) error
	walk = func(n *html.Node) error {
		if n.Type == html.ElementNode && strings.EqualFold(n.Data, "meta") {
			var name, content string
			var hasName, hasContent bool
			for _, a := range n.Attr {
				// 重复属性按 HTML 语义取首次出现。
				switch strings.ToLower(a.Key) {
				case "name":
					if !hasName {
						name = strings.ToLower(a.Val)
						hasName = true
					}
				case "content":
					if !hasContent {
						content = a.Val
						hasContent = true
					}
				}
			}
			if hasName && hasContent {
				const prefix = "keymaster-app:"
				if strings.HasPrefix(name, prefix) {
					key := strings.TrimPrefix(name, prefix)
					switch key {
					case "id", "publisher-public-key", "name", "description", "identity-signature":
						seenSingle[key]++
						if seenSingle[key] > 1 {
							return fmt.Errorf("应用身份证明重复: %s", key)
						}
						// 与 Demo 入口 proof 读取一致：不静默 trim；含首尾空格即非法，
						// 保证重构的 proof 与发行人签名时的摘要输入完全一致。
						if content == "" || content != strings.TrimSpace(content) {
							return fmt.Errorf("应用身份证明非法: %s", key)
						}
						singles[key] = content
					case "requirement":
						requirements = append(requirements, content)
					default:
						return fmt.Errorf("应用身份证明含未知字段: %s", key)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(doc); err != nil {
		return nil, err
	}
	for _, key := range []string{"id", "publisher-public-key", "name", "description", "identity-signature"} {
		if seenSingle[key] != 1 {
			return nil, fmt.Errorf("应用身份证明缺失: %s", key)
		}
	}
	proof := map[string]any{
		"version":            1,
		"publisherPublicKey": singles["publisher-public-key"],
		"app": map[string]any{
			"id":          singles["id"],
			"name":        singles["name"],
			"description": singles["description"],
		},
		"requirements": requirements,
		"signature":    singles["identity-signature"],
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		return nil, fmt.Errorf("应用身份证明组装失败: %v", err)
	}
	return raw, nil
}
