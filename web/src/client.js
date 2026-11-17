// 应用级单一 APIProtocol 客户端：多页面共享同一连接，不同标签页各有连接。
// 统一 WS 传输、重连、快照与实例隔离。
// 前台装配失败不关闭传输：保留受生命周期管理的诊断传输，继续提供固定只读诊断。
import { KmWSClient } from 'keymaster-api-protocol';
import { startWebPlugins } from './plugins.js';
import { registerDiagnostics, diagnosticFailure, setBackendInspect, backendInspectFailure, markDiagnosticsDisconnected } from './diagnostics.js';
import { noteConnected } from './connect.js';

let clientPromise = null;
let connectionGeneration = 0;
let activeConnection = null;
let clientInstance = null;
let statusListeners = new Set();
let currentStatus = 'connecting';
let currentInstance = '';
let retryTimer = null;
let retryStep = 0;

const RETRY_DELAYS = [1000, 2000, 4000, 8000, 15000];

function setStatus(s, instance = '') {
  currentStatus = s;
  if (instance) currentInstance = instance;
  for (const fn of statusListeners) {
    try { fn(s); } catch { /* ignore */ }
  }
}

export function onConnectionStatus(fn) {
  statusListeners.add(fn);
  fn(currentStatus);
  return () => statusListeners.delete(fn);
}

export function getConnectionStatus() {
  return currentStatus;
}

export function getInstanceId() {
  return currentInstance;
}

export function getConnectionGeneration() {
  return connectionGeneration;
}

function toStructuredError(stage, error) {
  const code = (error && (error.code || error.name)) ? String(error.code || error.name) : 'UNKNOWN';
  const message = error && error.message ? String(error.message) : String(error || '后台诊断失败');
  // 只保留阶段、代码和脱敏消息，不复制完整原始错误对象、堆栈、请求参数、
  // 业务内容、附件、授权对象、实际数据根路径或带查询参数的连接 URL。
  return { stage, code: code.slice(0, 64), message: message.slice(0, 500) };
}

async function fetchBackendForConnection(connection) {
  const { raw, generation } = connection;
  if (!raw || generation !== connectionGeneration || activeConnection !== connection) return null;
  const fetchedAt = Date.now();
  try {
    const data = await raw.call(undefined, 'plugins.inspect');
    if (generation !== connectionGeneration || activeConnection !== connection) return null;
    setBackendInspect(data, { generation, fetchedAt });
    return data;
  } catch (error) {
    if (generation !== connectionGeneration || activeConnection !== connection) return null;
    // 诊断读取失败不能导致正常连接被判失败或被关闭。
    backendInspectFailure(toStructuredError('backend.inspect', error), { generation, fetchedAt });
    return null;
  }
}

// 显式刷新后台只读诊断；失败不影响连接状态，由调用方展示未知态。
export async function refreshBackendInspect() {
  const connection = activeConnection;
  if (!connection || !connection.raw) throw new Error('后台连接不可用');
  return fetchBackendForConnection(connection);
}

function failingBusinessClient(raw, frontendError) {
  return new Proxy(raw, { get(target, property) {
    if (property === 'getInstanceId' || property === 'getConnectionId' || property === 'getStatus' || property === 'onConnectionChange' || property === 'close' || property === 'subscribeEvents') {
      const value = Reflect.get(target, property);
      return typeof value === 'function' ? value.bind(target) : value;
    }
    if (property === 'then') return undefined;
    if (typeof property === 'string' && (property === 'call' || property === 'callWithBinary')) {
      return () => Promise.reject(frontendError);
    }
    const value = Reflect.get(target, property);
    if (typeof value !== 'function') return value;
    return () => Promise.reject(frontendError);
  } });
}

async function connectOnce(generation) {
  setStatus('connecting');
  // 同源：浏览器经同一后台的 /api/ws 进入；WS 来源即 CLI Web 来源。
  const base = window.location.origin;
  const c = await KmWSClient.connect(base, { name: 'km-web', version: '0.1.0' });
  if (generation !== connectionGeneration) {
    c.close();
    throw new Error('连接已被替换');
  }
  // 认领诊断代际：清除旧 Go 快照，迟到结果不得覆盖新状态。
  try { registerDiagnostics(null, { generation, source: 'web.plugins' }); } catch { /* ignore */ }
  let plugins = null;
  let frontendError = null;
  try {
    plugins = await startWebPlugins(c, { generation, source: 'web.plugins' });
  } catch (error) {
    frontendError = error;
    try { diagnosticFailure(error, { generation, source: 'web.plugins' }); } catch { /* ignore */ }
  }
  if (generation !== connectionGeneration) {
    if (plugins) { try { await plugins.dispose(); } catch { /* ignore */ } }
    c.close();
    throw new Error('连接已被替换');
  }
  const connection = { raw: c, plugins, frontendError, generation };
  activeConnection = connection;
  clientInstance = c.getInstanceId();
  setStatus('connected', clientInstance);
  noteConnected(clientInstance);
  retryStep = 0;
  // 连接关闭由 SDK 状态回调通知；仅当仍是当前连接时才触发重连。
  // 断开时标记诊断失效，不保留绿色正常状态；替换、断开和卸载时正确清理。
  c.onConnectionChange((s) => {
    if (s !== 'connected' && activeConnection === connection) {
      try { markDiagnosticsDisconnected({ generation }); } catch { /* ignore */ }
      if (connection.plugins) { try { connection.plugins.dispose()?.catch?.(() => {}); } catch { /* ignore */ } }
      activeConnection = null;
      scheduleReconnect();
    } else if (s !== 'connected' && connection.plugins) {
      try { connection.plugins.dispose()?.catch?.(() => {}); } catch { /* ignore */ }
    }
  });
  // 进入页面、连接建立后读取后台只读诊断；失败不影响连接。
  void fetchBackendForConnection(connection).catch(() => {});
  if (plugins) return plugins.client;
  // 前台装配失败：保留诊断传输，业务调用明确失败，诊断仍可经固定只读调用取得。
  return failingBusinessClient(c, frontendError || new Error('前台插件装配失败'));
}

function scheduleReconnect() {
  if (currentStatus === 'protocol-incompatible') return;
  connectionGeneration += 1;
  setStatus('disconnected');
  clientPromise = null;
  clientInstance = '';
  if (retryStep >= RETRY_DELAYS.length) {
    // 停止自动尝试，提供显式重试（调用 getClient(true)）。
    return;
  }
  if (retryTimer) clearTimeout(retryTimer);
  const delay = RETRY_DELAYS[retryStep++];
  retryTimer = setTimeout(() => {
    clientPromise = null;
    getClient().catch(() => {});
  }, delay);
}

export async function getClient(force = false) {
  if (force) {
    connectionGeneration += 1;
    const previous = activeConnection;
    activeConnection = null;
    if (previous) {
      if (previous.plugins) { try { previous.plugins.dispose()?.catch?.(() => {}); } catch { /* ignore */ } }
      try { previous.raw.close(); } catch { /* ignore */ }
    }
    retryStep = 0;
    clientPromise = null;
    if (retryTimer) { clearTimeout(retryTimer); retryTimer = null; }
  }
  if (!clientPromise) {
    const generation = connectionGeneration;
    clientPromise = connectOnce(generation).catch((e) => {
      if (generation !== connectionGeneration) throw e;
      const msg = String(e?.message || e);
      if (e?.name === 'VersionError' || msg.includes('协议')) {
        setStatus('protocol-incompatible');
      } else {
        scheduleReconnect();
      }
      clientPromise = null;
      throw e;
    });
  }
  const pending = clientPromise;
  const connected = await pending;
  if (pending !== clientPromise) throw new Error('连接已被替换');
  try { clientInstance = connected.getInstanceId(); } catch { /* ignore */ }
  return connected;
}

// 显式重试（重连停止后由用户触发）：重建前台，清理旧前台 Runtime，不重启 Go 后台。
export function retryConnection() {
  return getClient(true);
}
