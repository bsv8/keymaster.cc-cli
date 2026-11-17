import React, { useState } from 'react';
export function AppIcon({ icon, url, name, size = 64 }) {
  let source; try { const u = new URL(icon || '', url); if (icon && (['http:', 'https:'].includes(u.protocol) || u.href.startsWith('data:image/'))) source = u.href; } catch {}
  const [failed, setFailed] = useState('');
  return <span className="market-icon" style={{ width: size, height: size }} aria-hidden="true">{source && failed !== source ? <img src={source} alt="" onError={() => setFailed(source)} /> : Array.from(name || '应用')[0]}</span>;
}
const LABELS = { 'identity.get': '获取身份凭证', 'intent.sign': '签名', 'cipher.encrypt': '加密内容', 'cipher.decrypt': '解密内容', 'payment.pay': '付款', 'connect.login': '连接钱包', 'connect.launch': '打开应用', 'connect.logout': '断开连接' };
export const operationName = method => LABELS[method] || '应用操作';
export const eventName = event => ({ connected: '会话已建立', disconnected: '会话已断开', 'permission-granted': '开启永久授权', 'permission-revoked': '取消永久授权', 'manual-approved': '已批准', 'auto-approved': '自动批准', executed: '已完成', failed: '失败', 'user-canceled': '已拒绝', 'timed-out': '已超时' })[event.kind] || '已处理';
