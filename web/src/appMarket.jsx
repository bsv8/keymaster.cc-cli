import React, { useCallback, useEffect, useRef, useState } from 'react';
import { getClient } from './client.js';
import { watchTopics } from './subscribe.js';
import { useApplicationWorkspace } from './workspace.jsx';
import { directoryLaunchTarget, prepareDirectoryEntry, selectedDirectoryTarget } from './directoryLaunch.js';
import { useConnectData } from './useConnectData.js';
import { AppIcon, operationName, eventName } from './appPresentation.jsx';

export async function appCall(method, params = {}) { return (await getClient()).call(undefined, method, params); }
function Star({ favorite, disabled, onClick, testId }) { const label = favorite ? '取消收藏' : '收藏'; return <button type="button" className="ghost market-star" aria-label={label} title={label} aria-pressed={!!favorite} disabled={disabled} onClick={onClick} data-testid={testId}><span aria-hidden="true">{favorite ? '★' : '☆'}</span></button>; }
export function useMarket() {
  const [data, setData] = useState({ entries: [], favorites: {}, knownApplications: {}, deletedKeys: [] });
  const [deployments, setDeployments] = useState([]);
  const [historicalApps, setHistoricalApps] = useState([]);
  const [loading, setLoading] = useState(true), [error, setError] = useState('');
  const sequence = useRef(0);
  const load = useCallback(async () => {
    const id = ++sequence.current;
    try {
      const result = await appCall('connect.directory.list');
      if (id !== sequence.current) return;
      setData(result); setError('');
      const [installed, known] = await Promise.all([appCall('connect.deployment.list'), appCall('connect.apps.list')]);
      if (id === sequence.current) setHistoricalApps(known.applications || []);
      if (id === sequence.current) setDeployments(installed.deployments || []);
    } catch { if (id === sequence.current) setError('应用资料读取失败，请重试。'); }
    finally { if (id === sequence.current) setLoading(false); }
  }, []);
  useEffect(() => { void load(); const off = watchTopics(['connect'], load); return () => { ++sequence.current; off(); }; }, [load]);
  // 用户手动删除的 App 不进入目录展示；新的已验证连接清除删除标记后恢复。
  const deleted = new Set(data.deletedKeys || []);
  const entries = [...(data.entries || [])];
  for (const [key, display] of Object.entries(data.knownApplications || {})) {
    if (deleted.has(key) || entries.some(entry => entry.appPublicKeyHex === key)) continue;
    entries.push({ appPublicKeyHex: key, ...display, urls: [], files: [], favorite: !!data.favorites?.[key] });
  }
  for (const app of historicalApps) {
    if (deleted.has(app.applicationKey) || entries.some(entry => entry.appPublicKeyHex === app.applicationKey)) continue;
    entries.push({ appPublicKeyHex: app.applicationKey, name: '使用过的应用', urls: [], files: [], favorite: !!data.favorites?.[app.applicationKey] });
  }
  return { ...data, entries, deployments, loading, error, load };
}
export function entryPresentation(entry, deployments) {
  const selected = selectedDirectoryTarget(entry);
  const channel = selected.url ? entry.urls?.find(item => item.url === selected.url) : entry.files?.find(item => item.indexseedhashhex === selected.indexseedhashhex);
  const display = channel?.display || entry;
  return { name: display.name || entry.name || '未命名应用', description: display.description || '', icon: selected.url || !selected.indexseedhashhex ? display.icon : deployments.find(item => item.indexseedhashhex === selected.indexseedhashhex && item.status === 'serving')?.iconUrl, url: selected.url };
}
export function MarketPage({ navigate }) {
  const market = useMarket(), workspace = useApplicationWorkspace();
  const [query, setQuery] = useState(''), [busy, setBusy] = useState(false), [error, setError] = useState('');
  const mutation = useRef(false);
  const changeFavorite = async entry => { if (mutation.current) return; mutation.current = true; setBusy(true); setError(''); try { await appCall('connect.directory.favorite', { appPublicKeyHex: entry.appPublicKeyHex, favorite: !entry.favorite }); await market.load(); } catch { setError('收藏更改未能保存。'); } finally { mutation.current = false; setBusy(false); } };
  const launch = entry => workspace.requestLaunch(directoryLaunchTarget(entry, () => prepareDirectoryEntry(entry, appCall)));
  const card = (entry, favorite = false) => {
    const key = entry.appPublicKeyHex, display = entryPresentation(entry, market.deployments), selected = selectedDirectoryTarget(entry);
    return <article className="market-card" key={key} data-testid={`${favorite ? 'favorite-card' : 'directory-card'}-${key}`}>
      <div className="market-card-heading"><AppIcon {...display} /><div><h3>{display.name}</h3><small>{entry.favorite ? '已收藏' : '应用'}</small></div></div>
      <p className="hint">{entry.appVersion ? `版本 ${entry.appVersion}` : ""}</p>
      <p className="hint">{display.description || '打开应用，体验更多服务。'}</p>
      <div className="row"><button data-entry-url={selected.url} data-testid={favorite ? `favorite-launch-${key}` : `directory-launch-${key}`} disabled={!workspace.ready || workspace.busy || (!selected.url && !selected.indexseedhashhex)} onClick={() => launch(entry)}>打开</button><button className="ghost" onClick={() => navigate(`/app/${key}`)}>查看详情</button><Star favorite={entry.favorite} disabled={busy} onClick={() => changeFavorite(entry)} testId={`${favorite ? 'unfavorite' : 'favorite'}-${key}`} /></div>
    </article>;
  };
  const matches = entry => entryPresentation(entry, market.deployments).name.toLocaleLowerCase().includes(query.trim().toLocaleLowerCase());
  const visible = market.entries.filter(matches);
  return <div className="app-market">
    <header className="market-heading"><div><h1>应用市场</h1><p className="hint">发现应用，管理授权与使用记录。</p></div><button className="ghost" onClick={() => navigate('/settings/market')}>添加应用</button></header>
    {(market.error || error) && <p className="error" role="alert">{error || market.error}</p>}
    <section className="card" aria-label="收藏应用"><h2>收藏应用</h2><div className="market-grid" data-testid="apps-favorites">{market.entries.filter(entry => entry.favorite).map(entry => card(entry, true))}</div>{!market.entries.some(entry => entry.favorite) && <p className="hint">还没有收藏应用，可以在下方收藏。</p>}</section>
    <section aria-label="所有应用"><div className="market-heading"><h2>所有应用</h2><input type="search" aria-label="按应用名称搜索" placeholder="搜索应用名称" value={query} onChange={event => setQuery(event.target.value)} /></div>
      <div className="market-grid" data-testid="apps-list">{visible.map(entry => card(entry))}</div>
      {!visible.length && <p className="hint" role="status">{market.loading ? '正在加载应用…' : query ? '没有找到匹配的应用。' : '暂无应用。'}</p>}
    </section>
  </div>;
}

export function AppDetailPage({ appId, navigate }) {
  const market = useMarket(), data = useConnectData(), workspace = useApplicationWorkspace();
  const entry = market.entries.find(entry => entry.appPublicKeyHex === appId);
  const [tab, setTab] = useState('settings'), [busy, setBusy] = useState(false), [error, setError] = useState('');
  const display = entry ? entryPresentation(entry, market.deployments) : { name: '应用详情' };
  const mutate = async (method, params) => { if (busy) return; setBusy(true); setError(''); try { await appCall(method, params); await market.load(); await data.reload(); } catch { setError('更改未能完成，请重试。'); } finally { setBusy(false); } };
  const connections = data.apps.connections.filter(item => item.applicationKey === appId);
  const history = data.history.filter(item => item.applicationKey === appId);
  const permission = data.permissions.find(item => item.applicationKey === appId);
  return <section className="app-market" data-testid="app-detail-page"><header className="market-heading"><h1>{display.name}</h1><p>{entry?.appVersion ? `版本 ${entry.appVersion}` : ""}</p><button className="ghost" onClick={() => navigate('/apps')}>返回应用市场</button></header>
    {(error || market.error || data.err) && <p role="alert" className="error">{error || market.error || '应用记录暂时不可用。'}</p>}
    {entry ? <><div className="market-card-heading"><AppIcon {...display} size={80} /><p className="hint">{entry.appVersion ? `版本 ${entry.appVersion}` : ""}</p>
      <p className="hint">{display.description || '此应用的授权、会话及处理记录。'}</p></div><div className="row"><button disabled={!workspace.ready || workspace.busy || (!selectedDirectoryTarget(entry).url && !selectedDirectoryTarget(entry).indexseedhashhex)} onClick={() => workspace.requestLaunch(directoryLaunchTarget(entry, () => prepareDirectoryEntry(entry, appCall)))}>打开应用</button><Star favorite={entry.favorite} disabled={busy} onClick={() => mutate('connect.directory.favorite', { appPublicKeyHex: appId, favorite: !entry.favorite })} /><button className="ghost" data-testid={`delete-app-${appId}`} disabled={busy} onClick={async () => { await mutate('connect.directory.remove', { appPublicKeyHex: appId }); }}>删除应用</button></div>
      {(entry.urls?.length || 0) + (entry.files?.length || 0) > 1 && <label>首选入口<select aria-label="首选入口" value={entry.selected?.url ? `url:${entry.selected.url}` : entry.selected?.indexseedhashhex ? `files:${entry.selected.indexseedhashhex}` : entry.urls?.length ? `url:${entry.urls[0].url}` : `files:${entry.files[0].indexseedhashhex}`} onChange={event => { const value = event.target.value; void mutate('connect.directory.select', { appPublicKeyHex: appId, ...(value.startsWith('url:') ? { url: value.slice(4) } : { indexseedhashhex: value.slice(6) }) }); }}>{entry.urls?.map((item, index) => <option key={item.url} value={`url:${item.url}`}>网页入口 {index + 1}</option>)}{entry.files?.map((item, index) => <option key={item.indexseedhashhex} value={`files:${item.indexseedhashhex}`}>Files 渠道</option>)}</select></label>}
      <div className="application-tabs">{[['settings', '永久授权'], ['sessions', '会话'], ['history', '授权与处理历史']].map(([key, label]) => <button className={tab === key ? 'selected' : 'ghost'} key={key} aria-pressed={tab === key} onClick={() => setTab(key)}>{label}</button>)}</div>
      {tab === 'settings' && <><p className="hint">设置属于此应用，关闭或重新打开后仍有效。</p>{['identity.get', 'intent.sign', 'cipher.encrypt', 'cipher.decrypt', 'payment.pay'].map(method => <div className="application-setting" key={method}><strong>{operationName(method)}</strong><span>{data.err ? '状态未知' : permission?.methods?.includes(method) ? '以后自动同意' : '每次询问'}</span>{permission?.methods?.includes(method) && <button disabled={busy || !!data.err} data-testid={`revoke-perm-${method}`} onClick={() => mutate('connect.permissions.revoke', { applicationKey: appId, method })}>取消自动同意</button>}{method === 'payment.pay' && permission?.methods?.includes(method) && <small>自动付款不设金额上限。</small>}</div>)}<p className="hint">只能在实际审批中授予永久授权；取消后新的请求再次询问。</p></>}
      {tab === 'sessions' && <>{connections.length > 0 && <button data-testid="disconnect-app" disabled={busy} onClick={() => mutate('connect.auth.revoke', { appPublicKeyHex: appId })}>断开此应用全部连接</button>}{connections.map(connection => <div className="application-session" key={connection.id}><strong>当前会话</strong><time>{new Date(connection.createdAt).toLocaleString()}</time></div>)}{history.filter(event => event.kind === 'connected').map(event => <div className="application-session" key={event.id}><strong>会话记录</strong><time>{new Date(event.at).toLocaleString()}</time></div>)}{!connections.length && <p className="hint">当前没有在线会话。</p>}</>}
      {tab === 'history' && <>{history.map(event => <details className="application-history" key={event.id}><summary>{operationName(event.method)} · {eventName(event)} · {new Date(event.at).toLocaleString()}</summary><p className="hint">{event.auto ? '使用已有永久授权' : '人工处理或连接事件'}</p></details>)}{!history.length && <p className="hint">暂无处理记录。</p>}</>}
      {market.deployments.filter(item => item.appPublicKeyHex === appId && item.status === 'serving').map(item => <button className="ghost" key={item.deploymentId} disabled={busy} onClick={() => mutate('connect.deployment.stop', { deploymentId: item.deploymentId })}>停止文件应用服务</button>)}
    </> : <p className="hint" role="status">{market.loading ? '正在加载应用…' : market.error ? '暂时无法读取此应用。' : '未找到此应用。'}</p>}
  </section>;
}
