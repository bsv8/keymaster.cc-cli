package apppublication

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	apporigin "github.com/bsv8/keymaster.cc-cli/connectorigin"
	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/net/html"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	PublicationMeta       = "keymaster-app"
	PublicationDomain     = "keymaster.app-publication.v1"
	MaxHTMLBytes          = 16 * 1024 * 1024
	MaxFilesEntries       = 100000
	MaxManifestPathLength = 512
	FileBlockSize         = 256 * 1024
)

var (
	appPubPattern   = regexp.MustCompile(`^(02|03)[0-9a-f]{64}$`)
	sigHexPattern   = regexp.MustCompile(`^[0-9a-f]{128}$`)
	seedHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type AppFileEntry struct {
	Path        string `json:"path"`
	MIME        string `json:"mime"`
	Size        int64  `json:"size"`
	SeedHashHex string `json:"seedhashhex"`
}
type URLChannel struct {
	Address string `json:"address"`
	Icon    string `json:"icon"`
}
type FilesChannel struct {
	EntrySeedhashhex string         `json:"entrySeedhashhex"`
	Icon             string         `json:"icon"`
	Files            []AppFileEntry `json:"files"`
}
type Publication struct {
	AppVersion      string        `json:"appVersion"`
	Version         int           `json:"version"`
	Kind            string        `json:"kind"`
	AppPublicKeyHex string        `json:"appPublicKeyHex"`
	Name            string        `json:"name"`
	Description     string        `json:"description"`
	URL             *URLChannel   `json:"url,omitempty"`
	Files           *FilesChannel `json:"files,omitempty"`
}
type PublicationProof struct {
	Publication  *Publication `json:"publication"`
	SignatureHex string       `json:"signatureHex"`
}

func (p *Publication) Display() AppDisplay {
	icon := ""
	if p.URL != nil {
		icon = p.URL.Icon
	} else if p.Files != nil {
		icon = p.Files.Icon
	}
	return AppDisplay{Name: p.Name, Description: p.Description, Icon: icon}
}
func (p *Publication) FilesDisplay() AppDisplay {
	return AppDisplay{Name: p.Name, Description: p.Description, Icon: p.Files.Icon}
}
func ValidateAppPubKey(value string) (string, error) {
	if !appPubPattern.MatchString(value) {
		return "", fmt.Errorf("appPublicKeyHex 非法")
	}
	raw, _ := hex.DecodeString(value)
	if _, err := secp.ParsePubKey(raw); err != nil {
		return "", fmt.Errorf("appPublicKeyHex 不在曲线上")
	}
	return value, nil
}

func validateSigHex(value string) error {
	if !sigHexPattern.MatchString(value) {
		return fmt.Errorf("signatureHex 非法")
	}
	return nil
}

// domainHash 计算 SHA256(UTF8(domain) || NUL || payload)。
func domainHash(domain string, payload []byte) []byte {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write([]byte{0})
	h.Write(payload)
	return h.Sum(nil)
}

// canonicalJSON 将已解析的 JSON 值按规范编码：键排序、无 HTML 转义。
// 仅支持 JSON 对象/数组/字符串/数值/布尔/null；数值按 json.Number 原样保留。
func canonicalJSON(v any) (string, error) {
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		raw, err := json.Marshal(t)
		if err != nil {
			return err
		}
		// Match JSON.stringify without rewriting escaped literal backslashes.
		s := string(raw)
		for i := 0; i < len(s); {
			if s[i] != '\\' {
				buf.WriteByte(s[i])
				i++
				continue
			}
			if i+6 <= len(s) {
				escapes := map[string]string{`\u003c`: "<", `\u003e`: ">", `\u0026`: "&", `\u2028`: "\u2028", `\u2029`: "\u2029"}
				if text, ok := escapes[s[i:i+6]]; ok {
					buf.WriteString(text)
					i += 6
					continue
				}
			}
			buf.WriteString(s[i : i+2])
			i += 2
		}
	case json.Number:
		if _, err := t.Float64(); err != nil {
			// 整数原样输出。
			buf.WriteString(t.String())
			return nil
		}
		buf.WriteString(t.String())
	case float64:
		raw, err := json.Marshal(t)
		if err != nil {
			return err
		}
		buf.Write(raw)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	default:
		return fmt.Errorf("canonical: 不支持的类型 %T", v)
	}
	return nil
}

// decodePayloadJSON 将 base64url 载荷解码为 JSON 值（数值保留 json.Number）。
func verifyCompact(digest []byte, sigHex, pubHex string) bool {
	if len(digest) != 32 {
		return false
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != 64 {
		return false
	}
	pub, err := hex.DecodeString(pubHex)
	if err != nil || len(pub) != 33 {
		return false
	}
	pk, err := secp.ParsePubKey(pub)
	if err != nil {
		return false
	}
	var r, s secp.ModNScalar
	if r.SetByteSlice(sig[:32]) || s.SetByteSlice(sig[32:]) || r.IsZero() || s.IsZero() || s.IsOverHalfOrder() {
		return false
	}
	return ecdsa.NewSignature(&r, &s).Verify(digest, pk)
}

func validateManifestPath(path string) error {
	if len(path) == 0 || len(path) > MaxManifestPathLength {
		return fmt.Errorf("清单路径非法: %s", path)
	}
	if strings.HasPrefix(path, "/") || strings.Contains(path, "\\") || strings.ContainsRune(path, 0) {
		return fmt.Errorf("清单路径非法: %s", path)
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("清单路径非法: %s", path)
		}
	}
	segments := strings.Split(path, "/")
	for _, s := range segments {
		if s == "" || s == "." || s == ".." || len(s) > 255 {
			return fmt.Errorf("清单路径非法: %s", path)
		}
	}
	if path == "index.html" {
		return fmt.Errorf("清单不得列出 index.html 自身")
	}
	return nil
}

func MasterSeedOfBytes(data []byte) (seed []byte, seedHashHex string) {
	var digests []byte
	for offset := 0; offset < len(data); offset += FileBlockSize {
		end := offset + FileBlockSize
		if end > len(data) {
			end = len(data)
		}
		sum := sha256.Sum256(data[offset:end])
		digests = append(digests, sum[:]...)
	}
	seed = digests
	sum := sha256.Sum256(seed)
	return seed, hex.EncodeToString(sum[:])
}
func exact(value any, keys ...string) (map[string]any, error) {
	m, ok := value.(map[string]any)
	if !ok || len(m) != len(keys) {
		return nil, fmt.Errorf("publication fields invalid")
	}
	for _, key := range keys {
		if _, ok := m[key]; !ok {
			return nil, fmt.Errorf("publication field missing: %s", key)
		}
	}
	return m, nil
}
func parsePublication(value any) (*Publication, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("publication object required")
	}
	kind, _ := m["kind"].(string)
	keys := []string{"version", "appVersion", "kind", "appPublicKeyHex", "name", "description"}
	switch kind {
	case "url":
		keys = append(keys, "url")
	case "files":
		keys = append(keys, "files")
	case "both":
		keys = append(keys, "url", "files")
	default:
		return nil, fmt.Errorf("publication kind invalid")
	}
	if _, e := exact(m, keys...); e != nil {
		return nil, e
	}
	raw, e := json.Marshal(m)
	if e != nil {
		return nil, e
	}
	var p Publication
	if e = json.Unmarshal(raw, &p); e != nil {
		return nil, e
	}
	if err := ValidateAppVersion(p.AppVersion); err != nil {
		return nil, err
	}
	if p.Version != 1 {
		return nil, fmt.Errorf("version invalid")
	}
	if _, e = ValidateAppPubKey(p.AppPublicKeyHex); e != nil {
		return nil, e
	}
	if _, e = ParseDisplay(map[string]any{"name": p.Name, "description": p.Description, "icon": "valid"}); e != nil {
		return nil, e
	}
	if p.URL != nil {
		if _, e = exact(m["url"], "address", "icon"); e != nil {
			return nil, e
		}
		for _, s := range []string{p.URL.Address, p.URL.Icon} {
			u, er := canonicalHTTPURL(s)
			if er != nil || u != s || len(s) > 8192 {
				return nil, fmt.Errorf("publication URL invalid")
			}
		}
	} else if kind != "files" {
		return nil, fmt.Errorf("URL channel missing")
	}
	if p.Files != nil {
		f, e := exact(m["files"], "entrySeedhashhex", "icon", "files")
		if e != nil {
			return nil, e
		}
		if !seedHashPattern.MatchString(p.Files.EntrySeedhashhex) || !seedHashPattern.MatchString(p.Files.Icon) {
			return nil, fmt.Errorf("invalid Files seeds")
		}
		items, ok := f["files"].([]any)
		if !ok || len(items) > MaxFilesEntries {
			return nil, fmt.Errorf("manifest invalid")
		}
		last := ""
		icon := false
		for i, item := range items {
			if _, e = exact(item, "path", "mime", "size", "seedhashhex"); e != nil {
				return nil, e
			}
			entry := p.Files.Files[i]
			if e = validateManifestPath(entry.Path); e != nil {
				return nil, e
			}
			if !pathLess(last, entry.Path) || entry.Size < 0 || entry.Size > 9007199254740991 || !seedHashPattern.MatchString(entry.SeedHashHex) || entry.MIME == "" || utf8.RuneCountInString(entry.MIME) > 256 || trimDisplayText(entry.MIME) != entry.MIME || strings.ContainsAny(entry.MIME, "\r\n") {
				return nil, fmt.Errorf("manifest item invalid")
			}
			for _, r := range entry.MIME {
				if r < 32 || (r >= 127 && r <= 159) {
					return nil, fmt.Errorf("invalid MIME control")
				}
			}
			last = entry.Path
			if entry.SeedHashHex == p.Files.Icon {
				icon = true
			}
		}
		if !icon {
			return nil, fmt.Errorf("icon seed absent from manifest")
		}
	} else if kind != "url" {
		return nil, fmt.Errorf("Files channel missing")
	}
	return &p, nil
}
func DecodePublication(content string) (*PublicationProof, error) {
	parts := strings.Split(content, ".")
	if len(parts) != 2 || validateSigHex(parts[1]) != nil {
		return nil, fmt.Errorf("publication encoding invalid")
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if e != nil || base64.RawURLEncoding.EncodeToString(raw) != parts[0] || !utf8.Valid(raw) {
		return nil, fmt.Errorf("publication encoding invalid")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if e = dec.Decode(&value); e != nil {
		return nil, e
	}
	var tail any
	if dec.Decode(&tail) != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	p, e := parsePublication(value)
	if e != nil {
		return nil, e
	}
	canonical, e := canonicalJSON(value)
	if e != nil || canonical != string(raw) {
		return nil, fmt.Errorf("noncanonical publication JSON")
	}
	return &PublicationProof{Publication: p, SignatureHex: parts[1]}, nil
}

// PublicationID identifies the complete verified publication across its channels.
func PublicationID(p *Publication) (string, error) {
	canonical, err := CanonicalPublication(p)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:]), nil
}

func CanonicalPublication(p *Publication) (string, error) {
	raw, e := json.Marshal(p)
	if e != nil {
		return "", e
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if e = dec.Decode(&value); e != nil {
		return "", e
	}
	if _, e = parsePublication(value); e != nil {
		return "", e
	}
	return canonicalJSON(value)
}
func VerifyPublication(proof *PublicationProof, expectedOrigin string) (*Publication, error) {
	if proof == nil || proof.Publication == nil {
		return nil, fmt.Errorf("publication missing")
	}
	canonical, e := CanonicalPublication(proof.Publication)
	if e != nil {
		return nil, e
	}
	content := base64.RawURLEncoding.EncodeToString([]byte(canonical)) + "." + proof.SignatureHex
	if _, e = DecodePublication(content); e != nil {
		return nil, e
	}
	p := proof.Publication
	if !verifyCompact(domainHash(PublicationDomain, []byte(canonical)), proof.SignatureHex, p.AppPublicKeyHex) {
		return nil, fmt.Errorf("publication signature mismatch")
	}
	if expectedOrigin != "" {
		if p.URL == nil {
			return nil, fmt.Errorf("URL channel required")
		}
		origin, e := apporigin.ParseEntryOrigin(p.URL.Address)
		if e != nil || origin != expectedOrigin {
			return nil, fmt.Errorf("publication origin mismatch")
		}
	}
	return p, nil
}

type recordRange struct {
	start, end int
	content    string
}

var fixedRecord = regexp.MustCompile(`^<meta name="keymaster-app" content="([A-Za-z0-9_.-]*)">$`)

func locateRecord(source []byte) (*recordRange, error) {
	if len(source) > MaxHTMLBytes || !utf8.Valid(source) {
		return nil, fmt.Errorf("entry size or UTF-8 invalid")
	}
	z := html.NewTokenizer(bytes.NewReader(source))
	offset := 0
	var found *recordRange
	inHead := false
	heads := 0
	for {
		tt := z.Next()
		raw := append([]byte(nil), z.Raw()...)
		start := offset
		offset += len(raw)
		if tt == html.ErrorToken {
			if z.Err() != io.EOF {
				return nil, z.Err()
			}
			break
		}
		token := z.Token()
		if tt == html.StartTagToken && token.Data == "head" {
			heads++
			inHead = true
		}
		if (tt == html.EndTagToken && token.Data == "head") || (tt == html.StartTagToken && token.Data == "body") {
			inHead = false
		}
		if (tt == html.StartTagToken || tt == html.SelfClosingTagToken) && token.Data == "meta" {
			for _, a := range token.Attr {
				if a.Key != "name" {
					continue
				}
				switch a.Val {
				case "keymaster-app-files", "keymaster-app-url-proof", "keymaster-app-name", "keymaster-app-description", "keymaster-app-icon":
					return nil, fmt.Errorf("obsolete publication meta")
				case PublicationMeta:
					if found != nil || !inHead {
						return nil, fmt.Errorf("publication must be unique in actual head")
					}
					m := fixedRecord.FindSubmatchIndex(raw)
					if m == nil {
						return nil, fmt.Errorf("publication fixed syntax required")
					}
					found = &recordRange{start: start + m[2], end: start + m[3], content: string(raw[m[2]:m[3]])}
				}
			}
		}
	}
	if found == nil || heads != 1 {
		return nil, fmt.Errorf("publication missing or invalid head")
	}
	// Confirm HTML tree semantics, including template/script/comment handling.
	doc, e := html.Parse(bytes.NewReader(bytes.TrimPrefix(source, []byte{239, 187, 191})))
	if e != nil {
		return nil, e
	}
	count := 0
	valid := true
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "meta" {
			for _, a := range n.Attr {
				if a.Key == "name" && a.Val == PublicationMeta {
					count++
					if n.Parent == nil || n.Parent.Data != "head" {
						valid = false
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if !valid || count != 1 {
		return nil, fmt.Errorf("publication outside actual head")
	}
	return found, nil
}
func ReadPublicationBytes(source []byte) (*PublicationProof, error) {
	rec, e := locateRecord(source)
	if e != nil {
		return nil, e
	}
	return DecodePublication(rec.content)
}
func VerifyEntryBytes(b []byte) (*Publication, []byte, []byte, string, error) {
	rec, e := locateRecord(b)
	if e != nil {
		return nil, nil, nil, "", e
	}
	proof, e := DecodePublication(rec.content)
	if e != nil {
		return nil, nil, nil, "", e
	}
	p, e := VerifyPublication(proof, "")
	if e != nil {
		return nil, nil, nil, "", e
	}
	if p.Files == nil {
		return nil, nil, nil, "", fmt.Errorf("Files channel required")
	}
	a := append(append([]byte{}, b[:rec.start]...), b[rec.end:]...)
	_, hash := MasterSeedOfBytes(a)
	if hash != p.Files.EntrySeedhashhex {
		return nil, nil, nil, "", fmt.Errorf("entry seed mismatch")
	}
	seed, hash := MasterSeedOfBytes(b)
	return p, a, seed, hash, nil
}

func pathLess(a, b string) bool {
	left, right := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(left) && i < len(right); i++ {
		if left[i] != right[i] {
			return left[i] < right[i]
		}
	}
	return len(left) < len(right)
}

// ValidateAppVersion 校验大、中、小三段非负整数，不允许前导零或后缀。
func ValidateAppVersion(value string) error {
	if !regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(value) {
		return fmt.Errorf("invalid appVersion")
	}
	return nil
}

// CompareAppVersions 按整数大小比较，不依赖机器整数宽度。
func CompareAppVersions(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		if len(left[i]) < len(right[i]) {
			return -1
		}
		if len(left[i]) > len(right[i]) {
			return 1
		}
		if left[i] < right[i] {
			return -1
		}
		if left[i] > right[i] {
			return 1
		}
	}
	return 0
}
