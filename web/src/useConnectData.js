import { useCallback, useEffect, useState } from 'react';
import { getClient } from './client.js';
import { watchTopics } from './subscribe.js';
import { pumpPendingApprovals } from './connect.js';
export function useConnectData() {
  const [approvals, setApprovals] = useState([]);
  const [apps, setApps] = useState({ applications: [], connections: [], permissions: {} });
  const [permissions, setPermissions] = useState([]);
  const [history, setHistory] = useState([]);
  const [err, setErr] = useState('');
  // 读取失败保留已知快照并明确标记未知，不伪装成正常空状态。
  const load = useCallback(async () => {
    const failures = [];
    try {
      const c = await getClient();
      try {
        const a = await c.call(undefined, 'connect.approvals.list');
        setApprovals(a.approvals || []);
      } catch (e) { failures.push(`approvals: ${e?.message || e}`); }
      try {
        const ap = await c.call(undefined, 'connect.apps.list');
        setApps(ap);
      } catch (e) { failures.push(`apps: ${e?.message || e}`); }
      try {
        const pm = await c.call(undefined, 'connect.permissions.list', {});
        setPermissions(pm.permissions || []);
      } catch (e) { failures.push(`permissions: ${e?.message || e}`); }
      try {
        const h = await c.call(undefined, 'connect.history.list', { limit: 200 });
        setHistory(h.events || []);
      } catch (e) { failures.push(`history: ${e?.message || e}`); }
      setErr(failures.length ? `读取失败（显示已知快照）：${failures.join('；')}` : '');
      try { await pumpPendingApprovals(); } catch { /* ignore */ }
    } catch (e) {
      setErr(`连接失败（显示已知快照）：${String(e?.message || e)}`);
    }
  }, []);
  useEffect(() => watchTopics(['connect'], () => load()), [load]);
  useEffect(() => { load(); }, [load]);
  return { approvals, apps, permissions, history, err, reload: load };
}

