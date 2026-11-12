import React, { useCallback, useEffect, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import './styles.css';
import { t, setLang, resolveLang } from './i18n.js';
import { watchTopics } from './subscribe.js';
import { getClient, getInstanceId, onConnectionStatus, retryConnection } from './client.js';
import { initConnectReceiver, issueLaunchToken, registerLaunchedApp, teardownLaunchedApp, teardownAllLaunchedApps, sweepClosedLaunchedApps, sweepClosedPopup, pumpPendingApprovals } from './connect.js';
import { GlobalApproval, AppCenter } from './approvalCenter.jsx';

const TABS = [
  { id: 'overview', label: '概览' },
  { id: 'sell', label: '售卖' },
  { id: 'get', label: '下载' },
  { id: 'files', label: '文件' },
  { id: 'collect', label: '采集' },
  { id: 'settings', label: '设置' },
  { id: 'connect', label: '连接' },
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
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h3>{t('选择目录')}</h3>
        <div className="row">
          <input value={path} onChange={(e) => { setPath(e.target.value); onPick(e.target.value); }} placeholder={t("/srv/files（可直接输入 path）")} className="mono" />
          <button type="button" className="ghost" disabled={!parent} onClick={() => browse(parent)}>{t('.. 上级')}</button>
        </div>
        {err && <p className="error">{err}</p>}
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
      </div>
    </div>
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
            <input value={descSeed} onChange={(e) => setDescSeed(e.target.value)} placeholder="seedHashHex" className="mono" />
            <input value={descText} onChange={(e) => setDescText(e.target.value)} placeholder={t("介绍（≤2048 字节，存采集自有存储）")} />
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
      load();
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
              <input value={c.name || ''} onChange={(e) => {
                const list = [...(s.server.collector.collectors || [])];
                list[i] = { ...list[i], name: e.target.value };
                setCollector({ collectors: list });
              }} placeholder={t("名称（如 官方）")} style={{ maxWidth: 140 }} />
              <input value={c.endpoint || ''} onChange={(e) => {
                const list = [...(s.server.collector.collectors || [])];
                list[i] = { ...list[i], endpoint: e.target.value };
                setCollector({ collectors: list });
              }} placeholder={t("https://collector.example.com（完整提交 URL，官方地址待定）")} className="mono" />
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

function ConnectPanel() {
  const [err, setErr] = React.useState('');
  const [demoUrl, setDemoUrl] = React.useState('');
  const [walletReady, setWalletReady] = React.useState(false);
  const [launchErr, setLaunchErr] = React.useState('');
  const [launchBusy, setLaunchBusy] = React.useState(false);
  const [iframeSrc, setIframeSrc] = React.useState('');
  const iframeRef = React.useRef(null);
  const iframeHandleRef = React.useRef(null);
  const load = React.useCallback(async () => {
    try {
      const c = await getClient();
      const st = await c.status().catch(() => null);
      setWalletReady(!!(st && st.initialized && !st.locked));
      setErr('');
    } catch (e) { setErr(String(e?.message || e)); }
  }, []);
  // 订阅替代轮询：Connect 状态变化时重载；不长期维护两套状态来源。
  React.useEffect(() => watchTopics(['connect'], () => load()), [load]);
  React.useEffect(() => { load(); }, [load]);
  // 独立窗口的连接管理归钱包整体生命周期（App 根 effect 定期收殓已关闭窗口，
  // 卸载时 teardown 全部启动连接）；面板卸载只处理随展示销毁的 iframe，
  // 页面导航不得误终止仍在运行的独立窗口。
  React.useEffect(() => () => {
    if (iframeHandleRef.current) { try { teardownLaunchedApp(iframeHandleRef.current); } catch { /* ignore */ } iframeHandleRef.current = null; }
  }, []);
  const demoOriginOf = (value) => {
    const trimmed = String(value || '').trim();
    const u = new URL(trimmed);
    if (!['http:', 'https:'].includes(u.protocol) || !u.host) throw new Error('应用地址必须为精确 HTTP(S) origin');
    return u.origin;
  };
  // 窗口方式在点击时预留窗口，避免异步签发后被拦截；签发失败则关闭预留窗口。
  const launchWindow = async () => {
    setLaunchErr('');
    let child = null;
    try {
      const origin = demoOriginOf(demoUrl);
      setLaunchBusy(true);
      child = window.open('about:blank', '_blank');
      if (!child) throw new Error('应用窗口被拦截');
      const issued = await issueLaunchToken(origin);
      if (!issued || !issued.launchToken) throw new Error('签发启动令牌失败');
      const base = origin.replace(/\/+$/, '');
      const url = `${base}/?mode=appView&launchToken=${encodeURIComponent(issued.launchToken)}&sessionWindowOrigin=${encodeURIComponent(window.location.origin)}`;
      const handle = registerLaunchedApp({ origin, getSource: () => child, kind: 'window', child });
      try {
        child.location.href = url;
      } catch (e) {
        try { teardownLaunchedApp(handle); } catch { /* ignore */ }
        throw e;
      }
    } catch (e) {
      try { if (child && !child.closed) child.close(); } catch { /* ignore */ }
      setLaunchErr(String(e?.message || e));
    } finally {
      setLaunchBusy(false);
    }
  };
  // 页内方式由钱包页面挂载 iframe；应用以 parent 为启动方自动 launch。
  const launchIframe = async () => {
    setLaunchErr('');
    try {
      const origin = demoOriginOf(demoUrl);
      setLaunchBusy(true);
      const issued = await issueLaunchToken(origin);
      if (!issued || !issued.launchToken) throw new Error('签发启动令牌失败');
      if (iframeHandleRef.current) { try { teardownLaunchedApp(iframeHandleRef.current); } catch { /* ignore */ } iframeHandleRef.current = null; }
      const base = origin.replace(/\/+$/, '');
      const handle = registerLaunchedApp({ origin, getSource: () => (iframeRef.current ? iframeRef.current.contentWindow : null), kind: 'iframe' });
      iframeHandleRef.current = handle;
      setIframeSrc(`${base}/?mode=iframe&launchToken=${encodeURIComponent(issued.launchToken)}&sessionWindowOrigin=${encodeURIComponent(window.location.origin)}`);
    } catch (e) {
      setLaunchErr(String(e?.message || e));
    } finally {
      setLaunchBusy(false);
    }
  };
  const closeIframe = () => {
    setIframeSrc('');
    if (iframeHandleRef.current) { try { teardownLaunchedApp(iframeHandleRef.current); } catch { /* ignore */ } iframeHandleRef.current = null; }
  };
  return (
    <div>
      <section className="card"><h2>启动应用</h2>
        <p className="hint">钱包已初始化并解锁后才能启动；点击启动即授权，应用自动完成登录，不再弹第二次审批。</p>
        {launchErr && <p className="error" data-testid="launch-error">{launchErr}</p>}
        <div className="row">
          <input data-testid="launch-demo-url" value={demoUrl} onChange={(e) => setDemoUrl(e.target.value)} placeholder="应用地址（精确 origin，如 http://127.0.0.1:4392）" className="mono" style={{ minWidth: 320 }} />
          <button type="button" data-testid="launch-window" disabled={!walletReady || launchBusy} onClick={launchWindow}>窗口打开</button>
          <button type="button" data-testid="launch-iframe" disabled={!walletReady || launchBusy} onClick={launchIframe}>页内打开</button>
        </div>
        {!walletReady && <p className="hint">钱包未就绪或已锁定：先完成初始化并解锁。</p>}
        {iframeSrc && <div style={{ marginTop: 12 }}>
          <div className="row"><span className="hint mono">{iframeSrc}</span><button type="button" className="danger" onClick={closeIframe}>关闭页内应用</button></div>
          <iframe data-testid="launch-iframe-view" ref={iframeRef} src={iframeSrc} title="launched-app" style={{ width: '100%', height: 560, border: '1px solid var(--border, #ccc)' }} />
        </div>}
      </section>
      <div style={{ marginTop: 12 }}>
        {err && <p className="error">{err}</p>}
        <AppCenter />
      </div>
    </div>
  );
}

function WalletInit({ onDone }) {
  const [mode, setMode] = React.useState('new');
  const [password, setPassword] = React.useState('');
  const [network, setNetwork] = React.useState('test');
  const [keyMaterial, setKeyMaterial] = React.useState('');
  const [err, setErr] = React.useState('');
  const submit = async (e) => {
    e.preventDefault(); setErr('');
    try {
      const c = await getClient();
      if (mode === 'new') await c.walletCreate(password, network);
      else await c.walletImport(password, keyMaterial);
      setPassword(''); setKeyMaterial(''); await onDone();
    } catch (e) { setErr(String(e?.message || e)); }
  };
  return (<form data-testid="wallet-init" onSubmit={submit}>
    {err && <p className="error">{err}</p>}
    <div className="row">
      <select data-testid="wallet-init-mode" value={mode} onChange={(e) => setMode(e.target.value)}><option value="new">新建</option><option value="import">导入 KeyHold</option></select>
      <select data-testid="wallet-init-network" value={network} onChange={(e) => setNetwork(e.target.value)}><option value="test">test</option><option value="main">main</option></select>
    </div>
    <div className="row"><input data-testid="wallet-init-password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder="钱包密码" autoComplete="off" /></div>
    {mode === 'import' && <textarea rows={4} value={keyMaterial} onChange={(e) => setKeyMaterial(e.target.value)} placeholder="纯 KeyHold JSON" className="mono" />}
    <div className="row"><button data-testid="wallet-init-submit" type="submit">执行</button></div>
    <p className="hint">失败不覆盖已有钱包，不留下半套状态。</p>
  </form>);
}

function App() {
  const [page, setPage] = useState('overview');
  const [theme, setTheme] = useState('system');
  const [lang, setLangUi] = useState('zh');
  const [status, setStatus] = useState(null);
  const [info, setInfo] = useState(null);
  const [roots, setRoots] = useState(null);
  const [files, setFiles] = useState(null);
  const [tasks, setTasks] = useState([]);
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [seed, setSeed] = useState('');
  const [newRoot, setNewRoot] = useState('');
  const [pickerOpen, setPickerOpen] = useState(false);
  const [conn, setConn] = useState('connecting');
  const instanceRef = useRef('');

  useAppliedTheme(theme);

  useEffect(() => onConnectionStatus(setConn), []);

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
  // 面板切换只影响 iframe 展示不误终止独立窗口。
  useEffect(() => {
    initConnectReceiver().catch(() => {});
    const t = setInterval(() => { try { sweepClosedLaunchedApps(); sweepClosedPopup(); } catch { /* ignore */ } }, 2000);
    const onHide = () => { try { teardownAllLaunchedApps(); } catch { /* ignore */ } };
    window.addEventListener('pagehide', onHide);
    return () => { clearInterval(t); window.removeEventListener('pagehide', onHide); };
  }, []);
  // 原请求窗口订阅后台 connect 变化，收到决定后复核 Receiver 上下文再认领执行。
  // 重连后读完整快照再触发认领，不依赖某一页内存或仅依赖通知消息；无固定轮询兜底。
  useEffect(() => watchTopics(['connect'], () => { pumpPendingApprovals().catch(() => {}); }), []);

  const load = useCallback(async () => {
    try {
      const s = await (await client()).status();
      setStatus(s);
      const i = await (await client()).info();
      setInfo(i);
      const r = await (await client()).listRoots().catch(() => null);
      setRoots(r);
      const f = await (await client()).listFiles().catch(() => null);
      setFiles(f);
      const t = await (await client()).listTasks().catch(() => ({ tasks: [] }));
      setTasks(t.tasks || []);
      const st = await (await client()).getSettings().catch(() => null);
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

  const unlock = async (e) => {
    e.preventDefault();
    setError('');
    try {
      await (await client()).unlock(password);
      await load();
    } catch (e) {
      setError(String(e?.message || e));
    } finally {
      setPassword('');
    }
  };

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

  return (
    <div className="layout">
      <aside className="sidebar">
        <div className="brand">
          <img src="/favicon.svg" alt="km" />
          <div><b>km</b><small>{t('Keymaster 后台')}</small></div>
        </div>
        <nav className="nav">
          {TABS.map((tab) => (
            <button key={tab.id} type="button" data-testid={`nav-${tab.id}`} className={page === tab.id ? 'active' : ''} onClick={() => setPage(tab.id)}>
              <span className="dot" />{t(tab.label)}
            </button>
          ))}
        </nav>
        <div className="sidefoot">{t('单二进制 · 本机回环')}<br />{t('主题跟随系统/设置')}</div>
      </aside>
      <div className="main">
        <header className="topbar">
          <h1>{TABS.find((t) => t.id === page)?.label ? t(TABS.find((x) => x.id === page).label) : t('Keymaster 后台')}</h1>
          <span className="spacer" />
          <span className="badge" data-testid="connection-state" data-state={conn}>{conn === 'connected' ? t('已连接') : conn === 'connecting' ? t('连接中') : conn === 'protocol-incompatible' ? t('协议不兼容') : t('已断开')}</span>
          {conn === 'disconnected' && <button type="button" className="ghost" onClick={() => retryConnection()}>{t('重试')}</button>}
          <GlobalApproval wallet={{ network: info?.network, address: info?.address || status?.address }} onNavigateConnect={() => setPage('connect')} />
          <StateBadge state={conn === 'connected' ? status?.state : undefined} locked={status?.locked ?? info?.locked} />
          {info?.network && <span className="badge">{info.network} · {info?.address ? `${info.address.slice(0, 8)}…` : ''}</span>}
        </header>
        <div className="content">
          {error && <p className="error">{error}</p>}
      {!status?.initialized && status && (
        <section className="card"><h2>{t('新建 / 导入钱包（后台执行）')}</h2>
          <WalletInit onDone={() => load()} />
        </section>)}

      {page === 'overview' && (
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

          <section className="card">
            <h2>{t('解锁')}</h2>
            {(status?.locked ?? info?.locked) ? (
              <form onSubmit={unlock}>
                <div className="row">
                  <input data-testid="unlock-password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder={t("钱包密码（仅解密，不建会话）")} autoComplete="off" />
                  <button data-testid="unlock-submit" type="submit">{t('解锁')}</button>
                </div>
                <p className="hint">{t('密码只进本次请求体，不存浏览器存储，提交后清空。')}</p>
              </form>
            ) : (
              <p className="hint">{t('后台已解锁。重启后需重新解锁；本轮无主动锁定命令。')}</p>
            )}
          </section>
        </div>
      )}

      {page === 'sell' && (
        <section className="card">
          <div className="section-head"><h2>{t('售卖根')}{t('（{n}）', { n: roots?.roots?.length ?? 0 })}{t('· 默认下载根：')}{defaultRootPath}</h2><button type="button" className="ghost" onClick={() => refreshRoots()}>{t('刷新目录')}</button></div>
          <p className="hint">{t('后台每 5 秒自动核对（新增/长度变化才重算）并支持 SIGHUP；这里可手动立即刷新。')}</p>
          <form onSubmit={addRoot}>
            <div className="row">
              <input value={newRoot} onChange={(e) => setNewRoot(e.target.value)} placeholder={t("/srv/files（后台可读目录，可直接输入）")} />
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
              <input value={seed} onChange={(e) => setSeed(e.target.value)} placeholder={t("seedhashhex（64 hex），重复提交复用任务")} className="mono" />
              <button type="submit">{t('提交 get')}</button>
            </div>
          </form>
          <table>
            <thead><tr><th>{t('seed')}</th><th>{t('状态')}</th><th>{t('进度')}</th><th>{t('完成路径')}</th><th>{t('操作')}</th></tr></thead>
            <tbody>
              {tasks.map((t) => (
                <tr key={t.seedHashHex}>
                  <td className="mono">{t.seedHashHex.slice(0, 12)}…</td>
                  <td>{{pending: t('待提交'), running: t('运行中'), done: t('已完成'), failed: t('失败'), cancelled: t('已取消')}[t.state] || t.state}{t.detail ? ` · ${t.detail}` : ''}{t.error ? ` · ${t.error}` : ''}</td>
                  <td>
                    <div className="progress"><i style={{ width: `${t.blocksTotal ? Math.round((t.blocksDone / t.blocksTotal) * 100) : 0}%` }} /></div>
                    <span className="hint">{t.blocksDone}/{t.blocksTotal}</span>
                  </td>
                  <td className="mono">{t.finalPath || '—'}</td>
                  <td>{['pending', 'running'].includes(t.state) && <button className="danger" onClick={() => cancelTask(t.seedHashHex)}>{t('取消')}</button>}</td>
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

      {page === 'settings' && <SettingsPage onTheme={setTheme} />}

      {page === 'connect' && <ConnectPanel />}

          <p className="hint">{t('主题跟随系统或设置页选择（settings 唯一来源，本页不写 localStorage）。远程访问请自备反向代理与认证。')}</p>
        </div>
      </div>
    </div>
  );
}

createRoot(document.getElementById('root')).render(<App />);
