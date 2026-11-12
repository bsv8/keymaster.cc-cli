// CLI Web 官方 Receiver：接收外部 App 请求，经 APIProtocol 调用后台 Connect 模块。
// WS 连接本身即 CLI Web 来源（同源 /api/ws），不与外部 App 来源混淆；
// 后台核验 App 身份、授权绑定与运行代际。实现对照官方 Receiver 类型：
// peer/lifecycle/identity 由宿主提供，取消只通知宿主，回复只发给当前有效绑定。
import { KeymasterConnectReceiver } from 'keymaster-connect/server';
import { getClient, getInstanceId } from './client.js';

const pendingApproval = new Map(); // approvalId -> { approvalId, ctx, method, origin, reqId, receiver }
// 授权与在途请求按 Receiver（具体连接上下文）隔离：同源多连接不得串用。
// popup 有独立单例 Receiver；钱包启动的每个窗口/iframe 各有一个官方 Receiver。
const inflightByReceiver = new Map(); // Receiver -> Map<reqId, { abort: AbortController }>
const authzByReceiver = new Map(); // Receiver -> Map<origin, authorizationId>（登录成功后绑定）

let receiver = null;
let cachedGeneration = '';
// popup 对端引用在对端关闭后可能直接消失：初始化时捕获，丢失即视为关闭。
let popupOpenerAtInit = null;
try { popupOpenerAtInit = window.opener || null; } catch { popupOpenerAtInit = null; }
// 钱包主动启动的应用：每个启动上下文一个官方 Receiver（appView 模式）。
// 窗口方式 source 为预留的 child window；iframe 方式为 iframe contentWindow。
// 窗口关闭、卸载或后台重启后由钱包整体生命周期 teardown 丢弃旧上下文，
// 旧启动上下文不得继续建立连接。
const launchedApps = new Map(); // handle -> { receiver, holder }
let launchedSeq = 0;

function inflightOf(recv) {
  let m = inflightByReceiver.get(recv);
  if (!m) { m = new Map(); inflightByReceiver.set(recv, m); }
  return m;
}

function authzOf(recv) {
  let m = authzByReceiver.get(recv);
  if (!m) { m = new Map(); authzByReceiver.set(recv, m); }
  return m;
}

function isReceiverAlive(recv) {
  if (!recv) return false;
  if (recv === receiver) return true;
  return [...launchedApps.values()].some((app) => app.receiver === recv);
}

// 认领执行前核对原请求上下文、连接世代与实际来源仍有效：
// 注册表存续、来源窗口未关闭、官方 Receiver 权威校验（pending 持有、
// 世代一致、绑定有效）三者缺一即视为失效。popup teardown 后 clear() 已丢弃
// 协议运行态旧上下文无法再通过校验。
function isContextAlive(entry) {
  const recv = (entry && entry.receiver) || receiver;
  if (!recv || !entry || !entry.ctx) return false;
  const registered = recv === receiver || [...launchedApps.values()].some((app) => app.receiver === recv);
  if (!registered) return false;
  try {
    const src = entry.ctx.source;
    if (!src || src.closed) return false;
  } catch {
    return false;
  }
  try {
    recv.assertCurrent(entry.ctx);
  } catch {
    return false;
  }
  return true;
}

// 丢弃某连接的本地待审批映射；后台待办尽力取消（已终态的不强行回滚）。
// 已进入不可逆阶段的业务仍按后台原有终态处理，不宣称取消成功。
function dropPendingForReceiver(recv) {
  const ids = [...pendingApproval.entries()].filter(([, p]) => p.receiver === recv).map(([id]) => id);
  for (const approvalId of ids) pendingApproval.delete(approvalId);
  if (ids.length === 0) return;
  getClient().then((c) => Promise.allSettled(
    ids.map((approvalId) => c.call(undefined, 'connect.approvals.decide', { approvalId, decision: 'cancel' })),
  )).catch(() => { /* 卸载/断连时尽力而为，后台以其终态为准 */ });
}

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

function receiverLifecycle() {
  return { walletGeneration: cachedGeneration, runGeneration: getInstanceId() || cachedGeneration };
}

async function receiverIdentity() {
  const c = await getClient();
  const st = await c.status();
  if (!st.initialized || !st.network || !st.publicKey) throw new Error('钱包未就绪');
  return { bsvNetwork: st.network, publicKeyHex: st.publicKey };
}

function receiverSend(target, origin, message) {
  target.postMessage(message, origin);
}

// 钱包主动启动：向后台签发一次性令牌（钱包管理上下文，需已初始化并解锁）。
// 前端不能自行生成授权；后台签发时独立解析应用身份证明并绑定发布者、应用标识、
// 身份摘要与许可范围，消费时 proof 必须与之完全一致。
export async function issueLaunchToken(origin) {
  const c = await getClient();
  return c.call(undefined, 'connect.launch.issue', { origin });
}

// 登记钱包启动的应用上下文。origin 为应用精确来源；getSource 返回
// 预留窗口（窗口方式）或 iframe contentWindow（页内方式），就绪前可为 null；
// kind 区分 'window'（独立窗口，页面导航不得误终止）与 'iframe'（随展示关闭）。
// 返回句柄；其生命周期归钱包整体管理，见 teardownLaunchedApp。
export function registerLaunchedApp({ origin, getSource, kind = 'window', child = null }) {
  const holder = { origin, getSource, kind, child };
  const appReceiver = new KeymasterConnectReceiver({
    peer: () => ({ mode: 'appView', origin: holder.origin, source: holder.getSource() || null }),
    lifecycle: receiverLifecycle,
    identity: receiverIdentity,
    send: receiverSend,
  });
  const handle = `launched-${++launchedSeq}`;
  launchedApps.set(handle, { receiver: appReceiver, holder });
  return handle;
}

// 关闭/卸载/后台重启时处理启动连接：中止该连接的在途请求、取消该连接尚未批准的
// 后台待办（已终态的不强行回滚）、尽力发送官方 closing 通知、丢弃协议运行态与本地授权映射，
// 而不是只从 Map 删除入口后任其超时，更不让已关闭连接的待办继续执行。
// 退出（logout）只撤销该连接实际持有的授权；此处不主动撤销后台授权，
// 后续重建连接按同 App 同来源替换旧授权，后台重启则整批失效。
export function teardownLaunchedApp(handle, { notify = true } = {}) {
  const app = launchedApps.get(handle);
  if (!app) return false;
  launchedApps.delete(handle);
  const recv = app.receiver;
  let source = null;
  try { source = app.holder.getSource() || null; } catch { source = null; }
  const inflight = inflightByReceiver.get(recv);
  if (inflight) {
    for (const entry of inflight.values()) {
      try { entry.abort.abort(); } catch { /* ignore */ }
    }
  }
  inflightByReceiver.delete(recv);
  authzByReceiver.delete(recv);
  dropPendingForReceiver(recv);
  try {
    if (notify && source && !source.closed) recv.sendClosing(source);
  } catch { /* 窗口退出不重试推送。 */ }
  try { recv.clear(); } catch { /* ignore */ }
  return true;
}

// 定期收殓已关闭的启动窗口：与显式关闭同等的完整 teardown。
// iframe 随展示关闭由展示方 teardown；页面导航不得误终止独立窗口。
export function sweepClosedLaunchedApps() {
  for (const [handle, app] of [...launchedApps]) {
    if (app.holder.kind !== 'window') continue;
    let closed = false;
    try { closed = !app.holder.child || app.holder.child.closed; } catch { closed = true; }
    if (closed) teardownLaunchedApp(handle);
  }
}

// 定期收殓传输对端已关闭的 popup 连接：应用页关闭后钱包侧不再持有其待办，
// 与显式关闭同等的完整 teardown（含取消该连接后台待办）。
// 直接打开的钱包没有 popup 对端，不处理；独立窗口由启动 sweeper 另行收殓。
export function sweepClosedPopup() {
  if (!receiver) return;
  let opener = null;
  try { opener = window.opener || null; } catch { opener = null; }
  if (!opener) {
    // 对端关闭后引用可能直接消失：初始化时存在现已丢失即视为关闭。
    if (!popupOpenerAtInit) return;
    teardownPopupReceiver();
    popupOpenerAtInit = null;
    return;
  }
  let closed = false;
  try { closed = opener.closed; } catch { closed = true; }
  if (closed) teardownPopupReceiver();
}

// 卸载时处理全部启动连接（尽力通知）；调用方负责在途页面逻辑。
export function teardownAllLaunchedApps() {
  for (const handle of [...launchedApps.keys()]) teardownLaunchedApp(handle);
}

// popup 单例 Receiver 的运行期清理：后台重启等场景下旧绑定与授权一律失效，
// 由后续 resume 心跳或重新登录重建，不沿用旧映射；该连接尚未批准的后台待办一并取消。
export function teardownPopupReceiver({ notify = true } = {}) {
  if (!receiver) return;
  const inflight = inflightByReceiver.get(receiver);
  if (inflight) {
    for (const entry of inflight.values()) {
      try { entry.abort.abort(); } catch { /* ignore */ }
    }
  }
  inflightByReceiver.delete(receiver);
  authzByReceiver.delete(receiver);
  dropPendingForReceiver(receiver);
  try {
    if (notify && window.opener && !window.opener.closed) receiver.sendClosing(window.opener);
  } catch { /* ignore */ }
  try { receiver.clear(); } catch { /* ignore */ }
}

async function handleCancel(recv, cancelled) {
  const inflight = inflightByReceiver.get(recv);
  const inf = inflight?.get(cancelled.id);
  if (inf) {
    try { inf.abort.abort(); } catch { /* ignore */ }
    inflight.delete(cancelled.id);
  }
  // 取消只作用于同一连接的待审批；同源他连接的审批不受影响。
  const found = [...pendingApproval.entries()].find(([, p]) => p.reqId === cancelled.id && p.receiver === recv);
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
      try { recv.replyError(cancelled, { code: 'user_rejected', message: 'App 已取消请求' }); } catch { /* ignore */ }
    }
  }
}

export async function initConnectReceiver(onApproval) {
  if (receiver) return receiver;
  receiver = new KeymasterConnectReceiver({
    // 宿主提供 peer 来源策略：popup 由 opener 识别；无 opener 时拒绝外来窗口。
    peer: () => (window.opener ? { mode: 'popup', source: window.opener } : { mode: 'popup', source: null }),
    // 世代快照来自后台实例标识；后台重启后旧授权/绑定失效，需重新登录。
    lifecycle: receiverLifecycle,
    // 首次建立连接时读取真实钱包身份；不得从请求参数取值。
    identity: receiverIdentity,
    send: receiverSend,
  });
  window.addEventListener('message', async (ev) => {
    const msg = { source: ev.source, origin: ev.origin, data: ev.data };
    // popup（Demo 主动打开钱包）：保持原有语义，与启动应用互不干扰。
    try {
      if (receiver.isAllowedPeer(msg)) {
        const cancelled = receiver.acceptCancel(msg);
        if (cancelled) {
          await handleCancel(receiver, cancelled);
          return;
        }
        let req = null;
        try {
          req = await receiver.accept(msg);
        } catch {
          // 解析/校验失败：按官方语义忽略首条非法请求，不回复。
          return;
        }
        if (!req) return;
        handleRequest(req, receiver).catch((e) => {
          try {
            receiver.replyError(req, { code: 'internal_error', message: String(e?.message || e) });
          } catch { /* ignore */ }
          inflightByReceiver.get(receiver)?.delete(req.id);
        });
        return;
      }
    } catch {
      return;
    }
    // 钱包主动启动的应用（窗口/iframe）：按登记的精确来源逐个匹配；
    // 首个来源命中的 Receiver 独占处理，避免重复执行。
    for (const app of launchedApps.values()) {
      let allowed = false;
      try { allowed = app.receiver.isAllowedPeer(msg); } catch { allowed = false; }
      if (!allowed) continue;
      try {
        // appView 首条合法 ready 认领来源；无效版本和外来窗口不改变绑定。
        try { app.receiver.acceptReady(msg); } catch { /* ignore */ }
        const cancelled = app.receiver.acceptCancel(msg);
        if (cancelled) {
          await handleCancel(app.receiver, cancelled);
          return;
        }
        let req = null;
        try {
          req = await app.receiver.accept(msg);
        } catch {
          return;
        }
        if (!req) return;
        handleRequest(req, app.receiver).catch((e) => {
          try {
            app.receiver.replyError(req, { code: 'internal_error', message: String(e?.message || e) });
          } catch { /* ignore */ }
          inflightByReceiver.get(app.receiver)?.delete(req.id);
        });
      } catch { /* ignore */ }
      return;
    }
  });
  return receiver;
}

function sdkError(method, perr) {
  const code = perr?.code || 'internal_error';
  const message = perr?.message || 'Internal error';
  // 授权本身失效（非业务对象缺失）：统一回 authorization_expired，调用方按恢复语义处理。
  if ((code === 'NOT_FOUND' || code === 'BAD_REQUEST') && message.includes('授权')) {
    return { code: 'authorization_expired', message };
  }
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

async function handleRequest(req, recv) {
  const abort = new AbortController();
  inflightOf(recv).set(req.id, { abort });
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
          // 刷新恢复附带当前入口 proof（如有），由后台核对原授权绑定身份；心跳无 proof 时按原授权恢复。
          const resumeProof = req.params?.appIdentity;
          const resumed = await client.call({ signal }, 'connect.request.exec', {
            method, origin, authorizationId: req.authorizationId,
            ...(resumeProof ? { proof: resumeProof } : {}),
          });
          // 同一连接的心跳/刷新恢复成功：沿用原授权，本连接继续持有，不新建映射到他处。
          // 刷新后的新连接先由后台验证记录中的授权，再建立新映射，不要求新 Receiver 事先已有映射。
          authzOf(recv).set(origin, req.authorizationId);
          recv.replyResult(req, resumed);
        } catch (e) {
          const message = e?.message || '恢复连接失败，请重新登录';
          // 授权已不存在/过期/代际失效：显式收回并回 authorization_expired，调用方清除本地记录。
          // proof 无效或身份不匹配：回 invalid_request，保留原记录，不创建新授权。
          if (e?.code === 'NOT_FOUND') {
            try {
              req.source.postMessage({ v: 1, type: 'revoked', authorizationId: req.authorizationId }, origin);
            } catch { /* 窗口关闭时静默 */ }
            recv.replyError(req, { code: 'authorization_expired', message });
          } else if (e?.code === 'BAD_REQUEST') {
            recv.replyError(req, { code: 'invalid_request', message });
          } else {
            recv.replyError(req, { code: 'internal_error', message });
          }
        }
        return;
      }
      const proof = params.appIdentity;
      if (!proof) {
        recv.replyError(req, { code: 'invalid_request', message: '缺少 App 身份证明' });
        return;
      }
      const res = await client.call({ signal }, 'connect.request.exec', {
        method, params, proof, origin,
      });
      if (res && res.approvalId) {
        pendingApproval.set(res.approvalId, { approvalId: res.approvalId, ctx: req, method, origin, reqId: req.id, receiver: recv });
      } else if (res) {
        // launch 直接返回授权结果（发起即授权，不经审批），只绑定本次连接；
        // 登录/恢复的心跳恢复同样直接回执。旧连接不得借新连接的授权执行业务。
        if (req.method === 'connect.launch' && res.authorizationId) authzOf(recv).set(origin, res.authorizationId);
        recv.replyResult(req, res);
      }
      return;
    }

    if (method === 'connect.logout') {
      // 退出只撤销该连接实际持有的授权：按 Receiver 查本连接的授权引用，
      // 不得按 origin 全局查找而撤销他连接的授权。
      const authorizationId = authzOf(recv).get(origin);
      if (!authorizationId) {
        recv.replyError(req, { code: 'invalid_request', message: '无有效授权' });
        return;
      }
      const res = await client.call({ signal }, 'connect.request.exec', { method, origin, authorizationId });
      authzOf(recv).delete(origin);
      recv.replyResult(req, res);
      return;
    }

    // 其余业务方法：必须持有本连接的有效授权（登录时按连接绑定），不接受裸 proof；
    // 旧连接的已失效授权在此被拒绝，不能借新连接的授权继续执行业务。
    const authorizationId = authzOf(recv).get(origin);
    if (!authorizationId) {
      const code = method.startsWith('storage.') ? 'storage_identity_required' : 'identity_required';
      recv.replyError(req, { code, message: '需要先登录授权' });
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
      pendingApproval.set(raw.result.approvalId, { approvalId: raw.result.approvalId, ctx: req, method, origin, reqId: req.id, receiver: recv });
      return;
    }
    recv.replyResult(req, toSdkResult(method, raw.result, raw.binaries || {}));
  } catch (e) {
    if (signal.aborted) return;
    const perr = e?.code && e?.message ? { code: e.code, message: e.message } : null;
    const method = req.method || '';
    if (perr && (perr.code === 'LOCKED' || perr.code === 'STOPPING' || perr.code === 'NOT_INITIALIZED' || perr.code === 'VERSION_MISMATCH')) {
      recv.replyError(req, sdkError(method, perr));
    } else if (e instanceof Error && e.name === 'TransportError') {
      recv.replyError(req, { code: 'internal_error', message: '后台连接失败' });
    } else if (perr) {
      recv.replyError(req, sdkError(method, perr));
    } else {
      recv.replyError(req, { code: 'internal_error', message: String(e?.message || e) });
    }
  } finally {
    inflightByReceiver.get(recv)?.delete(req.id);
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
  const recv = entry.receiver || receiver;
  try {
    if (decision === 'approve' && res?.status === 'approved') {
      const method = entry.method;
      // 认领执行前核对原请求上下文、连接世代与实际来源仍有效；失效不再认领，
      // 后台批准保持原终态并尽力原子终结该已批准未认领项（释放冻结不重复执行）。
      if (!isContextAlive(entry)) {
        try { await client.call(undefined, 'connect.approvals.decide', { approvalId, decision: 'cancel' }); } catch { /* Backend keeps its terminal state. */ }
        return res;
      }
      const exec = await client.callWithBinary(undefined, 'connect.request.exec', { approvalId }, {});
      recv.replyResult(entry.ctx, toSdkResult(method, exec.result, exec.binaries || {}));
      // 授权只绑定发起该审批的连接；旧连接不得借新连接的授权执行业务。
      if (method === 'connect.login' || method === 'connect.resume' || method === 'connect.launch') {
        const authorizationId = exec.result?.authorizationId;
        if (authorizationId) authzOf(recv).set(entry.origin, authorizationId);
      }
      if (method === 'connect.logout') authzOf(recv).delete(entry.origin);
    } else {
      recv.replyError(entry.ctx, { code: 'user_rejected', message: '用户拒绝' });
    }
  } catch (e) {
    const perr = e?.code && e?.message ? { code: e.code, message: e.message } : null;
    try {
      recv.replyError(entry.ctx, perr ? sdkError(entry.method, perr) : { code: 'internal_error', message: String(e?.message || e) });
    } catch { /* ignore */ }
  }
  return res;
}

export function noteConnected(instanceId) {
  const next = instanceId || '';
  // 后台重启导致运行代际变化：旧授权整体失效；通知各启动方并丢弃旧连接上下文
  // （含 popup 单例），旧启动上下文不得继续建立连接，由用户重新启动或重新登录。
  if (cachedGeneration && next && next !== cachedGeneration) {
    teardownAllLaunchedApps();
    teardownPopupReceiver();
  }
  cachedGeneration = next;
}
