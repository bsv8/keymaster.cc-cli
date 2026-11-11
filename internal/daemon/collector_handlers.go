package daemon

import (
	"sort"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
	"github.com/bsv8/keymaster.cc-cli/internal/collector"
)

// collectorStatusView 构建采集器状态视图，供 API 与事件发布共用。
func (d *Daemon) collectorStatusView() kmapi.CollectorStatus {
	_, settings, err := d.store.Snapshot()
	if err != nil {
		return kmapi.CollectorStatus{}
	}
	out := []kmapi.CollectorStatusEntry{}
	if d.collectors != nil {
		names := map[string]string{}
		for _, e := range settings.Server.Collector.Collectors {
			names[e.ID] = e.Name
		}
		var mu []kmapi.CollectorStatusEntry
		d.collectors.each(func(id string, svc *collector.Service) {
			st := svc.Status()
			pc, _ := st["pendingCount"].(int)
			cc, _ := st["confirmedCount"].(int)
			inf, _ := st["inflight"].(bool)
			le, _ := st["lastError"].(string)
			ep, _ := st["endpoint"].(string)
			en, _ := st["enabled"].(bool)
			var lastPtr *int64
			if v, ok := st["lastSuccessAt"]; ok && v != nil {
				switch n := v.(type) {
				case int64:
					lastPtr = optI64(n, true)
				case float64:
					n64 := int64(n)
					lastPtr = optI64(n64, true)
				}
			}
			mu = append(mu, kmapi.CollectorStatusEntry{
				ID: id, Name: optStr(names[id]), Endpoint: optStr(ep), Enabled: en,
				PendingCount: pc, ConfirmedCount: cc, Inflight: inf,
				LastError: optStr(le), LastSuccessAt: lastPtr,
			})
		})
		out = mu
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return kmapi.CollectorStatus{
		Collectors: out,
		Intervals: map[string]any{
			"earlyTriggerCount": settings.Server.Collector.EarlyTriggerCount,
			"submitIntervalMs":  settings.Server.Collector.SubmitIntervalMs,
			"retryBaseMs":       settings.Server.Collector.RetryBaseMs,
			"retryCapMs":        settings.Server.Collector.RetryCapMs,
		},
		OfficialEndpoint: reqStr(appconfig.DefaultCollectorEndpoint),
	}
}

// handleCollectorStatus aggregates per-collector queues, intervals and state.
