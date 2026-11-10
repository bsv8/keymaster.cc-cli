// Package events 是后台统一消息总线：只做事件分发，不持有业务真值。
//
// 业务模块在状态提交成功后发布事件；发布永不等待前端处理，慢订阅者
// 不能阻塞钱包、数据库或交易执行。每订阅者有界队列，溢出时要求重新
// 同步（resync），不静默丢弃关键状态。
//
// 事件包格式见协议 SDK（kmapi.EventEnvelope）：协议版本、后台实例 ID、
// 序号、主题、实体 ID、实体 revision、数据。快照构建与业务真值读取由
// 后台负责，本包只保证“先订阅再取快照”期间的变化不遗漏（at-least-once
// 重放缓冲，接收方按全量状态幂等应用）。
package events

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
)

// ProtocolVersion 与协议 SDK 的事件版本一致。
const ProtocolVersion = "v1"

// QueueSize 是每订阅者有界队列长度。
const QueueSize = 128

// Subscriber 是一路订阅：缓冲其订阅期间的变化，溢出后要求重同步。
type Subscriber struct {
	id      uint64
	topics  map[string]bool // nil 表示全部主题
	ch      chan kmapi.EventEnvelope
	seq     int64 // 已投递的最大序号（快照衔接用）
	mu      sync.Mutex
	dropped bool
	closed  bool
}

// Chan 返回事件通道；调用者按序号顺序消费。
func (s *Subscriber) Chan() <-chan kmapi.EventEnvelope { return s.ch }

// Dropped 报告队列是否溢出；溢出后必须重新同步快照。
func (s *Subscriber) Dropped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Wants 报告是否订阅该主题。
func (s *Subscriber) Wants(topic string) bool {
	return s.topics == nil || s.topics[topic]
}

// Broker 分发事件。零值不可用，请用 New。
type Broker struct {
	instanceID string
	seq        atomic.Int64
	mu         sync.Mutex
	subs       map[uint64]*Subscriber
	next       uint64
	closed     bool
}

// New 创建总线并生成后台实例 ID。
func New() *Broker {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return &Broker{
		instanceID: "b_" + hex.EncodeToString(b[:]),
		subs:       map[uint64]*Subscriber{},
	}
}

// InstanceID 返回后台实例标识，用于识别重启与实例变化。
func (b *Broker) InstanceID() string { return b.instanceID }

// Seq 返回当前最大序号。
func (b *Broker) Seq() int64 { return b.seq.Load() }

// Subscribe 注册订阅并开始暂存变化。topics 为空表示全部主题。
func (b *Broker) Subscribe(topics []string) *Subscriber {
	var want map[string]bool
	if len(topics) > 0 {
		want = map[string]bool{}
		for _, t := range topics {
			want[t] = true
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	sub := &Subscriber{id: b.next, topics: want, ch: make(chan kmapi.EventEnvelope, QueueSize)}
	if b.closed {
		close(sub.ch)
		sub.closed = true
		return sub
	}
	b.subs[sub.id] = sub
	return sub
}

// Unsubscribe 清理订阅资源。
func (b *Broker) Unsubscribe(sub *Subscriber) {
	if sub == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if cur, ok := b.subs[sub.id]; ok && cur == sub {
		delete(b.subs, sub.id)
	}
	sub.mu.Lock()
	if !sub.closed {
		sub.closed = true
		close(sub.ch)
	}
	sub.mu.Unlock()
}

// Close 关闭总线并清理全部订阅。
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, sub := range b.subs {
		delete(b.subs, id)
		sub.mu.Lock()
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
		sub.mu.Unlock()
	}
}

// Publish 发布事件。永不阻塞：慢订阅者队列满时标记溢出并关闭其通道，
// 由订阅循环发送 resync。调用者应在业务提交成功后调用，传入已准备好的
// 数据（本函数只做浅拷贝分发，不读业务锁）。
func (b *Broker) Publish(topic, entityID string, revision *int64, data map[string]any) {
	seq := b.seq.Add(1)
	env := kmapi.EventEnvelope{
		ProtocolVersion: ProtocolVersion,
		InstanceID:      b.instanceID,
		Seq:             seq,
		Topic:           topic,
		Data:            data,
	}
	if entityID != "" {
		env.EntityID = &entityID
	}
	if revision != nil {
		v := *revision
		env.Revision = &v
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sub := range b.subs {
		if !sub.Wants(topic) {
			continue
		}
		sub.mu.Lock()
		if sub.closed || sub.dropped {
			sub.mu.Unlock()
			continue
		}
		select {
		case sub.ch <- env:
			if seq > sub.seq {
				sub.seq = seq
			}
		default:
			sub.dropped = true
			if !sub.closed {
				sub.closed = true
				close(sub.ch)
			}
		}
		sub.mu.Unlock()
	}
}
