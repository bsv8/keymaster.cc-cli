package daemon

import (
	"context"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/pluginmanifest"
)

// opPluginsInspect 为后台只读诊断：返回后台实例标识、Go Runtime 标识与
// active、真实单元列表及后台实际方法归属目录。钱包管理上下文可调用，
// App 上下文由统一分派器拒绝；初始化前、锁定可读；不读私钥、不触发解锁、
// 不修改持久化；首版不制造框架没有提供的逐插件业务就绪状态。
func (d *Daemon) opPluginsInspect(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if d.plugins == nil {
		return nil, errDetail(kmapi.ErrInternal, "后台插件不可用")
	}
	snap := d.plugins.Snapshot()
	units := make([]kmapi.PluginUnitView, 0, len(snap.Units))
	for _, u := range snap.Units {
		units = append(units, kmapi.PluginUnitView{
			PluginID:   u.PluginID,
			UnitID:     u.UnitID,
			InstanceID: u.InstanceID,
		})
	}
	catalog := make([]kmapi.PluginCatalogEntry, 0)
	for _, u := range pluginmanifest.Units() {
		methods := append([]string(nil), u.Methods...)
		catalog = append(catalog, kmapi.PluginCatalogEntry{
			PluginID: u.PluginID,
			UnitID:   u.UnitID,
			Methods:  methods,
		})
	}
	return kmapi.PluginsInspectView{
		InstanceID:        d.InstanceID(),
		RuntimeInstanceID: snap.RuntimeInstanceID,
		Active:            snap.Active,
		Units:             units,
		Catalog:           catalog,
	}, nil
}
