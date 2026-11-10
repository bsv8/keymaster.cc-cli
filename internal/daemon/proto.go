package daemon

import (
	"encoding/json"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
)

// 本文件是产品内部胶水：把后台内部状态翻译成协议 DTO。线格式以 v1 为准，
// 逐键核对：当前实现写出的键，DTO 必须写出；当前省略的键，DTO 必须省略
//（可选指针为 nil 时省略，非 nil 空字符串仍写出）。

// optStr 在空字符串时返回 nil（对应当前 omitempty 省略行为）。
func optStr(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

// reqStr 永远返回非 nil（对应当前无条件写出的键，即使值为空）。
func reqStr(s string) *string {
	v := s
	return &v
}

func optBool(b bool, present bool) *bool {
	if !present {
		return nil
	}
	v := b
	return &v
}

func optU64(v uint64, present bool) *uint64 {
	if !present {
		return nil
	}
	out := v
	return &out
}

func optI64(v int64, present bool) *int64 {
	if !present {
		return nil
	}
	out := v
	return &out
}

// taskDTO 把内部任务翻译成协议 DTO，不暴露 cancel/done/subs 等执行态。
func taskDTO(t *Task) kmapi.Task {
	return kmapi.Task{
		SeedHashHex: t.SeedHashHex,
		RootID:      t.RootID,
		State:       string(t.State),
		Detail:      optStr(t.Detail),
		BlocksDone:  t.BlocksDone,
		BlocksTotal: t.BlocksTotal,
		Recommended: append([]string(nil), t.Recommended...),
		FinalPath:   optStr(t.FinalPath),
		Bytes:       optI64(t.Bytes, t.Bytes != 0),
		PaidSats:    optStr(t.PaidSats),
		Error:       optStr(t.Error),
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
	}
}

// taskListDTO 翻译任务列表；空列表保持 []（与当前行为一致，不写 null）。
func taskListDTO(tasks []Task) kmapi.TaskList {
	out := make([]kmapi.Task, 0, len(tasks))
	for i := range tasks {
		out = append(out, taskDTO(&tasks[i]))
	}
	return kmapi.TaskList{Tasks: out}
}

// settingsDTO 把统一存储的设置全文翻译成协议 DTO。两边 JSON 标签一致，
// 经一次编码往返保持字段、缺省与空值口径；磁盘类型本身不导出。
func settingsDTO(s appconfig.Settings) kmapi.Settings {
	raw, err := json.Marshal(s)
	if err != nil {
		return kmapi.Settings{}
	}
	var out kmapi.Settings
	if err := json.Unmarshal(raw, &out); err != nil {
		return kmapi.Settings{}
	}
	return out
}

// collectorEntriesToApp 把协议采集器条目翻回磁盘类型。Name/Endpoint 的
// 缺失与空字符串都落为 ""；空列表落为 nil，与当前整份替换语义一致。
func collectorEntriesToApp(in []kmapi.CollectorEntry) []appconfig.CollectorEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]appconfig.CollectorEntry, 0, len(in))
	for _, e := range in {
		name, endpoint := "", ""
		if e.Name != nil {
			name = *e.Name
		}
		if e.Endpoint != nil {
			endpoint = *e.Endpoint
		}
		out = append(out, appconfig.CollectorEntry{
			ID: e.ID, Name: name, Endpoint: endpoint, Enabled: e.Enabled,
		})
	}
	return out
}

// applySettingsUpdate 把部分更新（指针语义：省略不动，显式零值参与校验）
// 应用到设置副本，与此前 settingsUpdate 的行为一致。
func applySettingsUpdate(next *appconfig.Settings, body *kmapi.SettingsUpdate) error {
	if body.Port != nil {
		next.Port = *body.Port
	}
	if body.Theme != nil {
		next.Theme = *body.Theme
	}
	if body.Language != nil {
		next.Language = *body.Language
	}
	if body.WocAPIKey != nil {
		next.WOCAPIKey = *body.WocAPIKey
	}
	if body.FeeRateSatoshisPerKb != nil {
		next.FeeRateSatoshisPerKb = *body.FeeRateSatoshisPerKb
	}
	if body.Scheduler != nil {
		if body.Scheduler.MaxBlockPriceSatoshis != nil {
			next.Scheduler.MaxBlockPriceSatoshis = *body.Scheduler.MaxBlockPriceSatoshis
		}
		if body.Scheduler.PoolBlocks != nil {
			next.Scheduler.PoolBlocks = *body.Scheduler.PoolBlocks
		}
		if body.Scheduler.TotalSeats != nil {
			next.Scheduler.TotalSeats = *body.Scheduler.TotalSeats
		}
		if body.Scheduler.FloatingSeats != nil {
			next.Scheduler.FloatingSeats = *body.Scheduler.FloatingSeats
		}
		if body.Scheduler.RequestTimeoutMs != nil {
			next.Scheduler.RequestTimeoutMs = *body.Scheduler.RequestTimeoutMs
		}
	}
	if body.Server != nil {
		if body.Server.DefaultSeedPriceSatoshis != nil {
			next.Server.DefaultSeedPriceSatoshis = *body.Server.DefaultSeedPriceSatoshis
		}
		if body.Server.DefaultFullBlockPriceSatoshis != nil {
			next.Server.DefaultFullBlockPriceSatoshis = *body.Server.DefaultFullBlockPriceSatoshis
		}
		if body.Server.ArbiterPublicKeys != nil {
			next.Server.ArbiterPublicKeys = append([]string(nil), body.Server.ArbiterPublicKeys...)
		}
		if body.Server.Collector != nil {
			if body.Server.Collector.Collectors != nil {
				next.Server.Collector.Collectors = collectorEntriesToApp(body.Server.Collector.Collectors)
			}
			if body.Server.Collector.EarlyTriggerCount != nil {
				next.Server.Collector.EarlyTriggerCount = *body.Server.Collector.EarlyTriggerCount
			}
			if body.Server.Collector.SubmitIntervalMs != nil {
				next.Server.Collector.SubmitIntervalMs = *body.Server.Collector.SubmitIntervalMs
			}
			if body.Server.Collector.RetryBaseMs != nil {
				next.Server.Collector.RetryBaseMs = *body.Server.Collector.RetryBaseMs
			}
			if body.Server.Collector.RetryCapMs != nil {
				next.Server.Collector.RetryCapMs = *body.Server.Collector.RetryCapMs
			}
		}
	}
	return nil
}
