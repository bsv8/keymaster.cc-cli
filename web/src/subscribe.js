// 统一事件订阅入口：复用同一 APIProtocol 客户端，连接恢复时自动重新订阅。
// 订阅先快照后变化；实例变化丢弃旧连接响应与旧“已解锁”状态。
import { getClient, getConnectionStatus, getInstanceId, onConnectionStatus } from './client.js';

const watchers = new Set();

/** 取消当前绑定；旧连接关闭时取消帧失败可忽略。 */
function detach(w) {
  const ctrl = w.ctrl;
  w.ctrl = null;
  w.cancelSub = null;
  if (ctrl) {
    try { ctrl.abort(); } catch { /* ignore */ }
  }
}

/** 用当前客户端建立订阅；旧订阅先取消，避免重连后重复投递。 */
function attach(w) {
  detach(w);
  if (!watchers.has(w)) return;
  const ctrl = new AbortController();
  w.ctrl = ctrl;
  getClient().then((c) => {
    if (!watchers.has(w) || w.ctrl !== ctrl || ctrl.signal.aborted) return;
    const boundInstance = c.getInstanceId();
    w.cancelSub = c.subscribeEvents(w.topics, {
      onSnapshot: () => {
        if (watchers.has(w) && w.ctrl === ctrl && c.getInstanceId() === boundInstance) w.onDirty();
      },
      onEvent: () => {
        if (!watchers.has(w) || w.ctrl !== ctrl) return false;
        // 旧实例迟到事件丢弃：实例已变则忽略。
        if (c.getInstanceId() !== getInstanceId() || c.getInstanceId() !== boundInstance) return true;
        w.onDirty();
      },
      onResync: () => {
        if (!watchers.has(w) || w.ctrl !== ctrl) return;
        // resync 可能来自缺口，也可能服务端已关闭该订阅；重新订阅并取新快照
        // 才能保证后续事件继续到达。
        w.onDirty();
        attach(w);
      },
    }, { signal: ctrl.signal });
  }).catch(() => {
    /* 连接失败由 client.js 重连；下一次 connected 状态会重新 attach */
  });
}

onConnectionStatus((status) => {
  if (status === 'connected') {
    for (const w of watchers) attach(w);
  } else {
    for (const w of watchers) detach(w);
  }
});

/** 订阅主题变化，有变化或需重同步时调用 onDirty。返回取消函数。 */
export function watchTopics(topics, onDirty) {
  const w = { topics: [...topics], onDirty, ctrl: null, cancelSub: null };
  watchers.add(w);
  if (getConnectionStatus() === 'connected') attach(w);
  return () => {
    watchers.delete(w);
    detach(w);
  };
}
