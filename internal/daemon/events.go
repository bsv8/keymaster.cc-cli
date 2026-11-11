package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/events"
)

// 本文件实现统一事件订阅。消息总线（internal/events）只做分发；
// 快照构建与业务真值读取在这里完成，发布点在各业务提交成功后调用。
// 发布永不等待前端，更不阻塞交易与持久化。

// toMap 把协议 DTO 转为事件数据体。DTO 与线格式一致，转换是无损的。
func toMap(v any) map[string]any {
	raw, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// isKnownTopic 报告是否为协议定义的订阅主题。
func isKnownTopic(t string) bool {
	for _, k := range kmapi.KnownTopics {
		if k == t {
			return true
		}
	}
	return false
}

// parseTopics 解析 ?topics=a,b。省略表示全部；未知主题报错。
func parseTopics(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	for _, t := range strings.Split(raw, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !isKnownTopic(t) {
			return nil, fmt.Errorf("未知订阅主题: %q", t)
		}
		out = append(out, t)
	}
	return out, nil
}

func wants(topics []string, t string) bool {
	if len(topics) == 0 {
		return true
	}
	for _, x := range topics {
		if x == t {
			return true
		}
	}
	return false
}

// buildSnapshot 取得当前状态快照。调用者在订阅注册之后、发送快照之前
// 调用；注册期间暂存的变化在快照发送后按序重放，不遗漏（at-least-once，
// 接收方按全量状态幂等应用）。
func (d *Daemon) buildSnapshot(topics []string) (kmapi.SnapshotEvent, error) {
	if d.store == nil {
		return kmapi.SnapshotEvent{}, fmt.Errorf("尚未初始化")
	}
	snap := kmapi.SnapshotEvent{
		ProtocolVersion: events.ProtocolVersion,
		InstanceID:      d.events.InstanceID(),
	}
	var topicList []string
	revs := map[string]any{}
	state := map[string]any{}
	if wants(topics, kmapi.TopicConnection) {
		topicList = append(topicList, kmapi.TopicConnection)
		state[kmapi.TopicConnection] = map[string]any{
			"instanceId": d.events.InstanceID(),
			"product":    DaemonProduct,
			"apiVersion": DaemonAPIVersion,
		}
	}
	d.mu.Lock()
	daemonState := d.state
	d.mu.Unlock()
	if wants(topics, kmapi.TopicWallet) {
		topicList = append(topicList, kmapi.TopicWallet)
		state[kmapi.TopicWallet] = map[string]any{
			"state":  string(daemonState),
			"locked": daemonState != StateUnlocked,
		}
	}
	info, settings, err := d.store.Snapshot()
	if err != nil {
		return kmapi.SnapshotEvent{}, err
	}
	if wants(topics, kmapi.TopicRoots) {
		topicList = append(topicList, kmapi.TopicRoots)
		views := make([]kmapi.RootView, 0, len(info.Roots))
		for _, root := range info.Roots {
			accessible, errStr := true, ""
			if h, ok := d.roots.get(root.ID); ok {
				if h.err != "" {
					accessible, errStr = false, h.err
				} else if h.catalog == nil {
					accessible, errStr = false, "目录不可访问"
				}
			}
			views = append(views, kmapi.RootView{ID: root.ID, Path: root.Path, Accessible: accessible, Error: optStr(errStr)})
		}
		state[kmapi.TopicRoots] = toMap(kmapi.RootsView{
			Roots: views, DefaultDownloadRootID: reqStr(info.DefaultDownloadRootID),
			Revision: info.Revision,
		})
		revs[kmapi.TopicRoots] = info.Revision
	}
	if wants(topics, kmapi.TopicSettings) {
		topicList = append(topicList, kmapi.TopicSettings)
		state[kmapi.TopicSettings] = toMap(settingsDTO(settings))
		revs[kmapi.TopicSettings] = settings.Revision
	}
	if wants(topics, kmapi.TopicTask) {
		topicList = append(topicList, kmapi.TopicTask)
		tasks := []kmapi.Task{}
		if d.tasks != nil {
			for _, t := range d.tasks.list() {
				tasks = append(tasks, taskDTO(&t))
			}
		}
		state[kmapi.TopicTask] = map[string]any{"tasks": tasks}
	}
	if wants(topics, kmapi.TopicCollector) {
		topicList = append(topicList, kmapi.TopicCollector)
		state[kmapi.TopicCollector] = toMap(d.collectorStatusView())
	}
	if wants(topics, kmapi.TopicBalance) {
		topicList = append(topicList, kmapi.TopicBalance)
		state[kmapi.TopicBalance] = d.balanceState()
	}
	snap.Topics = topicList
	if len(revs) > 0 {
		snap.Revisions = revs
	}
	if len(state) > 0 {
		snap.State = state
	}
	return snap, nil
}



// publishTask 发布下载任务变化。调用者在任务持久化成功后调用。
func (d *Daemon) publishTask(t *Task) {
	if d.events == nil {
		return
	}
	d.events.Publish(kmapi.TopicTask, t.SeedHashHex, nil, toMap(taskDTO(t)))
}

// publishSettings 发布设置变化（PUT /api/settings、恢复出厂、采集器配置、
// 缺省售价调整都会使设置 revision 前进）。
func (d *Daemon) publishSettings(rev int64) {
	if d.events == nil {
		return
	}
	d.events.Publish(kmapi.TopicSettings, "", &rev, nil)
}

// publishRoots 发布售卖根变化。
func (d *Daemon) publishRoots(rootID string, rev int64) {
	if d.events == nil {
		return
	}
	d.events.Publish(kmapi.TopicRoots, rootID, &rev, nil)
}

// publishWallet 发布钱包锁定状态变化。
func (d *Daemon) publishWallet() {
	if d.events == nil {
		return
	}
	d.mu.Lock()
	state := d.state
	d.mu.Unlock()
	d.events.Publish(kmapi.TopicWallet, "", nil, map[string]any{
		"state":  string(state),
		"locked": state != StateUnlocked,
	})
}

// publishCollector 发布采集状态变化。
func (d *Daemon) publishCollector() {
	if d.events == nil {
		return
	}
	d.events.Publish(kmapi.TopicCollector, "", nil, toMap(d.collectorStatusView()))
}

// balanceState 返回余额快照态（供快照与发布共用）。
func (d *Daemon) balanceState() map[string]any {
	out := map[string]any{}
	bal, cached, queryErr := d.queryBalance(context.Background())
	if queryErr != nil {
		out["error"] = queryErr.Error()
		if cached != nil {
			out["totalSats"] = cached.Total
			out["availableSats"] = cached.Available
			out["allocatedSats"] = cached.Allocated
			out["queryTime"] = cached.QueryTime
			out["cached"] = true
		} else {
			out["cached"] = false
		}
		return out
	}
	out["totalSats"] = bal.Total
	out["availableSats"] = bal.Available
	out["allocatedSats"] = bal.Allocated
	out["queryTime"] = bal.QueryTime
	out["cached"] = false
	return out
}
