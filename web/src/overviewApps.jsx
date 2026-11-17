import React, { useCallback, useEffect, useRef, useState } from 'react';
import { getClient } from './client.js';
import { watchTopics } from './subscribe.js';
import { useApplicationWorkspace } from './workspace.jsx';
import { directoryLaunchTarget, prepareDirectoryEntry, selectedDirectoryTarget } from './directoryLaunch.js';

function AppIcon({ source, name }) {
  const [failed, setFailed] = useState(false);
  useEffect(() => setFailed(false), [source]);
  return <span className="overview-app-icon" aria-hidden="true">{source && !failed
    ? <img src={source} alt="" width="72" height="72" loading="lazy" referrerPolicy="no-referrer" onError={() => setFailed(true)} />
    : <span>{Array.from(name)[0]?.toLocaleUpperCase() || 'A'}</span>}</span>;
}

/** 首页只投影后台目录，不另存缓存，也不改变应用授权。 */
export function OverviewApps({ onBrowse }) {
  const workspace = useApplicationWorkspace();
  const [entries, setEntries] = useState([]);
  const [deployments, setDeployments] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [marketError, setMarketError] = useState('');
  const sequence = useRef(0);
  const load = useCallback(async () => {
    const current = ++sequence.current;
    setLoading(true);
    try {
      const client = await getClient();
      const result = await client.call(undefined, 'connect.directory.list', {});
      if (current !== sequence.current) return;
      setEntries((result.entries || []).filter(entry => entry.marketListed || entry.favorite)
        .sort((left, right) => Number(right.favorite) - Number(left.favorite)));
      setMarketError(result.marketError || ''); setError('');
      // 浏览首页只读取已存在的 Files 部署图标，不启动下载或采购。
      try {
        const installed = await client.call(undefined, 'connect.deployment.list', {});
        if (current === sequence.current) setDeployments(installed.deployments || []);
      } catch { if (current === sequence.current) setDeployments([]); }
    } catch (cause) { if (current === sequence.current) setError(String(cause.message || cause)); }
    finally { if (current === sequence.current) setLoading(false); }
  }, []);
  useEffect(() => {
    void load();
    const stop = watchTopics(['connect'], load);
    return () => { ++sequence.current; stop(); };
  }, [load]);
  const launch = entry => workspace.requestLaunch(directoryLaunchTarget(entry, async () => {
    const client = await getClient();
    return prepareDirectoryEntry(entry, (method, params) => client.call(undefined, method, params));
  }));
  return <section className="card overview-apps" aria-labelledby="overview-apps-title" data-testid="overview-apps">
    <div className="overview-apps-heading"><h2 id="overview-apps-title">应用</h2><button type="button" className="ghost" onClick={onBrowse}>应用市场</button></div>
    {error && <p className="error" role="alert">应用加载失败：{error} <button type="button" onClick={load}>重试</button></p>}
    {marketError && <p className="error" role="status">市场更新失败：{marketError} <button type="button" onClick={load}>重试</button></p>}
    {!entries.length && !error && <p className="hint" role="status">{loading ? '正在加载应用…' : '暂无应用，可前往应用市场查看。'}</p>}
    <ul className="overview-app-grid" aria-label="应用快捷入口" aria-busy={loading}>
      {entries.map(entry => {
        const name = entry.name || '未命名应用';
        const target = selectedDirectoryTarget(entry);
        const icon = target.url ? (() => { try { return new URL(entry.icon, target.url).href; } catch { return undefined; } })() : deployments.find(d => d.indexseedhashhex === target.indexseedhashhex && d.status === 'serving')?.iconUrl;
        return <li key={entry.appPublicKeyHex}><button type="button" className="overview-app-tile" data-testid={`overview-app-${entry.appPublicKeyHex}`} disabled={!workspace.ready || workspace.busy} onClick={() => launch(entry)} title={name}>
          <AppIcon source={icon} name={name} /><span className="overview-app-name">{name}</span>
          {entry.favorite && <span className="overview-app-favorite" aria-label="已收藏">★</span>}
        </button></li>;
      })}
    </ul>
  </section>;
}
