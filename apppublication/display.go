package apppublication

import (
	"fmt"
	whatwg "github.com/nlnwa/whatwg-url/url"
	"strings"
	"unicode/utf8"
)

type AppDisplay struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
}

func ParseDisplay(v any) (AppDisplay, error) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 3 {
		return AppDisplay{}, fmt.Errorf("display fields invalid")
	}
	d := AppDisplay{}
	fields := []struct {
		name   string
		limit  int
		target *string
	}{{"name", 120, &d.Name}, {"description", 2000, &d.Description}, {"icon", 8192, &d.Icon}}
	for _, f := range fields {
		s, ok := m[f.name].(string)
		if !ok || s != trimDisplayText(s) || !utf8.ValidString(s) || utf8.RuneCountInString(s) > f.limit || (f.name != "description" && s == "") {
			return d, fmt.Errorf("keymaster-app-%s missing, empty or invalid", f.name)
		}
		for _, r := range s {
			if r < 32 || (r >= 127 && r <= 159) {
				return d, fmt.Errorf("keymaster-app-%s control character", f.name)
			}
		}
		*f.target = s
	}
	return d, nil
}
func DisplayMap(d AppDisplay) map[string]any {
	return map[string]any{"name": d.Name, "description": d.Description, "icon": d.Icon}
}
func canonicalHTTPURL(s string) (string, error) {
	u, e := whatwg.Parse(s)
	if e != nil {
		return "", e
	}
	if (u.Scheme() != "http" && u.Scheme() != "https") || u.Username() != "" || u.Password() != "" || u.Host() == "" {
		return "", fmt.Errorf("HTTP(S) URL required")
	}
	return u.Href(false), nil
}
func trimDisplayText(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return r == 9 || r == 10 || r == 11 || r == 12 || r == 13 || r == 32 || r == 0xa0 || r == 0x1680 || (r >= 0x2000 && r <= 0x200a) || r == 0x2028 || r == 0x2029 || r == 0x202f || r == 0x205f || r == 0x3000 || r == 0xfeff
	})
}
