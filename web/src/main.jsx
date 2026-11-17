import React, { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import './styles.css';
import { WalletFrame, WalletOnboarding, WalletUnlock } from './walletEntry.jsx';
import { t, setLang, resolveLang } from './i18n.js';
import { watchTopics } from './subscribe.js';
import { getClient, getInstanceId, getConnectionStatus, onConnectionStatus, retryConnection } from './client.js';
import { initConnectReceiver, teardownAllLaunchedApps, sweepClosedLaunchedApps, sweepClosedPopup, pumpPendingApprovals } from './connect.js';
import { ApplicationWorkspace, WorkspaceTabs, useWorkspace, useApplicationWorkspace } from './workspace.jsx';
import { OverviewApps } from './overviewApps.jsx';
import { GlobalApproval } from './approvalCenter.jsx';
import { MarketPage, AppDetailPage } from './appMarket.jsx';
import { PluginDiagnosticsPage } from './pluginDiagnostics.jsx';

const TABS = [
  { id: 'overview', label: '概览' },
  { id: 'sell', label: '售卖' },
  { id: 'get', label: '下载' },
  { id: 'files', label: '文件' },
  { id: 'collect', label: '采集' },
  { id: 'settings', label: '设置' },
  { id: 'apps', label: '应用市场' },
  { id: 'plugins', label: '插件' },
];

function useAppliedTheme(theme) {
  useEffect(() => {
    // Theme single-source is settings; default follows the OS.
    // No localStorage is used anywhere.
    const root = document.documentElement;
    if (!theme || theme === 'system') {
      const mq = window.matchMedia('(prefers-color-scheme: dark)');
      const apply = () => {
        const t = mq.matches ? 'dark' : 'light';
        root.dataset.theme = t;
        root.style.colorScheme = t;
      };
      apply();
      mq.addEventListener('change', apply);
      return () => mq.removeEventListener('change', apply);
    }
    root.dataset.theme = theme;
    root.style.colorScheme = theme;
  }, [theme]);
}

async function client() {
  return getClient();
}

function StateBadge({ state, locked }) {
  if (state === 'uninitialized') return <span className="badge bad">{t('未初始化')}</span>;
  if (locked) return <span className="badge warn">{t('已锁定')}</span>;
  if (state === 'unlocked') return <span className="badge ok">{t('已解锁')}</span>;
  return <span className="badge">{state || '—'}</span>;
}

function DirPicker({ initial, onPick, onConfirm, onClose }) {
  const dialog = useRef(null);
  const titleId = useId();
  const pathId = useId();
  const pressedOutside = useRef(false);
  useLayoutEffect(() => {
    const element = dialog.current;
    element.showModal();
    return () => element.close();
  }, []);
  const outside = event => {
    const bounds = dialog.current.getBoundingClientRect();
    return event.clientX < bounds.left || event.clientX > bounds.right || event.clientY < bounds.top || event.clientY > bounds.bottom;
  };
  const [path, setPath] = useState(initial && initial.startsWith('/') ? initial : '/data');
  const [dirs, setDirs] = useState([]);
  const [parent, setParent] = useState('');
  const [err, setErr] = useState('');

  const browse = useCallback(async (p) => {
    try {
      const r = await (await client()).browse(p);
      setPath(r.path);
      setParent(r.parent || '');
      setDirs(r.dirs || []);
      onPick(r.path);
      setErr('');
    } catch (e) {
      setErr(String(e?.message || e));
    }
  }, [onPick]);

  useEffect(() => { browse(initial && initial.startsWith('/') ? initial : '/data'); }, []);
  useEffect(() => {
    const t = setTimeout(() => { if (path.startsWith('/')) browse(path).catch(() => {}); }, 600);
    return () => clearTimeout(t);
  }, [path]);

  return (
    <dialog ref={dialog} className="modal directory-dialog" aria-labelledby={titleId}
      onCancel={event => { event.preventDefault(); onClose(); }}
      onPointerDown={event => { pressedOutside.current = outside(event); }}
      onPointerUp={event => { if (pressedOutside.current && outside(event)) onClose(); pressedOutside.current = false; }}
      onPointerCancel={() => { pressedOutside.current = false; }}>
        <h2 id={titleId}>{t('选择目录')}</h2>
        <label htmlFor={pathId}>{t('路径')}</label>
        <div className="row">
          <input id={pathId} autoFocus value={path} onChange={(e) => { setPath(e.target.value); onPick(e.target.value); }} placeholder={t("/srv/files（可直接输入 path）")} className="mono" />
          <button type="button" className="ghost" disabled={!parent} onClick={() => browse(parent)}>{t('.. 上级')}</button>
        </div>
        {err && <p className="error" role="alert">{err}</p>}
        <div className="dirlist">
          {dirs.map((d) => (
            <button key={d.path} type="button" className="diritem" onDoubleClick={() => browse(d.path)} onClick={() => { setPath(d.path); onPick(d.path); }}>
              <span className="mono">{d.name}/</span>
              <span className="hint">{t('双击进入')}</span>
            </button>
          ))}
          {dirs.length === 0 && <p className="hint">{t('无子目录（可直接输入 path 后选择）。')}</p>}
        </div>
        <div className="row" style={{ justifyContent: 'flex-end' }}>
          <button type="button" className="ghost" onClick={onClose}>{t('取消')}</button>
          <button type="button" onClick={() => onConfirm(path)}>{t('选择此目录')}</button>
        </div>
    </dialog>
  );
}

function CollectorPage() {
  const [data, setData] = useState(null);
  const [hist, setHist] = useState([]);
  const [err, setErr] = useState('');
  const [descSeed, setDescSeed] = useState('');
  const [descText, setDescText] = useState('');

  const load = useCallback(async () => {
    try {
      const s = await (await client()).collectorStatus();
      setData(s);
      const h = await (await client()).collectorHistory();
      setHist(h.submissions || []);
      setErr('');
    } catch (e) {
      setErr(String(e?.message || e));
    }
  }, []);
  useEffect(() => {
    load();
    // 统一订阅替代 8s 轮询：采集状态变化时重载。
    return watchTopics(['collector'], () => load());
  }, [load]);

  const flush = async () => {
    try {
      const r = await (await client()).collectorFlush();
      setErr('');
      await load();
      if ((r.triggered || []).length === 0) setErr(t('无待提交队列'));
    } catch (e) {
      setErr(String(e?.message || e));
    }
  };
  const retry = async () => {
    try {
      const r = await (await client()).collectorRetry();
      setErr('');
      await load();
      if ((r.retried || []).length === 0) setErr(t('无发送中批次'));
    } catch (e) {
      setErr(String(e?.message || e));
    }
  };
  const saveDesc = async (e) => {
    e.preventDefault();
    try {
      await (await client()).collectorDescribe(descSeed.trim().toLowerCase(), descText);
      setDescSeed('');
      setDescText('');
      await load();
    } catch (e) {
      setErr(String(e?.message || e));
    }
  };

  return (
    <div>
      <section className="card">
        <div className="section-head"><h2>{t('采集目标')}</h2><span className="hint">{t('每次提交发往每个启用的 Collector')}</span></div>
        {err && <p className="error">{err}</p>}
        <table>
          <thead><tr><th>{t('名称')}</th><th>{t('ID')}</th><th>{t('端点')}</th><th>{t('启用')}</th><th>{t('待提交')}</th><th>{t('已确认')}</th><th>{t('发送中')}</th><th>{t('最近成功')}</th><th>{t('失败')}</th></tr></thead>
          <tbody>
            {(data?.collectors || []).map((c) => (
              <tr key={c.id}>
                <td>{c.name || '—'}</td>
                <td className="mono">{c.id}</td>
                <td className="mono">{c.endpoint || '—'}</td>
                <td>{c.enabled ? t('是') : t('否')}</td>
                <td>{c.pendingCount}</td>
                <td>{c.confirmedCount}</td>
                <td>{c.inflight ? t('是') : t('否')}</td>
                <td className="mono">{c.lastSuccessAt ? new Date(c.lastSuccessAt).toLocaleString() : '—'}</td>
                <td>{c.lastError || '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <p className="hint">{t('间隔：提前触发')}{data?.intervals?.earlyTriggerCount}{t('条 · 周期')}{data?.intervals?.submitIntervalMs}{t('ms · 退避')}{data?.intervals?.retryBaseMs}{t('→')}{data?.intervals?.retryCapMs}{t('ms。官方端点：')}<span className="mono">{data?.officialEndpoint || '—'}</span>{t('（待定）。目标与间隔在设置页修改。')}</p>
        <div className="row">
          <button type="button" className="ghost" onClick={flush}>{t('立即提交')}</button>
          <button type="button" className="ghost" onClick={retry}>{t('重发发送中批次')}</button>
        </div>
      </section>
      <section className="card" style={{ marginTop: 12 }}>
        <h2>{t('文件介绍')}</h2>
        <form onSubmit={saveDesc}>
          <div className="row">
            <label className="inline-field" htmlFor="collector-description-seed">{t('seedHashHex')}<input id="collector-description-seed" value={descSeed} onChange={(e) => setDescSeed(e.target.value)} placeholder="seedHashHex" className="mono" /></label>
            <label className="inline-field" htmlFor="collector-description-text">{t('介绍')}<input id="collector-description-text" value={descText} onChange={(e) => setDescText(e.target.value)} placeholder={t("介绍（≤2048 字节，存采集自有存储）")} /></label>
            <button type="submit">{t('保存介绍并重入队')}</button>
          </div>
        </form>
      </section>
      <section className="card" style={{ marginTop: 12 }}>
        <h2>{t('操作历史')}{t('（{n}）', { n: hist.length })}</h2>
        <table>
          <thead><tr><th>{t('时间')}</th><th>Collector</th><th>{t('触发')}</th><th>{t('条数')}</th><th>{t('回执')}</th></tr></thead>
          <tbody>
            {hist.map((h, i) => (
              <tr key={i}>
                <td className="mono">{new Date(h.at).toLocaleString()}</td>
                <td className="mono">{h.collectorId}</td>
                <td>{h.trigger}</td>
                <td>{h.count}</td>
                <td className="mono">{h.archiveId}</td>
              </tr>
            ))}
          </tbody>
        </table>
        {hist.length === 0 && <p className="hint">{t('暂无提交成功记录。')}</p>}
      </section>
    </div>
  );
}

function SettingsPage({ onTheme }) {
  const [s, setS] = useState(null);
  const [err, setErr] = useState('');
  const [saved, setSaved] = useState('');
  const [conflict, setConflict] = useState('');
  // 正在编辑的草稿标记：收到设置变化事件时不静默覆盖，改为提示冲突。
  const dirtyRef = useRef(false);

  const load = useCallback(async () => {
    try {
      const v = await (await client()).getSettings();
      setS(v);
      dirtyRef.current = false;
      setConflict('');
      setErr('');
    } catch (e) {
      setErr(String(e?.message || e));
    }
  }, []);
  useEffect(() => { load(); }, [load]);
  // 订阅设置主题：他人修改或恢复出厂时，非编辑态直接重载，
  // 编辑中只提示冲突，由用户决定刷新或覆盖保存。
  useEffect(() => watchTopics(['settings'], () => {
    if (dirtyRef.current) {
      setConflict(t('设置已在别处修改，当前草稿可能过期；刷新将丢失草稿，保存将按表单 revision 覆盖。'));
    } else {
      return load();
    }
  }), [load]);
  useEffect(() => { if (s) onTheme(s.theme); }, [s, onTheme]);

  if (!s) return <section className="card"><h2>{t('设置')}</h2>{err ? <p className="error">{err}</p> : <p className="hint">{t('加载中…')}</p>}</section>;

  const set = (patch) => { dirtyRef.current = true; setS({ ...s, ...patch }); };
  const setSched = (patch) => { dirtyRef.current = true; setS({ ...s, scheduler: { ...s.scheduler, ...patch } }); };
  const setServer = (patch) => { dirtyRef.current = true; setS({ ...s, server: { ...s.server, ...patch } }); };
  const setCollector = (patch) => { dirtyRef.current = true; setS({ ...s, server: { ...s.server, collector: { ...s.server.collector, ...patch } } }); };

  const save = async (e) => {
    e.preventDefault();
    setErr(''); setSaved('');
    try {
      const updated = await (await client()).updateSettings({
          revision: s.revision,
          port: Number(s.port),
          theme: s.theme,
          applicationOpenMode: s.applicationOpenMode,
          language: s.language,
          wocApiKey: s.wocApiKey || '',
          feeRateSatoshisPerKb: Number(s.feeRateSatoshisPerKb),
          scheduler: {
            maxBlockPriceSatoshis: String(s.scheduler.maxBlockPriceSatoshis ?? '0'),
            poolBlocks: Number(s.scheduler.poolBlocks),
            totalSeats: Number(s.scheduler.totalSeats),
            floatingSeats: Number(s.scheduler.floatingSeats),
            requestTimeoutMs: Number(s.scheduler.requestTimeoutMs),
          },
          server: {
            defaultSeedPriceSatoshis: String(s.server.defaultSeedPriceSatoshis),
            defaultFullBlockPriceSatoshis: String(s.server.defaultFullBlockPriceSatoshis),
            arbiterPublicKeys: (s.server.arbiterPublicKeys || []),
            collector: {
              collectors: (s.server.collector?.collectors || []),
              earlyTriggerCount: Number(s.server.collector?.earlyTriggerCount ?? 1001),
              submitIntervalMs: Number(s.server.collector?.submitIntervalMs ?? 3600000),
              retryBaseMs: Number(s.server.collector?.retryBaseMs ?? 30000),
              retryCapMs: Number(s.server.collector?.retryCapMs ?? 3600000),
            },
          },
        });
      setS(updated);
      dirtyRef.current = false;
      setConflict('');
      setSaved(t('已保存（revision {rev}）。端口修改需重启后台生效。', { rev: updated.revision }));
    } catch (e) {
      setErr(String(e?.message || e));
    }
  };

  const reset = async () => {
    setErr(''); setSaved('');
    try {
      const updated = await (await client()).resetSettings(s.revision);
      setS(updated);
      dirtyRef.current = false;
      setConflict('');
      setSaved(t('已恢复出厂（普通参数回到默认，网络身份与售卖根保留）。'));
    } catch (e) {
      setErr(String(e?.message || e));
    }
  };

  const num = (v, fn) => (
    <input type="number" value={v ?? ''} onChange={(e) => fn(e.target.value === '' ? 0 : Number(e.target.value))} />
  );

  return (
    <section className="card">
      <h2>{t('设置（revision {rev}）', { rev: s.revision })}</h2>
      {err && <p className="error">{err}</p>}
      {conflict && <p className="error">{conflict}</p>}
      {saved && <p className="hint">{saved}</p>}
      <form onSubmit={save}>
        <h3>{t('通用')}</h3>
        <div className="formgrid">
          <label>{t('端口（重启生效）')}{num(s.port, (v) => set({ port: v }))}</label>
          <label>{t('主题')}
            <select value={s.theme} onChange={(e) => set({ theme: e.target.value })}>
              <option value="system">{t('跟随系统')}</option>
              <option value="light">{t('浅色')}</option>
              <option value="dark">{t('深色')}</option>
            </select>
          </label>
          <label>{t('语言')}
            <select value={['system', 'zh', 'en', 'ja'].includes(s.language) ? s.language : 'system'} onChange={(e) => set({ language: e.target.value })}>
              <option value="system">{t('跟随系统')} (system)</option>
              <option value="zh">中文</option>
              <option value="en">English</option>
              <option value="ja">日本語</option>
            </select>
          </label>
          <label>应用打开方式
            <select data-testid="application-open-mode" value={s.applicationOpenMode} onChange={e => set({ applicationOpenMode: e.target.value })}>
              <option value="iframe">内嵌 Tab（默认）</option><option value="popup">新窗口</option>
            </select>
            <small>仅影响之后打开的应用，已打开应用保持原方式。</small>
          </label>
          <label>{t('WoC API Key')}<input value={s.wocApiKey || ''} onChange={(e) => set({ wocApiKey: e.target.value })} autoComplete="off" placeholder={t('可选')} /></label>
          <label>{t('手续费率 sats/KB')}<input data-testid="settings-fee-rate" type="number" value={s.feeRateSatoshisPerKb ?? ''} onChange={(e) => set({ feeRateSatoshisPerKb: e.target.value === '' ? 0 : Number(e.target.value) })} /></label>
        </div>
        <h3>{t('调度')}</h3>
        <div className="formgrid">
          <label>{t('块价上限 sats')}<input value={s.scheduler.maxBlockPriceSatoshis} onChange={(e) => setSched({ maxBlockPriceSatoshis: e.target.value })} className="mono" /></label>
          <label>{t('池块数')}{num(s.scheduler.poolBlocks, (v) => setSched({ poolBlocks: v }))}</label>
          <label>{t('总席位')}{num(s.scheduler.totalSeats, (v) => setSched({ totalSeats: v }))}</label>
          <label>{t('浮动席位')}{num(s.scheduler.floatingSeats, (v) => setSched({ floatingSeats: v }))}</label>
          <label>{t('请求超时 ms')}{num(s.scheduler.requestTimeoutMs, (v) => setSched({ requestTimeoutMs: v }))}</label>
        </div>
        <h3>{t('售卖缺省价')}</h3>
        <div className="formgrid">
          <label>{t('Seed 缺省 sats')}<input value={s.server.defaultSeedPriceSatoshis} onChange={(e) => setServer({ defaultSeedPriceSatoshis: e.target.value })} className="mono" /></label>
          <label>{t('完整块缺省 sats')}<input value={s.server.defaultFullBlockPriceSatoshis} onChange={(e) => setServer({ defaultFullBlockPriceSatoshis: e.target.value })} className="mono" /></label>
        </div>
        <h3>{t('采集上报（多目标，每次提交发往每个启用的 Collector）')}</h3>
        {(s.server.collector?.collectors || []).map((c, i) => (
          <div className="card" key={c.id || i} style={{ marginBottom: 8 }}>
            <div className="row">
              <label className="inline-field" htmlFor={`collector-${i}-name`}>{t('采集器名称')}<input id={`collector-${i}-name`} value={c.name || ''} onChange={(e) => {
                const list = [...(s.server.collector.collectors || [])];
                list[i] = { ...list[i], name: e.target.value };
                setCollector({ collectors: list });
              }} placeholder={t("名称（如 官方）")} style={{ maxWidth: 140 }} /></label>
              <label className="inline-field" htmlFor={`collector-${i}-endpoint`}>{t('采集器 URL')}<input id={`collector-${i}-endpoint`} value={c.endpoint || ''} onChange={(e) => {
                const list = [...(s.server.collector.collectors || [])];
                list[i] = { ...list[i], endpoint: e.target.value };
                setCollector({ collectors: list });
              }} placeholder={t("https://collector.example.com（完整提交 URL，官方地址待定）")} className="mono" /></label>
              <label className="check"><input type="checkbox" checked={!!c.enabled} onChange={(e) => {
                const list = [...(s.server.collector.collectors || [])];
                list[i] = { ...list[i], enabled: e.target.checked };
                setCollector({ collectors: list });
              }} /> {t('启用')}</label>
              <button type="button" className="danger" onClick={() => {
                setCollector({ collectors: (s.server.collector.collectors || []).filter((_, j) => j !== i) });
              }}>{t('删除')}</button>
            </div>
            <p className="hint mono">{c.id || t('(新条目保存时分配 ID)')}</p>
          </div>
        ))}
        <div className="row">
          <button type="button" className="ghost" onClick={() => {
            const id = 'c_new_' + Math.random().toString(16).slice(2, 10);
            setCollector({ collectors: [...(s.server.collector.collectors || []), { id, name: '', endpoint: '', enabled: false }] });
          }}>{t('添加 Collector')}</button>
        </div>
        <h3>{t('采集任务间隔')}</h3>
        <div className="formgrid">
          <label>{t('提前触发条数')}<input type="number" value={s.server.collector?.earlyTriggerCount ?? 1001} onChange={(e) => setCollector({ earlyTriggerCount: Number(e.target.value) })} /></label>
          <label>{t('周期提交间隔 ms')}<input type="number" value={s.server.collector?.submitIntervalMs ?? 3600000} onChange={(e) => setCollector({ submitIntervalMs: Number(e.target.value) })} /></label>
          <label>{t('退避基线 ms')}<input type="number" value={s.server.collector?.retryBaseMs ?? 30000} onChange={(e) => setCollector({ retryBaseMs: Number(e.target.value) })} /></label>
          <label>{t('退避上限 ms')}<input type="number" value={s.server.collector?.retryCapMs ?? 3600000} onChange={(e) => setCollector({ retryCapMs: Number(e.target.value) })} /></label>
        </div>
        <h3>{t('仲裁公钥（每行一个，可空）')}</h3>
        <textarea rows={3} value={(s.server.arbiterPublicKeys || []).join('\n')} onChange={(e) => setServer({ arbiterPublicKeys: e.target.value.split('\n').map((x) => x.trim()).filter(Boolean) })} className="mono" />
        <div className="row" style={{ marginTop: 12 }}>
          <button type="submit">{t('保存设置')}</button>
          <button type="button" className="danger" onClick={reset}>{t('恢复出厂')}</button>
        </div>
        <p className="hint">{t('恢复出厂只重置本页参数（端口回默认），网络身份、售卖根与业务数据保留。并发修改用 revision 拒绝旧页覆盖，请刷新后重试。')}</p>
      </form>
    </section>
  );
}

/** 工作区持有应用，内部路由导航不会结束应用连接。 */

function App() {
  const pageFromLocation = () => window.location.pathname.startsWith('/app/') ? window.location.pathname : TABS.find(tab => `/${tab.id}` === window.location.pathname)?.id ?? (window.location.pathname === '/settings/plugins' ? 'plugins' : 'overview');
  const [page, updatePage] = useState(pageFromLocation);
  const [sidebarOpen, setSidebarOpen] = useState(true);
  const setPage = (next) => {
    workspace.select(null);
    const path = next === 'overview' ? '/' : next === 'plugins' ? '/settings/plugins' : next.startsWith('/') ? next : `/${next}`;
    if (window.location.pathname !== path) window.history.pushState(null, '', path);
    updatePage(pageFromLocation());
  };
  useEffect(() => {
    const onPopState = () => { updatePage(pageFromLocation()); workspace.select(null); };
    window.addEventListener('popstate', onPopState);
    return () => window.removeEventListener('popstate', onPopState);
  }, []);
  const [theme, setTheme] = useState('system');
  const [lang, setLangUi] = useState('zh');
  const [status, setStatus] = useState(null);
  const initializing = useRef(false);
  const unlockCredential = useRef({ password: '' });
  const loadSequence = useRef(0);
  useEffect(() => {
    const clear = () => { unlockCredential.current.password = ''; };
    window.addEventListener('pagehide', clear);
    return () => { clear(); window.removeEventListener('pagehide', clear); };
  }, []);
  const [info, setInfo] = useState(null);
  const [roots, setRoots] = useState(null);
  const [files, setFiles] = useState(null);
  const [tasks, setTasks] = useState([]);
  const [error, setError] = useState('');
  const [seed, setSeed] = useState('');
  const [newRoot, setNewRoot] = useState('');
  const [pickerOpen, setPickerOpen] = useState(false);
  const [conn, setConn] = useState('connecting');
  const instanceRef = useRef('');
  const workspace = useWorkspace({ status, info, conn });

  useAppliedTheme(theme);

  useEffect(() => onConnectionStatus(setConn), []);
  useEffect(() => {
    if (conn !== 'connected') {
      ++loadSequence.current;
      unlockCredential.current.password = '';
      setStatus(null); setInfo(null);
    }
  }, [conn]);

  // 向 opener（App 弹窗模式）发布 ready：仅当后台已连接、已初始化且已解锁。
  // ready 只表示传输可用，不携带授权；官方 SDK 凭 origin 校验发送方。
  // 每个后台实例只发布一次；重启/重连后实例变化会再次发布。
  useEffect(() => {
    if (conn !== 'connected' || !status?.initialized || (info?.locked ?? status?.locked)) return;
    const iid = getInstanceId();
    if (!iid || instanceRef.current === iid) return;
    instanceRef.current = iid;
    try {
      const opener = window.opener;
      if (opener && !opener.closed) opener.postMessage({ v: 1, type: 'ready' }, '*');
    } catch { /* ignore */ }
  }, [conn, status, info]);
  // 独立启动窗口与 popup 传输对端的连接管理归钱包整体生命周期：
  // 定期收殓已关闭窗口与已关闭的应用方卸载时 teardown 全部启动连接并尽力发送官方 closing 通知；
  // 视图由工作区持有，菜单和 Tab 切换不终止应用连接。
  useEffect(() => {
    if (conn !== 'connected' || !status?.initialized || status.locked) return;
    initConnectReceiver().catch(() => {});
    const t = setInterval(() => { try { sweepClosedLaunchedApps(); sweepClosedPopup(); } catch { /* ignore */ } }, 2000);
    const onHide = () => { try { teardownAllLaunchedApps(); } catch { /* ignore */ } };
    window.addEventListener('pagehide', onHide);
    return () => { clearInterval(t); window.removeEventListener('pagehide', onHide); };
  }, [conn, status?.initialized, status?.locked]);
  // 原请求窗口订阅后台 connect 变化，收到决定后复核 Receiver 上下文再认领执行。
  // 重连后读完整快照再触发认领，不依赖某一页内存或仅依赖通知消息；无固定轮询兜底。
  useEffect(() => {
    if (conn !== 'connected' || !status?.initialized || status.locked) return;
    return watchTopics(['connect'], () => pumpPendingApprovals().catch(() => {}));
  }, [conn, status?.initialized, status?.locked]);

  const load = useCallback(async () => {
    const sequence = ++loadSequence.current;
    const current = c => sequence === loadSequence.current && getConnectionStatus() === 'connected' && c.getInstanceId() === getInstanceId() && !initializing.current;
    try {
      const c = await client();
      const s = await c.status();
      if (!current(c)) return;
      setStatus(s);
      if (!s.initialized || s.locked || s.state !== 'unlocked') {
        setInfo(null); setRoots(null); setFiles(null); setTasks([]); setError('');
        return;
      }
      const i = await c.info();
      if (!current(c)) return;
      setInfo(i);
      const r = await c.listRoots().catch(() => null);
      if (!current(c)) return;
      setRoots(r);
      const f = await c.listFiles().catch(() => null);
      if (!current(c)) return;
      setFiles(f);
      const t = await c.listTasks().catch(() => ({ tasks: [] }));
      if (!current(c)) return;
      setTasks(t.tasks || []);
      const st = await c.getSettings().catch(() => null);
      if (!current(c)) return;
      if (st?.theme) setTheme(st.theme);
      {
        const L = resolveLang(st?.language);
        setLang(L);
        setLangUi(L);
        document.documentElement.lang = L === 'zh' ? 'zh-CN' : L;
      }
      setError('');
    } catch (e) {
      setError(String(e?.message || e));
    }
  }, []);

  useEffect(() => {
    load();
    // 统一订阅替代 5s 整页轮询：快照与事件衔接无遗漏，有变化时重载。
    return watchTopics([], () => load());
  }, [load]);

  const submitTask = async (e) => {
    e.preventDefault();
    setError('');
    try {
      await (await client()).createTask(seed.trim().toLowerCase());
      setSeed('');
      await load();
    } catch (e) {
      setError(String(e?.message || e));
    }
  };

  const cancelTask = async (seedHashHex) => {
    try {
      await (await client()).cancelTask(seedHashHex);
      await load();
    } catch (e) {
      setError(String(e?.message || e));
    }
  };

  const addRootPath = async (dirPath) => {
    if (!roots) return;
    const p = (dirPath || '').trim();
    if (!p) return;
    try {
      await (await client()).addRoot(p, roots.revision);
      setNewRoot('');
      setPickerOpen(false);
      await load();
    } catch (e) {
      setError(String(e?.message || e));
    }
  };

  const addRoot = async (e) => {
    e.preventDefault();
    await addRootPath(newRoot);
  };

  const setDefault = async (id) => {
    try {
      await (await client()).setDefaultRoot(id, roots.revision);
      await load();
    } catch (e) {
      setError(String(e?.message || e));
    }
  };

  const removeRoot = async (id) => {
    try {
      await (await client()).removeRoot(id, roots.revision);
      await load();
    } catch (e) {
      setError(String(e?.message || e));
    }
  };

  const refreshRoots = async (rootId) => {
    try {
      await (await client()).refreshCatalog(rootId || undefined, false);
      await load();
    } catch (e) {
      setError(String(e?.message || e));
    }
  };

  const copy = async (text) => {
    try {
      await navigator.clipboard.writeText(text);
    } catch { /* clipboard unavailable; selection still visible */ }
  };

  const rootPath = (id) => (roots?.roots || []).find((r) => r.id === id)?.path || '—';
  const defaultRootPath = rootPath(roots?.defaultDownloadRootID);

  // /settings/plugins 在未连接、未初始化、锁定、全新未创建钱包时都能打开：
  // 该路由的渲染判断放在初始化与解锁页面门禁之前，只允许读取诊断，不扩大业务方法。
  // 已解锁且已连接时走正常工作区布局（侧边菜单+Tab）；仅异常/未就绪时用 WalletFrame 承载。
  const pluginsStandalone = typeof window !== 'undefined' && window.location.pathname === '/settings/plugins'
    && !(conn === 'connected' && status && status.initialized && !status.locked && status.state === 'unlocked');
  if (pluginsStandalone) return <WalletFrame wide><PluginDiagnosticsPage standalone /></WalletFrame>;
  if (conn !== 'connected' || !status) return <WalletFrame><h1>{conn === 'protocol-incompatible' ? t('协议不兼容') : conn === 'disconnected' ? t('后台连接已断开') : '正在读取钱包状态…'}</h1>{error && <p role="alert" className="error">{error}</p>}<div className="row"><button type="button" onClick={() => { void retryConnection().then(load).catch(cause => setError(String(cause.message || cause))); }}>{t('重试')}</button><button type="button" className="ghost" onClick={() => { window.location.href = '/settings/plugins'; }}>{t('插件')}</button></div><p className="hint">{t('该页面不触发创建钱包、解锁或业务初始化')}</p></WalletFrame>;
  if (!status.initialized) return <WalletOnboarding onImporting={value => { initializing.current = value; if (!value) void load(); }} onImported={password => {
    unlockCredential.current.password = password;
    initializing.current = false;
    ++loadSequence.current;
    setStatus({ initialized: true, locked: true, state: 'locked' }); setError('');
  }} />;
  if (status.locked || status.state !== 'unlocked') return <WalletUnlock credential={unlockCredential.current} onReady={load} />;

  const title = page.startsWith('/app/') ? '应用详情' : t(TABS.find(tab => tab.id === page)?.label || 'Keymaster 后台');
  return (
    <ApplicationWorkspace.Provider value={workspace}>
    <div className="layout">
        <header className="topbar">
          <button type="button" className="workspace-brand" aria-label="Keymaster 首页" onClick={() => setPage('overview')}><img src="/favicon.svg" alt="" /><strong>Keymaster</strong></button>
          <span className="spacer" />
          <span className="badge" data-testid="connection-state" data-state={conn}>{conn === 'connected' ? t('已连接') : conn === 'connecting' ? t('连接中') : conn === 'protocol-incompatible' ? t('协议不兼容') : t('已断开')}</span>
          {conn === 'disconnected' && <button type="button" className="ghost" onClick={() => retryConnection()}>{t('重试')}</button>}
          <GlobalApproval wallet={{ network: info?.network, address: info?.address || status?.address }} showSettings={page === 'settings'} />
          <StateBadge state={conn === 'connected' ? status?.state : undefined} locked={status?.locked ?? info?.locked} />
          {info?.network && <span className="badge">{info.network} · {info?.address ? `${info.address.slice(0, 8)}…` : ''}</span>}
        </header>
      <div className="workspace-body">
      <aside id="workspace-menu" className="sidebar" hidden={!sidebarOpen}>
        {workspace.views.length > 0 && <nav className="workspace-app-menu" aria-label="已打开应用">
          <div className="workspace-menu-caption">应用</div>
          {workspace.views.map(view => <div className="workspace-app-menu-row" key={view.id}>
            <button type="button" aria-current={workspace.active === view.id ? 'true' : undefined} onClick={() => workspace.select(view.id)}>{view.title}</button>
            <button type="button" className="workspace-close" aria-label={`关闭 ${view.title}`} onClick={() => workspace.close(view.id)}>×</button>
          </div>)}
        </nav>}
        <div className="workspace-menu-caption">菜单</div>
        <nav className="nav" aria-label="钱包功能">
          {TABS.map(tab => <button key={tab.id} type="button" data-testid={`nav-${tab.id}`} className={!workspace.active && page === tab.id ? 'active' : ''} onClick={() => setPage(tab.id)}>{t(tab.label)}</button>)}
        </nav>
      </aside>
      <main className="main">
        <div className="workspace-tabs-row">
          <button type="button" className="workspace-sidebar-toggle" aria-controls="workspace-menu" aria-expanded={sidebarOpen} aria-label={sidebarOpen ? '收起菜单' : '展开菜单'} onClick={() => setSidebarOpen(open => !open)}>☰</button>
          <WorkspaceTabs workspace={workspace} title={title} />
        </div>
        <div className="workspace-route" role="tabpanel" aria-labelledby="internal-tab" hidden={!!workspace.active}>
        <div className="content">
          {workspace.error && <p className="error" role="alert">{workspace.error}</p>}
          {error && <p className="error">{error}</p>}
      {page === 'overview' && (
        <>
        <OverviewApps onBrowse={() => setPage('apps')} />
        <div className="grid">
          <section className="card">
            <h2>{t('钱包')}</h2>
            <dl className="kv">
              <dt>{t('地址')}</dt>
              <dd>
                <span className="addr">
                  <code className="mono">{info?.address || status?.address || '—'}</code>
                  {(info?.address || status?.address) && <button className="ghost" onClick={() => copy(info?.address || status?.address)}>{t('复制')}</button>}
                </span>
              </dd>
              <dt>{t('总额')}</dt><dd>{info?.totalSats ?? '—'} {t('sats')}</dd>
              <dt>{t('可用 / 占用')}</dt><dd>{info?.availableSats ?? '—'} / {info?.allocatedSats ?? '—'} {t('sats')}</dd>
              <dt>{t('查询')}</dt>
              <dd>
                {info?.queryTime || '—'}
                {info?.cached ? t('（缓存，非实时）') : ''}
                {info?.error && <span> · {t('失败：')}{info.error}</span>}
              </dd>
            </dl>
            <p className="hint">{t('查询失败不显示为零；缓存会明确标注时间。')}</p>
          </section>


        </div>
        </>
      )}

      {page === 'sell' && (
        <section className="card">
          <div className="section-head"><h2>{t('售卖根')}{t('（{n}）', { n: roots?.roots?.length ?? 0 })}{t('· 默认下载根：')}{defaultRootPath}</h2><button type="button" className="ghost" onClick={() => refreshRoots()}>{t('刷新目录')}</button></div>
          <p className="hint">{t('后台每 5 秒自动核对（新增/长度变化才重算）并支持 SIGHUP；这里可手动立即刷新。')}</p>
          <form onSubmit={addRoot}>
            <div className="row">
              <label className="inline-field" htmlFor="sell-root-path">{t('售卖目录')}<input id="sell-root-path" value={newRoot} onChange={(e) => setNewRoot(e.target.value)} placeholder={t("/srv/files（后台可读目录，可直接输入）")} /></label>
              <button type="button" className="ghost" onClick={() => { setPickerOpen(true); }}>{t('浏览…')}</button>
              <button type="submit">{t('添加')}</button>
            </div>
          </form>
          {pickerOpen && <DirPicker initial={newRoot || '/data'} onPick={(p) => { setNewRoot(p); }} onConfirm={(p) => addRootPath(p)} onClose={() => setPickerOpen(false)} />}
          <table>
            <thead><tr><th>{t('路径')}</th><th>{t('可达')}</th><th>{t('操作')}</th></tr></thead>
            <tbody>
              {(roots?.roots || []).map((r) => (
                <tr key={r.id}>
                  <td className="mono">{r.path}{roots?.defaultDownloadRootID === r.id && ' ★'}</td>
                  <td>{r.accessible ? t('是') : `${t('否')}: ${r.error || ''}`}</td>
                  <td>
                    <div className="row">
                      <button className="ghost" disabled={roots?.defaultDownloadRootID === r.id} onClick={() => setDefault(r.id)}>{t('设为默认')}</button>
                      <button className="ghost" onClick={() => refreshRoots(r.id)}>{t('刷新')}</button>
                      <button className="danger" disabled={roots?.defaultDownloadRootID === r.id} title={roots?.defaultDownloadRootID === r.id ? t('删除默认根前须另选默认') : ''} onClick={() => removeRoot(r.id)}>{t('删除')}</button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          <p className="hint">{t('删除仅停新售卖，不删真实文件与历史资金责任；在途会话保持原目录绑定，不改用他根同名文件。')}</p>
        </section>
      )}

      {page === 'get' && (
        <section className="card">
          <h2>{t('下载任务')}</h2>
          <form onSubmit={submitTask}>
            <div className="row">
              <label className="inline-field" htmlFor="download-seed">{t('seedhashhex')}<input id="download-seed" value={seed} onChange={(e) => setSeed(e.target.value)} placeholder={t("seedhashhex（64 hex），重复提交复用任务")} className="mono" /></label>
              <button type="submit">{t('提交 get')}</button>
            </div>
          </form>
          <table>
            <thead><tr><th>{t('seed')}</th><th>{t('状态')}</th><th>{t('进度')}</th><th>{t('完成路径')}</th><th>{t('操作')}</th></tr></thead>
            <tbody>
              {tasks.map((task) => (
                <tr key={task.seedHashHex}>
                  <td className="mono">{task.seedHashHex.slice(0, 12)}…</td>
                  <td>{{pending: t('待提交'), running: t('运行中'), done: t('已完成'), failed: t('失败'), cancelled: t('已取消')}[task.state] || task.state}{task.detail ? ` · ${task.detail}` : ''}{task.error ? ` · ${task.error}` : ''}</td>
                  <td>
                    <div className="progress"><i style={{ width: `${task.blocksTotal ? Math.round((task.blocksDone / task.blocksTotal) * 100) : 0}%` }} /></div>
                    <span className="hint">{task.blocksDone}/{task.blocksTotal}</span>
                  </td>
                  <td className="mono">{task.finalPath || '—'}</td>
                  <td>{['pending', 'running'].includes(task.state) && <button className="danger" onClick={() => cancelTask(task.seedHashHex)}>{t('取消')}</button>}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <p className="hint">{t('前台断开只停显示，后台继续；取消为明确任务操作，有界收尾不删证据。')}</p>
        </section>
      )}

      {page === 'files' && (
        <section className="card">
          <h2>{t('文件')}{t('（{n}）', { n: files?.files?.length ?? 0 })}</h2>
          <table>
            <thead><tr><th>{t('根')}</th><th>{t('路径')}</th><th>{t('大小')}</th><th>{t('seed')}</th><th>{t('价格')}</th><th>{t('状态')}</th></tr></thead>
            <tbody>
              {(files?.files || []).slice(0, 100).map((f) => (
                <tr key={`${f.rootId}:${f.path}`}>
                  <td className="mono">{rootPath(f.rootId)}</td>
                  <td className="mono">{f.path}</td>
                  <td>{f.size ?? '—'}</td>
                  <td className="mono">{f.seedHash?.slice(0, 12)}…</td>
                  <td>{f.seedPrice}/{f.blockPrice}{f.override ? '*' : ''}</td>
                  <td>{f.state}{f.error ? ` · ${f.error}` : ''}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <p className="hint">{t('身份为 根路径+相对路径；* 为文件独立价。目录不可访问显错，不自动删除。')}</p>
        </section>
      )}

      {page === 'collect' && <CollectorPage />}

      {page === 'settings' && <><SettingsPage onTheme={setTheme} /><section className="card"><h2>应用通知</h2><div id="application-reminders-settings" /></section></>}

      {page === 'apps' && <MarketPage navigate={setPage} />}
      {page.startsWith('/app/') && <AppDetailPage key={page} appId={page.slice(5)} navigate={setPage} />}
      {page === 'plugins' && <PluginDiagnosticsPage />}

          <p className="hint">{t('主题跟随系统或设置页选择（settings 唯一来源，本页不写 localStorage）。远程访问请自备反向代理与认证。')}</p>
        </div>
        </div>
        <div className="workspace-app-outlet" role="tabpanel" aria-labelledby={workspace.active ? `tab-${encodeURIComponent(workspace.active)}` : undefined} ref={workspace.outletRef} hidden={!workspace.active} />
      </main>
      </div>
    </div>

    </ApplicationWorkspace.Provider>
  );
}

createRoot(document.getElementById('root')).render(<App />);
