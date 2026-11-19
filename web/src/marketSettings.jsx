import React, { useCallback, useEffect, useRef, useState } from 'react';
import { appCall } from './appMarket.jsx';
import { watchTopics } from './subscribe.js';

const VALIDATION_TEXT = { pending: '验证：进行中', failed: '验证：失败', verified: '验证：通过' };
const SYNC_TEXT = { unsynced: '同步：待同步', synced: '同步：已同步', 'sync-failed': '同步：失败', 'lower-version': '同步：已验证，版本较低，未更新目录' };

function ManualItemCard({ item, busy, onRetry, onRemove }) {
  const title = item.name || (item.kind === 'url' ? item.url || 'URL 来源' : 'Files 来源');
  return <article className="market-card" data-testid={`${item.validation === 'verified' ? 'market-manual' : 'market-pending'}-${item.pendingId}`}>
    <div className="market-card-heading"><div><h3>{title}</h3><small>{item.kind === 'url' ? 'URL 手工来源' : 'Files 手工来源'}</small></div></div>
    {item.validation === 'failed' && <p className="error" role="alert">{item.validationError || '验证失败。'}</p>}
    {item.validation !== 'failed' && <p className="hint">{VALIDATION_TEXT[item.validation] || item.validation}{item.appVersion ? ` · 版本 ${item.appVersion}` : ''}</p>}
    {item.validation === 'verified' && <p className="hint">{SYNC_TEXT[item.syncStatus] || item.syncStatus || SYNC_TEXT.unsynced}</p>}
    {item.syncStatus === 'sync-failed' && item.syncError && <p className="error" role="alert">{item.syncError}</p>}
    <div className="row">
      {item.validation !== 'verified' && <button className="ghost" data-testid={`market-retry-${item.pendingId}`} disabled={busy} onClick={() => onRetry(item)}>重试验证</button>}
      {item.validation === 'verified' && item.syncStatus !== 'synced' && item.syncStatus !== 'lower-version' && <button className="ghost" data-testid={`market-retry-${item.pendingId}`} disabled={busy} onClick={() => onRetry(item)}>重试同步</button>}
      <button className="ghost" data-testid={`market-remove-${item.pendingId}`} disabled={busy} onClick={() => onRemove(item)}>移除</button>
    </div>
  </article>;
}

function MarketCard({ market, refreshing, removing, onRefresh, onDiscard }) {
  return <article className="market-card" data-testid={`market-card-${market.id}`}>
    <div className="market-card-heading"><div><h3>{market.name}</h3><small>{market.builtIn ? '内置市场' : '第三方市场'}</small></div></div>
    <p className="hint" data-testid={`market-url-${market.id}`}>{market.directoryUrl}</p>
    <p className="hint">{market.lastSuccessAt ? `上次成功同步：${new Date(market.lastSuccessAt).toLocaleString()}` : '尚未成功同步。'}</p>
    {market.lastError && <p className="error" role="alert" data-testid={`market-error-${market.id}`}>上次同步失败：{market.lastError}</p>}
    {(market.lastRejected || []).map((item, index) => <p className="hint" key={`${item.appPublicKeyHex}-${index}`}>被拒条目 {item.appPublicKeyHex.slice(0, 12)}…（{item.status}）：{item.reason || '未提供原因'}</p>)}
    <div className="row">
      <button className="ghost" data-testid={`market-refresh-${market.id}`} disabled={refreshing || removing} onClick={() => onRefresh(market)}>{refreshing ? '刷新中…' : '刷新此市场'}</button>
      {!market.builtIn && <button className="ghost" data-testid={`market-discard-${market.id}`} disabled={removing} onClick={() => onDiscard(market)}>移除市场</button>}
    </div>
  </article>;
}

export function MarketSettingsPage({ navigate }) {
  const [state, setState] = useState(null), [loadError, setLoadError] = useState('');
  const [submitting, setSubmitting] = useState(false), [submitError, setSubmitError] = useState('');
  const [syncing, setSyncing] = useState(false), [syncError, setSyncError] = useState('');
  const [configuring, setConfiguring] = useState(false);
  const [url, setURL] = useState(''), [seed, setSeed] = useState('');
  const [marketName, setMarketName] = useState(''), [marketURL, setMarketURL] = useState('');
  const [busyID, setBusyID] = useState('');
  const [refreshingMarkets, setRefreshingMarkets] = useState(() => new Set());
  const sequence = useRef(0);
  const load = useCallback(async () => {
    const id = ++sequence.current;
    try {
      const result = await appCall('connect.market.state');
      if (id !== sequence.current) return;
      setState(result); setLoadError('');
    } catch { if (id === sequence.current) setLoadError('市场状态读取失败，请重试。'); }
  }, []);
  useEffect(() => { void load(); const off = watchTopics(['connect'], load); return () => { ++sequence.current; off(); }; }, [load]);

  const syncAll = async () => {
    if (syncing) return;
    setSyncing(true); setSyncError('');
    try { setState(await appCall('connect.market.sync', {})); }
    catch (e) { setSyncError(`同步失败：${e?.message || '市场暂不可用'}`); await load(); }
    finally { setSyncing(false); }
  };
  const refreshMarket = async market => {
    if (refreshingMarkets.has(market.id)) return;
    setRefreshingMarkets(current => new Set(current).add(market.id)); setSyncError('');
    try { await appCall('connect.market.sync', { marketId: market.id }); }
    catch (e) { setSyncError(`同步失败：${e?.message || '市场暂不可用'}`); }
    finally {
      await load();
      setRefreshingMarkets(current => {
        const next = new Set(current);
        next.delete(market.id);
        return next;
      });
    }
  };
  const discardMarket = async market => {
    if (busyID) return;
    setBusyID(market.id); setSyncError('');
    try { setState(await appCall('connect.market.discard', { marketId: market.id })); }
    catch (e) { setSyncError(`移除失败：${e?.message || '请稍后再试。'}`); await load(); }
    finally { setBusyID(''); }
  };
  const configureMarket = async event => {
    event.preventDefault();
    if (configuring) return;
    if (!marketName.trim() || !marketURL.trim()) { setSubmitError('市场名称与目录 URL 必填。'); return; }
    setConfiguring(true); setSubmitError('');
    try {
      setState(await appCall('connect.market.configure', { name: marketName.trim(), directoryUrl: marketURL.trim() }));
      setMarketName(''); setMarketURL('');
    } catch (e) { setSubmitError(`添加失败：${e?.message || '请检查目录地址。'}`); }
    finally { setConfiguring(false); }
  };
  const submit = async event => {
    event.preventDefault();
    if (submitting) return;
    if (!!url.trim() === !!seed.trim()) { setSubmitError('URL 与种子只能二选一。'); return; }
    setSubmitting(true); setSubmitError('');
    try {
      setState(await appCall('connect.market.add', url.trim() ? { url: url.trim() } : { indexseedhashhex: seed.trim() }));
      setURL(''); setSeed('');
    } catch (e) { setSubmitError(`添加失败：${e?.message || '请检查输入。'}`); }
    finally { setSubmitting(false); }
  };
  const retry = async item => {
    if (busyID) return;
    setBusyID(item.pendingId); setSubmitError('');
    try { setState(await appCall('connect.market.retry', { pendingId: item.pendingId })); }
    catch (e) { setSubmitError(`重试失败：${e?.message || '请稍后再试。'}`); await load(); }
    finally { setBusyID(''); }
  };
  const remove = async item => {
    if (busyID) return;
    setBusyID(item.pendingId); setSubmitError('');
    try { setState(await appCall('connect.market.remove', { pendingId: item.pendingId })); }
    catch (e) { setSubmitError(`移除失败：${e?.message || '请稍后再试。'}`); await load(); }
    finally { setBusyID(''); }
  };

  const markets = state?.markets || [], pending = state?.pending || [], manual = state?.manual || [];
  return <div className="app-market" data-testid="market-settings-page">
    <header className="market-heading"><div><h1>市场与手工来源</h1><p className="hint">内置市场解锁后自动同步一次；第三方市场在此登记，手动刷新。</p></div><button className="ghost" onClick={() => navigate('/apps')}>返回应用市场</button></header>
    {(loadError || submitError || syncError) && <p className="error" role="alert">{loadError || submitError || syncError}</p>}
    {!state && !loadError && <p className="hint" role="status">正在读取市场状态…</p>}
    {state && <>
      <section className="card" aria-label="市场目录">
        <h2>市场目录</h2>
        <p className="hint" data-testid="market-sync-state">{`已收录市场条目：${state.appliedMarketCount ?? 0}`}</p>
        <button data-testid="market-sync-now" disabled={syncing || !!busyID} onClick={syncAll}>{syncing ? '全部同步中…' : '全部同步'}</button>
        <div className="market-grid">{markets.map(market => <MarketCard key={market.id} market={market} refreshing={refreshingMarkets.has(market.id)} removing={!!busyID} onRefresh={refreshMarket} onDiscard={discardMarket} />)}</div>
      </section>
      <section className="card" aria-label="添加第三方市场">
        <h2>添加第三方市场</h2>
        <p className="hint">填写名称与完整目录 URL（可指向任何路径或静态托管 JSON）；同一目录地址只登记一次。</p>
        <form onSubmit={configureMarket}>
          <label>市场名称<input data-testid="market-configure-name" aria-label="市场名称" value={marketName} onChange={event => setMarketName(event.target.value)} disabled={configuring} /></label>
          <label>目录 URL<input data-testid="market-configure-url" aria-label="目录 URL" type="url" value={marketURL} onChange={event => setMarketURL(event.target.value)} disabled={configuring} /></label>
          <button data-testid="market-configure-add" disabled={configuring}>登记市场</button>
        </form>
      </section>
      <section className="card" aria-label="添加手工来源">
        <h2>添加手工来源</h2>
        <p className="hint">URL 来源验签发布证明后收录；Files 来源完成整包下载验证后收录。名称与资料取自发布本身，不接受手填。</p>
        <form onSubmit={submit}>
          <label>应用网址<input data-testid="market-manual-url" aria-label="应用 URL" type="url" value={url} onChange={event => setURL(event.target.value)} disabled={submitting} /></label>
          <label>或文件应用种子<input data-testid="market-manual-seed" aria-label="Files seed" value={seed} onChange={event => setSeed(event.target.value)} disabled={submitting} /></label>
          <button data-testid="market-manual-add" disabled={submitting}>添加</button>
        </form>
      </section>
      <section className="card" aria-label="手工来源与待验证项">
        <h2>手工来源与待验证项</h2>
        {pending.length === 0 && manual.length === 0 && <p className="hint" role="status" data-testid="market-manual-empty">暂无手工来源或待验证项。</p>}
        {pending.length > 0 && <h3>待验证</h3>}
        <div className="market-grid">{pending.map(item => <ManualItemCard key={item.pendingId} item={item} busy={busyID === item.pendingId} onRetry={retry} onRemove={remove} />)}</div>
        {manual.length > 0 && <h3>已验证手工来源</h3>}
        <div className="market-grid">{manual.map(item => <ManualItemCard key={item.pendingId} item={item} busy={busyID === item.pendingId} onRetry={retry} onRemove={remove} />)}</div>
      </section>
    </>}
  </div>;
}
