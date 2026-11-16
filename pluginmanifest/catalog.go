// Package pluginmanifest 是前后台共同读取的产品方法归属表；不是授权名单。
package pluginmanifest

import (
	_ "embed"
	"encoding/json"
)

//go:embed catalog.json
var catalog []byte

// Unit 描述一个业务插件的后台单元及其正式 APIProtocol 方法。
type Unit struct {
	PluginID string   `json:"pluginId"`
	UnitID   string   `json:"unitId"`
	Methods  []string `json:"methods"`
}

func Units() []Unit {
	var result []Unit
	if err := json.Unmarshal(catalog, &result); err != nil {
		panic(err)
	}
	return result
}
