// CLI Web 官方 Receiver：接收外部 App 请求，经 APIProtocol 调用后台 Connect 模块。
// WS 连接本身即 CLI Web 来源（同源 /api/ws），不与外部 App 来源混淆；
// 后台核验 App 身份、授权绑定与运行代际。实现对照官方 Receiver 类型：
// peer/lifecycle/identity 由宿主提供，取消只通知宿主，回复只发给当前有效绑定。
import { KeymasterConnectReceiver } from 'keymaster-connect/server';
import { getClient, getInstanceId } from './client.js';

const pendingApproval = new Map(); // approvalId -> { approvalId, ctx, method, origin, reqId }
const inflight = new Map(); // Receiver 请求 id -> { abort: AbortController }
const authzByOrigin = new Map(); // origin -> authorizationId（登录成功后绑定）

let receiver = null;
let cachedGeneration = '';

function toArrayBuffer(u8) {
  if (u8 instanceof ArrayBuffer) return u8.slice(0);
  const copy = new Uint8Array(u8.length);
  copy.set(u8);
  return copy.buffer;
}

function toBinaryField(u8, mime) {
  const field = { $type: 'binary', bytes: toArrayBuffer(u8) };
  if (mime) field.mime = mime;
  return field;
}

function bytesOf(value) {
  if (!value) return null;
  if (value instanceof Uint8Array) return value;
  if (value instanceof ArrayBuffer) return new Uint8Array(value);
  if (value && value.$type === 'binary' && value.bytes) return bytesOf(value.bytes);
  return null;
}

export function getPendingApprovals() {
  return pendingApproval;
}

export async function initConnectReceiver(onApproval) {
  if (receiver) return receiver;
  receiver = new KeymasterConnectReceiver({
    // 宿主提供 peer 来源策略：popup 由 opener 识别；无 opener 时拒绝外来窗口。
    peer: () => (window.opener ? { mode: 'popup', source: window.opener } : { mode: 'popup', source: null }),
    // 世代快照来自后台实例标识；后台重启后旧授权/绑定失效，需重新登录。
    lifecycle: () => ({ walletGeneration: cachedGeneration, runGeneration: getInstanceId() || cachedGeneration }),
    // 首次建立连接时读取真实钱包身份；不得从请求参数取值。
    identity: async () => {
      const c = await getClient();
      const st = await c.status();
      if (!st.initialized || !st.network || !st.publicKey) throw new Error('钱包未就绪');
      return { bsvNetwork: st.network, publicKeyHex: st.publicKey };
    },
    send: (target, origin, message) => target.postMessage(message, origin),
  });
  window.addEventListener('message', async (ev) => {
    const msg = { source: ev.source, origin: ev.origin, data: ev.data };
    let req = null;
    try {
      if (!receiver.isAllowedPeer(msg)) return;
      const cancelled = receiver.acceptCancel(msg);
      if (cancelled) {
        const inf = inflight.get(cancelled.id);
        if (inf) {
          try { inf.abort.abort(); } catch { /* ignore */ }
          inflight.delete(cancelled.id);
        }
        const found = [...pendingApproval.entries()].find(([, p]) => p.reqId === cancelled.id);
        if (found) {
          const [approvalId] = found;
          try {
            const c = await getClient();
            // 由后台 Decide 原子决定取消/批准谁先到；成功后才删除本地映射。
            await c.call(undefined, 'connect.approvals.decide', { approvalId, decision: 'cancel' });
            pendingApproval.delete(approvalId);
          } catch (e) {
            // 审批已不存在（已执行/已清理）时可安全丢弃本地映射；其他错误保留，
            // 避免本地认为已取消而后台待办仍可批准。
            if (e?.code === 'NOT_FOUND') pendingApproval.delete(approvalId);
          } finally {
            // 回收 Receiver 的在途上下文；App 已中止本地等待，结果帧被忽略。
            try { receiver.replyError(cancelled, { code: 'user_rejected', message: 'App 已取消请求' }); } catch { /* ignore */ }
          }
        }
        return;
      }
      req = await receiver.accept(msg);
    } catch {
      // 解析/校验失败：按官方语义忽略首条非法请求，不回复。
      return;
    }
    if (!req) return;
    handleRequest(req).catch((e) => {
      try {
        receiver.replyError(req, { code: 'internal_error', message: String(e?.message || e) });
      } catch { /* ignore */ }
      inflight.delete(req.id);
    });
  });
  return receiver;
}

function sdkError(method, perr) {
  const code = perr?.code || 'internal_error';
  const message = perr?.message || 'Internal error';
  if (method.startsWith('storage.')) {
    switch (code) {
      case 'NOT_FOUND': return { code: 'storage_not_found', message };
      case 'CONFLICT': return { code: 'storage_conflict', message };
      case 'TOO_LARGE': return { code: 'storage_limit_exceeded', message };
      case 'BAD_REQUEST':
        return message.includes('路径')
          ? { code: 'storage_invalid_path', message }
          : { code: 'invalid_request', message };
      case 'LOCKED':
      case 'STOPPING': return { code: 'storage_unavailable', message };
      case 'NOT_INITIALIZED': return { code: 'storage_not_configured', message };
      case 'FORBIDDEN': return { code: 'storage_forbidden', message };
      case 'VERSION_MISMATCH': return { code: 'invalid_request', message };
      default: return { code: 'internal_error', message };
    }
  }
  if (method.startsWith('cipher.')) {
    if (code === 'BAD_REQUEST' && message === 'Decrypt failed') return { code: 'decrypt_failed', message };
    switch (code) {
      case 'BAD_REQUEST': return { code: 'invalid_request', message };
      case 'LOCKED': return { code: 'active_key_unavailable', message };
      case 'NOT_INITIALIZED': return { code: 'internal_error', message };
      case 'FORBIDDEN': return { code: 'unsupported_scope', message };
      case 'NOT_FOUND': return { code: 'internal_error', message };
      default: return { code: 'internal_error', message };
    }
  }
  if (method === 'payment.pay' || method === 'payment.status') {
    switch (code) {
      case 'BAD_REQUEST': return { code: 'invalid_request', message };
      case 'NOT_FOUND':
        return method === 'payment.status'
          ? { code: 'payment_not_found', message }
          : { code: 'invalid_request', message };
      case 'FORBIDDEN': return { code: 'unsupported_scope', message };
      case 'LOCKED': return { code: 'active_key_unavailable', message };
      case 'STATE_CONFLICT':
        if (message.includes('内容不同')) return { code: 'id_conflict', message };
        if (message.includes('过多')) return { code: 'payment_pending_limit', message };
        return { code: 'id_conflict', message };
      default: return { code: 'internal_error', message };
    }
  }
  switch (code) {
    case 'BAD_REQUEST': return { code: 'invalid_request', message };
    case 'NOT_FOUND': return { code: 'invalid_request', message };
    case 'FORBIDDEN': return { code: 'unsupported_scope', message };
    default: return { code: 'internal_error', message };
  }
}

function isEstablishing(method) {
  return method === 'connect.login' || method === 'connect.resume' || method === 'connect.launch';
}

async function handleRequest(req) {
  const abort = new AbortController();
  inflight.set(req.id, { abort });
  const signal = abort.signal;
  try {
    const client = await getClient();
    const method = req.method;
    const origin = req.origin;
    const params = { ...(req.params || {}) };

    if (isEstablishing(method)) {
      // 已持有授权的 reconnect 心跳：官方 Receiver 把 reconnect 转成 connect.resume
      // 并携带 authorizationId；宿主只复核既有授权，不重复审批、不新建授权。
      if (method === 'connect.resume' && req.authorizationId) {
        try {
          const resumed = await client.call({ signal }, 'connect.request.exec', {
            method, origin, authorizationId: req.authorizationId,
          });
          receiver.replyResult(req, resumed);
        } catch (e) {
          const stale = e?.code === 'NOT_FOUND' || e?.code === 'BAD_REQUEST';
          if (stale) {
            // 与官方钱包一致：失效授权要显式收回，App 才能停止心跳并重新登录。
            try {
              req.source.postMessage({ v: 1, type: 'revoked', authorizationId: req.authorizationId }, origin);
            } catch { /* 窗口关闭时静默 */ }
          }
          const code = stale ? 'connection_expired' : 'internal_error';
          receiver.replyError(req, { code, message: e?.message || '恢复连接失败，请重新登录' });
        }
        return;
      }
      const proof = params.appIdentity;
      if (!proof) {
        receiver.replyError(req, { code: 'invalid_request', message: '缺少 App 身份证明' });
        return;
      }
      const res = await client.call({ signal }, 'connect.request.exec', {
        method, params, proof, origin,
      });
      if (res && res.approvalId) {
        pendingApproval.set(res.approvalId, { approvalId: res.approvalId, ctx: req, method, origin, reqId: req.id });
      } else if (res) {
        receiver.replyResult(req, res);
      }
      return;
    }

    if (method === 'connect.logout') {
      const authorizationId = authzByOrigin.get(origin);
      if (!authorizationId) {
        receiver.replyError(req, { code: 'invalid_request', message: '无有效授权' });
        return;
      }
      const res = await client.call({ signal }, 'connect.request.exec', { method, origin, authorizationId });
      authzByOrigin.delete(origin);
      receiver.replyResult(req, res);
      return;
    }

    // 其余业务方法：必须持有有效授权（登录时按来源绑定），不接受裸 proof。
    const authorizationId = authzByOrigin.get(origin);
    if (!authorizationId) {
      const code = method.startsWith('storage.') ? 'storage_identity_required' : 'identity_required';
      receiver.replyError(req, { code, message: '需要先登录授权' });
      return;
    }

    // 提取二进制附件（BinaryField 转为 WS 二进制帧），JSON 参数中移除原始字节。
    const attachments = {};
    const takeBinary = (holder, key, name) => {
      const b = bytesOf(holder?.[key]);
      if (b) {
        attachments[name] = b instanceof Uint8Array ? b : new Uint8Array(b);
        delete holder[key];
      }
      return b;
    };
    if (method === 'storage.put' || method === 'storage.encrypted.put') {
      takeBinary(params, 'content', 'content');
    } else if (method === 'cipher.encrypt') {
      takeBinary(params, 'content', 'content');
    } else if (method === 'cipher.decrypt') {
      takeBinary(params, 'nonce', 'nonce');
      takeBinary(params, 'cipherbytes', 'cipherbytes');
    }

    const execParams = { method, params, origin, authorizationId };
    // 始终经 callWithBinary：服务端可能在响应中附带二进制（get/decrypt），
    // 普通 call 会丢弃这些附件并导致后续转换失败。
    const raw = await client.callWithBinary({ signal }, 'connect.request.exec', execParams, attachments);
    // payment.pay 冻结后返回 approvalId，需经用户批准后认领执行；其他业务直接返回结果。
    if (raw && raw.result && raw.result.approvalId) {
      pendingApproval.set(raw.result.approvalId, { approvalId: raw.result.approvalId, ctx: req, method, origin, reqId: req.id });
      return;
    }
    receiver.replyResult(req, toSdkResult(method, raw.result, raw.binaries || {}));
  } catch (e) {
    if (signal.aborted) return;
    const perr = e?.code && e?.message ? { code: e.code, message: e.message } : null;
    const method = req.method || '';
    if (perr && (perr.code === 'LOCKED' || perr.code === 'STOPPING' || perr.code === 'NOT_INITIALIZED' || perr.code === 'VERSION_MISMATCH')) {
      receiver.replyError(req, sdkError(method, perr));
    } else if (e instanceof Error && e.name === 'TransportError') {
      receiver.replyError(req, { code: 'internal_error', message: '后台连接失败' });
    } else if (perr) {
      receiver.replyError(req, sdkError(method, perr));
    } else {
      receiver.replyError(req, { code: 'internal_error', message: String(e?.message || e) });
    }
  } finally {
    inflight.delete(req.id);
  }
}

function toSdkResult(method, result, binaries) {
  const bf = (name, mime) => {
    const b = binaries[name];
    if (!b) return undefined;
    return toBinaryField(b, mime);
  };
  switch (method) {
    case 'storage.put':
    case 'storage.encrypted.put':
      return {
        path: result.path,
        size: result.size,
        ...(result.revision !== undefined ? { revision: String(result.revision) } : {}),
        updatedAt: result.updatedAt,
      };
    case 'storage.get':
    case 'storage.encrypted.get': {
      const content = bf('content', result.contentType);
      if (!content) throw new Error('后台未返回内容');
      return {
        path: result.path,
        content,
        ...(result.contentType ? { contentType: result.contentType } : {}),
        offset: result.offset ?? 0,
        totalSize: result.totalSize ?? 0,
        eof: result.eof ?? true,
        ...(result.revision !== undefined ? { revision: String(result.revision) } : {}),
        ...(result.lastModified ? { lastModified: result.lastModified } : {}),
      };
    }
    case 'storage.list':
      return result;
    case 'storage.delete':
      return result;
    case 'cipher.encrypt': {
      const nonce = bf('nonce');
      const cipherbytes = bf('cipherbytes');
      if (!nonce || !cipherbytes) throw new Error('后台未返回密文');
      return { nonce, cipherbytes };
    }
    case 'cipher.decrypt': {
      const content = bf('content');
      if (!content) throw new Error('后台未返回明文');
      return { content, contentType: result.contentType || '' };
    }
    default:
      return result;
  }
}

// 审批决定后由审批面板调用：决定只做终态转换；批准后取回执行结果并定向回复 App。
export async function decideApproval(approvalId, decision) {
  const client = await getClient();
  const entry = pendingApproval.get(approvalId);
  const res = await client.call(undefined, 'connect.approvals.decide', { approvalId, decision });
  if (!entry) return res;
  pendingApproval.delete(approvalId);
  try {
    if (decision === 'approve' && res?.status === 'approved') {
      const method = entry.method;
      const exec = await client.callWithBinary(undefined, 'connect.request.exec', { approvalId }, {});
      receiver.replyResult(entry.ctx, toSdkResult(method, exec.result, exec.binaries || {}));
      if (method === 'connect.login' || method === 'connect.resume' || method === 'connect.launch') {
        const authorizationId = exec.result?.authorizationId;
        if (authorizationId) authzByOrigin.set(entry.origin, authorizationId);
      }
      if (method === 'connect.logout') authzByOrigin.delete(entry.origin);
    } else {
      receiver.replyError(entry.ctx, { code: 'user_rejected', message: '用户拒绝' });
    }
  } catch (e) {
    const perr = e?.code && e?.message ? { code: e.code, message: e.message } : null;
    try {
      receiver.replyError(entry.ctx, perr ? sdkError(entry.method, perr) : { code: 'internal_error', message: String(e?.message || e) });
    } catch { /* ignore */ }
  }
  return res;
}

export function noteConnected(instanceId) {
  cachedGeneration = instanceId || '';
}
