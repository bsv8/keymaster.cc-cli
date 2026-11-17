// Package pluginmanifest 是前后台共同读取的产品方法归属表；不是授权名单。
package pluginmanifest

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed catalog.json
var catalog []byte

// Unit 描述一个业务插件的后台单元及其正式 APIProtocol 方法。
// Documentation 只承载活文档元数据，不参与运行装配；只有 PluginID/UnitID/Methods 影响 Runtime。
type Unit struct {
	PluginID      string         `json:"pluginId"`
	UnitID        string         `json:"unitId"`
	Methods       []string       `json:"methods"`
	Documentation *Documentation `json:"documentation,omitempty"`
}

// Documentation 集中插件活文档的国际化引用；键为中文源文，与 web/src/i18n.js 共用。
type Documentation struct {
	Name     string `json:"name"`
	Summary  string `json:"summary"`
	Duties   string `json:"duties"`
	Boundary string `json:"boundary"`
	Data     string `json:"data"`
}

func Units() []Unit {
	var result []Unit
	if err := json.Unmarshal(catalog, &result); err != nil {
		panic(err)
	}
	return result
}

// Validate 检查目录整体一致性：插件身份、单元身份、方法归属不得重复；
// 说明引用不得缺失；只校验结构，不限制插件数量（当前七个为事实，不硬编码上限）。
func Validate(units []Unit) error {
	seenPlugin := map[string]bool{}
	seenUnit := map[string]bool{}
	seenMethod := map[string]string{}
	for _, u := range units {
		if u.PluginID == "" || u.UnitID == "" {
			return fmt.Errorf("插件目录存在空身份")
		}
		if seenPlugin[u.PluginID] {
			return fmt.Errorf("插件身份重复 %q", u.PluginID)
		}
		seenPlugin[u.PluginID] = true
		if seenUnit[u.UnitID] {
			return fmt.Errorf("单元身份重复 %q", u.UnitID)
		}
		seenUnit[u.UnitID] = true
		if len(u.Methods) == 0 {
			return fmt.Errorf("插件 %q 缺少方法", u.PluginID)
		}
		for _, m := range u.Methods {
			if m == "" {
				return fmt.Errorf("插件 %q 存在空方法", u.PluginID)
			}
			if owner, ok := seenMethod[m]; ok {
				return fmt.Errorf("方法归属重复 %q（%s 与 %s）", m, owner, u.PluginID)
			}
			seenMethod[m] = u.PluginID
		}
		if u.Documentation == nil {
			return fmt.Errorf("插件 %q 缺少 documentation", u.PluginID)
		}
		d := u.Documentation
		if d.Name == "" || d.Summary == "" || d.Duties == "" || d.Boundary == "" || d.Data == "" {
			return fmt.Errorf("插件 %q 的 documentation 引用缺失", u.PluginID)
		}
	}
	return nil
}

// ValidateEmbedded 校验内嵌目录；用于后台启动前拒绝真实不一致。
func ValidateEmbedded() error {
	return Validate(Units())
}
