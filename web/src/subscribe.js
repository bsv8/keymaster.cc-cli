// 统一事件订阅入口：与 CLI/MyGo 共用同一份后台事件契约
//（keymaster-api-protocol），替代各页面的重复轮询。
//
// 订阅在连接建立即开始服务暂存；随后的 load() 反映更新状态，
// 暂存的变化以 change 形式重放后触发 reload，不遗漏（at-least-once，
// reload 幂等）。慢消费者溢出或实例变化时服务端发 resync，
// 这里按重载处理，由调用者重新进入稳态。
import { subscribeEvents } from 'keymaster-api-protocol';

/** 订阅主题变化，有变化或需重同步时调用 onDirty。返回取消函数。 */
export function watchTopics(topics, onDirty) {
  let disposed = false;
  const ctrl = new AbortController();
  subscribeEvents('', topics, {
    onSnapshot: () => {},
    onChange: () => {
      if (!disposed) onDirty();
    },
    onResync: () => {
      if (!disposed) onDirty();
      return false;
    },
  }, { signal: ctrl.signal }).catch(() => {});
  return () => {
    disposed = true;
    ctrl.abort();
  };
}
