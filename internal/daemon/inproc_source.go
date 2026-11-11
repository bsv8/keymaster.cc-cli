package daemon

import (
	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
)

// SubscribeEvents 为进程内订阅提供快照与事件流，与 WS 的 subscribe/snapshot/event 对等。
func (d *Daemon) SubscribeEvents(topics []string) (kmapi.SnapshotEvent, <-chan kmapi.EventEnvelope, func(), error) {
	for _, t := range topics {
		if !isKnownTopic(t) {
			return kmapi.SnapshotEvent{}, nil, nil, &kmapi.ErrorDetail{Code: kmapi.ErrBadRequest, Message: "未知订阅主题"}
		}
	}
	if d.store == nil {
		return kmapi.SnapshotEvent{}, nil, nil, &kmapi.ErrorDetail{Code: kmapi.ErrNotInitialized, Message: "尚未初始化"}
	}
	sub := d.events.Subscribe(topics)
	snap, err := d.buildSnapshot(topics)
	if err != nil {
		d.events.Unsubscribe(sub)
		return kmapi.SnapshotEvent{}, nil, nil, &kmapi.ErrorDetail{Code: kmapi.ErrInternal, Message: err.Error()}
	}
	return snap, sub.Chan(), func() { d.events.Unsubscribe(sub) }, nil
}

// Ensure InprocClient 可用：Daemon 同时实现 Dispatcher 与 EventSource。
var (
	_ kmapi.Dispatcher  = (*Daemon)(nil)
	_ kmapi.EventSource = (*Daemon)(nil)
)
