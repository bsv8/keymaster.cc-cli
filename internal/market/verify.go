package market

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bsv8/KeymasterMarketProtocol"
	"github.com/bsv8/keymaster.cc-cli/apppublication"
	"github.com/bsv8/keymaster.cc-cli/internal/connect"
)

const maxDirectoryBytes = 64 << 20 // KeymasterMarketProtocol v1 HTTP 目录上限

// verifyURLProof 验证 URL 渠道证明：解码、验签并要求发布包含受签名 URL
// 渠道；返回已验证公钥、版本、展示与签署完整入口。目录验签不绑定具体
// 请求源（expectedOrigin 为空跳过来源比对）；真实 URL 来源仍走建连验证。
func verifyURLProof(proof string) (marketprotocol.VerifiedPublication, error) {
	verified, err := decodeVerifiedProof(proof)
	if err != nil {
		return marketprotocol.VerifiedPublication{}, err
	}
	if verified.url == nil || verified.url.Address == "" {
		return marketprotocol.VerifiedPublication{}, fmt.Errorf("signed URL channel required")
	}
	return marketprotocol.VerifiedPublication{
		AppPublicKeyHex: verified.pub.AppPublicKeyHex,
		AppVersion:      verified.pub.AppVersion,
		Name:            verified.pub.Name,
		Description:     verified.pub.Description,
		IconURL:         verified.url.Icon,
		PublicationID:   verified.id,
		URLAddress:      verified.url.Address,
	}, nil
}

// verifyFilesProof 验证 Files 渠道证明：解码、验签并要求发布包含受签名
// Files 渠道；Files 的下载定位值随目录提供，实际部署时再核对入口摘要、
// 证明与文件清单。
func verifyFilesProof(proof string) (marketprotocol.VerifiedPublication, error) {
	verified, err := decodeVerifiedProof(proof)
	if err != nil {
		return marketprotocol.VerifiedPublication{}, err
	}
	if verified.pub.Files == nil {
		return marketprotocol.VerifiedPublication{}, fmt.Errorf("signed Files channel required")
	}
	return marketprotocol.VerifiedPublication{
		AppPublicKeyHex: verified.pub.AppPublicKeyHex,
		AppVersion:      verified.pub.AppVersion,
		Name:            verified.pub.Name,
		Description:     verified.pub.Description,
		IconURL:         verified.pub.Files.Icon,
		PublicationID:   verified.id,
	}, nil
}

type verifiedProof struct {
	pub *apppublication.Publication
	url *apppublication.URLChannel
	id  string
}

func decodeVerifiedProof(proof string) (verifiedProof, error) {
	decoded, err := apppublication.DecodePublication(proof)
	if err != nil {
		return verifiedProof{}, err
	}
	pub, err := apppublication.VerifyPublication(decoded, "")
	if err != nil {
		return verifiedProof{}, err
	}
	id, err := apppublication.PublicationID(pub)
	if err != nil {
		return verifiedProof{}, err
	}
	return verifiedProof{pub: pub, url: pub.URL, id: id}, nil
}

// FetchMarketEntries 拉取并整份校验一个市场的目录响应：GET 完整目录 URL，
// 经协议库逐渠道验签、双渠道一致性检查与按公钥去重后，转换为 Connect
// 目录条目。失败、截断、结构错误或验签失败整轮失败。
func FetchMarketEntries(ctx context.Context, directoryURL string) ([]connect.DirectoryEntry, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, directoryURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("market status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDirectoryBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxDirectoryBytes {
		return nil, fmt.Errorf("market directory too large")
	}
	apps, err := marketprotocol.DecodeDirectory(raw, verifyURLProof, verifyFilesProof)
	if err != nil {
		return nil, err
	}
	out := make([]connect.DirectoryEntry, 0, len(apps))
	for _, app := range apps {
		entry := connect.DirectoryEntry{
			AppVersion:      app.AppVersion,
			AppPublicKeyHex: app.AppPublicKeyHex,
			Name:            app.Name,
			Description:     app.Description,
			Icon:            app.IconURL,
			URLs:            []connect.DirectoryURL{},
			Files:           []connect.DirectoryFiles{},
		}
		if app.URL != nil {
			origin, err := connect.ParseEntryOrigin(app.URL.Address)
			if err != nil {
				return nil, fmt.Errorf("directory url origin invalid: %w", err)
			}
			entry.URLs = append(entry.URLs, connect.DirectoryURL{
				PublicationID: app.URL.PublicationID, Display: displayOf(app.URL.Name, app.URL.Description, app.URL.IconURL),
				URL: app.URL.Address, Origin: origin,
			})
		}
		if app.Files != nil {
			entry.Files = append(entry.Files, connect.DirectoryFiles{
				PublicationID:    app.Files.PublicationID,
				Display:          displayOf(app.Files.Name, app.Files.Description, app.Files.IconURL),
				Indexseedhashhex: app.Files.EntrySeedhashhex,
			})
		}
		out = append(out, entry)
	}
	if err := connect.ValidateDirectorySnapshot(out); err != nil {
		return nil, err
	}
	return out, nil
}

func displayOf(name, description, icon string) apppublication.AppDisplay {
	return apppublication.AppDisplay{Name: name, Description: description, Icon: icon}
}

// VerifyManualURL 获取真实入口，解析发布证明，校验签名、版本及签署入口与
// 实际验证地址的关系；名称、介绍、图标和公钥取自验签后的发布资料。
func VerifyManualURL(ctx context.Context, rawURL string) (connect.DirectoryEntry, error) {
	full, origin, err := connect.CanonicalEntryURL(rawURL)
	if err != nil {
		return connect.DirectoryEntry{}, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return connect.DirectoryEntry{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return connect.DirectoryEntry{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return connect.DirectoryEntry{}, fmt.Errorf("entry status %d", resp.StatusCode)
	}
	if resp.Request != nil && resp.Request.URL != nil {
		// 重定向后按最终地址核对签署来源。
		final, finalOrigin, ferr := connect.CanonicalEntryURL(resp.Request.URL.String())
		if ferr == nil {
			full, origin = final, finalOrigin
		}
	}
	const maxEntryHTMLBytes = 16 << 20 // 与 Web 侧入口读取上限一致
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxEntryHTMLBytes+1))
	if err != nil {
		return connect.DirectoryEntry{}, err
	}
	if len(body) > maxEntryHTMLBytes {
		return connect.DirectoryEntry{}, fmt.Errorf("entry too large")
	}
	proof, err := apppublication.ReadPublicationBytes(body)
	if err != nil {
		return connect.DirectoryEntry{}, err
	}
	publication, err := apppublication.VerifyPublication(proof, origin)
	if err != nil {
		return connect.DirectoryEntry{}, err
	}
	if publication.URL == nil {
		return connect.DirectoryEntry{}, fmt.Errorf("publication has no URL channel")
	}
	publicationID, err := apppublication.PublicationID(publication)
	if err != nil {
		return connect.DirectoryEntry{}, err
	}
	entryOrigin, err := connect.ParseEntryOrigin(publication.URL.Address)
	if err != nil {
		return connect.DirectoryEntry{}, err
	}
	display := apppublication.AppDisplay{Name: publication.Name, Description: publication.Description, Icon: publication.URL.Icon}
	return connect.DirectoryEntry{
		PublicationID:   publicationID,
		AppVersion:      publication.AppVersion,
		AppPublicKeyHex: publication.AppPublicKeyHex,
		Name:            publication.Name,
		Description:     publication.Description,
		Icon:            publication.URL.Icon,
		URLs: []connect.DirectoryURL{{
			PublicationID: publicationID, Display: display,
			URL: publication.URL.Address, Origin: entryOrigin,
		}},
		Files: []connect.DirectoryFiles{},
	}, nil
}

// EntryDirectoryURL 由基址构成内置市场的完整目录 URL；dev 基址覆盖仅
// 作用于内置市场。
func EntryDirectoryURL(base string) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(base), "/")
	if trimmed == "" {
		return DefaultMarketDirectoryURL, nil
	}
	full, err := CanonicalDirectoryURL(trimmed + "/v1/apps")
	if err != nil {
		return "", err
	}
	return full, nil
}
