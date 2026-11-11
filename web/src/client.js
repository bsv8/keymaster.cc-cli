// 应用级单一 APIProtocol 客户端：多页面共享同一连接，不同标签页各有连接。
// 统一 WS 传输、重连、快照与实例隔离。
import { KmWSClient } from 'keymaster-api-protocol';
import { noteConnected } from './connect.js';

let clientPromise = null;
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

async function connectOnce() {
  setStatus('connecting');
  // 同源：浏览器经同一后台的 /api/ws 进入；WS 来源即 CLI Web 来源。
  const base = window.location.origin;
  const c = await KmWSClient.connect(base, { name: 'km-web', version: '0.1.0' });
  clientInstance = c.getInstanceId();
  setStatus('connected', clientInstance);
  noteConnected(clientInstance);
  retryStep = 0;
  // 连接关闭由 SDK 状态回调通知；仅当仍是当前连接时才触发重连。
  c.onConnectionChange((s) => {
    if (s !== 'connected' && clientInstance === c.getInstanceId()) {
      scheduleReconnect();
    }
  });
  return c;
}

function scheduleReconnect() {
  if (currentStatus === 'protocol-incompatible') return;
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
    retryStep = 0;
    clientPromise = null;
    if (retryTimer) { clearTimeout(retryTimer); retryTimer = null; }
  }
  if (!clientPromise) {
    clientPromise = connectOnce().catch((e) => {
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
  clientInstance = (await clientPromise).getInstanceId();
  return clientPromise;
}

// 显式重试（重连停止后由用户触发）。
export function retryConnection() {
  return getClient(true);
}
