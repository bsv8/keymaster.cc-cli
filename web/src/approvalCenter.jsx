import React, { useCallback, useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { getClient } from './client.js';
import { watchTopics } from './subscribe.js';
import { decideApproval, pumpPendingApprovals } from './connect.js';
import { interpretLockingScript } from 'keymaster-connect/script';
import { t } from './i18n.js';

const METHOD_LABELS = {
  'identity.get': '身份凭证',
  'intent.sign': '意图签名',
  'cipher.encrypt': '加密',
  'cipher.decrypt': '解密',
  'payment.pay': '付款',
};

async function call(method, params) {
  const c = await getClient();
  return c.call(undefined, method, params || {});
}

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

function fmtTime(ms) {
  try { return new Date(ms).toLocaleString(); } catch { return String(ms); }
}

function ApprovalCard({ approval, wallet, onDecide, disabled }) {
  const [grantAuto, setGrantAuto] = useState(false);
  const [busy, setBusy] = useState(false);
  const [decideErr, setDecideErr] = useState('');
  useEffect(() => { setGrantAuto(false); setDecideErr(''); }, [approval.id]);
  const decide = async (decision) => {
    setBusy(true);
    setDecideErr('');
    try {
      await onDecide(approval, decision, grantAuto);
    } catch (e) {
      setDecideErr(String(e?.message || e));
    } finally {
      setBusy(false);
    }
  };
  const params = approval.params || {};
  return (
    <div className="card" data-testid={`approval-${approval.id}`} style={{ marginBottom: 8 }}>
      <p className="mono">{approval.method} · {approval.appId} · {approval.origin}</p>
      <p className="mono">{t('应用')} {approval.appName || '—'} · {t('来源')} {approval.origin} · {t('发行人')} {(approval.appPublicKey || '').slice(0, 12)}…</p>
      <p className="mono">{t('能力声明')} {(approval.requirements || []).join(', ') || '—'} · {t('钱包')} {wallet?.network || '—'} {wallet?.address ? wallet.address.slice(0, 8) + '…' : ''}</p>
      {approval.method === 'identity.get' && (
        <div className="mono">
          <p>{t('请求内容')} {params.text || '—'}</p>
          <p>{t('用途')} {(params.claims || []).join(', ') || '—'} · {t('有效期')} {params.iat ? fmtTime(params.iat * 1000) : '—'} → {params.exp ? fmtTime(params.exp * 1000) : '—'}</p>
          <p>{t('绑定来源')} {params.aud || approval.origin}</p>
        </div>
      )}
      {approval.method === 'intent.sign' && (
        <div className="mono">
          <p>{t('请求内容')} {params.text || '—'} · {t('类型')} {params.contentType || '—'}</p>
          <p>{t('有效期')} {params.iat ? fmtTime(params.iat * 1000) : '—'} → {params.exp ? fmtTime(params.exp * 1000) : '—'} · {t('绑定来源')} {params.aud || approval.origin}</p>
          <p>{t('附件大小')} {approval.contentSize ?? approval.attachmentSizes?.content ?? '—'} · SHA-256 <code>{approval.contentSha256Hex || '—'}</code></p>
        </div>
      )}
      {(approval.method === 'cipher.encrypt' || approval.method === 'cipher.decrypt') && (
        <div className="mono">
          <p>{t('操作类型')} {approval.method} · scope {t('应用隔离')} · {t('内容类型')} {params.contentType || '—'}</p>
          <p>{t('附件大小')} {JSON.stringify(approval.attachmentSizes || {})} · {t('解密默认不展示明文，仅展示类型与大小；批准后由原窗口执行。')}</p>
          {approval.expiresAt && <p>{t('期限')} {fmtTime(approval.expiresAt)}</p>}
        </div>
      )}
      {approval.method === 'payment.pay' && <PaymentPreview params={params} />}
      {(approval.status === 'cancelled' || approval.status === 'rejected') && <p className="error">{t('当前申请已结束或连接已失效。没有可批准的请求。')}</p>}
      {['identity.get', 'intent.sign', 'cipher.encrypt', 'cipher.decrypt', 'payment.pay'].includes(approval.method) && (
        <label className="check" style={{ margin: '8px 0' }}>
          <input type="checkbox" data-testid={`grant-auto-${approval.id}`} checked={grantAuto} disabled={disabled || busy || approval.status !== 'pending'} onChange={(e) => setGrantAuto(e.target.checked)} />
          {t('以后自动同意此应用的同类请求')}
        </label>
      )}
      {approval.method === 'payment.pay' && grantAuto && <p className="error">{t('已允许自动付款，不设金额上限。')}</p>}
      {decideErr && <p className="error">{decideErr}</p>}
      <div className="row">
        <button type="button" data-testid={`approve-${approval.id}`} disabled={disabled || busy || approval.status !== 'pending'} onClick={() => decide('approve')}>{t('批准')}</button>
        <button type="button" data-testid={`reject-${approval.id}`} className="danger" disabled={disabled || busy || approval.status !== 'pending'} onClick={() => decide('reject')}>{t('拒绝')}</button>
      </div>
      <p className="hint">{t('建立连接不会自动授予签名、加解密或付款许可。')}</p>
    </div>
  );
}

function PaymentPreview({ params }) {
  const outputs = Array.isArray(params?.outputs) ? params.outputs : [];
  let total = 0n;
  try { for (const o of outputs) total += BigInt(o.amount || 0); } catch { /* ignore */ }
  return (
    <div className="mono" style={{ marginTop: 8 }}>
      <p>{t('业务输出总额')} {total.toString()} sat · {t('矿工费')} {String(params?.feeSatoshis || 0)} sat · {t('找零')} {String(params?.changeSatoshis || 0)} sat · {t('实际支出')} {(() => { try { return (total + BigInt(params?.feeSatoshis || 0)).toString(); } catch { return '—'; } })()} sat（含费）</p>
      <p>{t('批准绑定已保存计划，不重新选币；计划改变需重新确认。')}</p>
      <ol>
        {outputs.map((o, i) => {
          const scriptHex = String(o.script || '').toLowerCase();
          let label = '未知脚本';
          let detail = '不认识此脚本，无法解释其用途';
          try {
            const it = interpretLockingScript(scriptHex, Number(o.amount || 0));
            if (it.kind === 'p2pkh') { label = 'P2PKH'; detail = `收款地址 ${it.addressMainnet} / ${it.addressTestnet}，金额 ${o.amount} sat`; }
            else if (it.kind === 'p2pk') { label = 'P2PK'; detail = `收款公钥 ${it.publicKeyHex}，金额 ${o.amount} sat`; }
            else if (it.kind === 'multisig') { label = `标准多签 ${it.m}/${it.n}`; detail = `所需签名数 ${it.m}，公钥 ${it.publicKeysHex.join(', ')}，金额 ${o.amount} sat`; }
            else if (it.kind === 'op_return') { label = '数据输出'; detail = `数据 ${it.dataHex || '(空)'}${it.dataText ? `（文本：${it.dataText}）` : ''}，金额 ${o.amount} sat`; }
          } catch { /* ignore */ }
          return <li key={i}><div>#{i} [{label}] {String(o.amount)} sat</div><div>{detail}</div><div>完整脚本 <code>{scriptHex}</code></div></li>;
        })}
      </ol>
    </div>
  );
}

export function GlobalApproval({ wallet, onNavigateConnect }) {
  const { approvals, apps, err, history } = useConnectData();
  const online = (apps.connections || []).length;
  const autoSeen = useRef(new Set());
  const [reviewing, setReviewing] = useState(undefined);
  const [open, setOpen] = useState(false);
  const [sound, setSound] = useState('off');
  const [notification, setNotification] = useState('未开启');
  const [reminder, setReminder] = useState('');
  const [seen, setSeen] = useState(false);
  const dialog = useRef(null);
  const audio = useRef(null);
  const notified = useRef(new Set());
  const soundAt = useRef(0);
  const originalTitle = useRef(document.title);
  const originalIcon = useRef(null);
  const count = approvals.length;
  const pendingKey = approvals.map((a) => a.id).join('|');

  const review = (id) => {
    setReviewing(id ?? approvals[0]?.id);
    setSeen(true);
    setOpen(true);
  };

  const play = async (manual) => {
    if (!audio.current || audio.current.state !== 'running') { if (sound !== 'off') setSound('failed'); return; }
    if (Date.now() - soundAt.current < 1200) return;
    soundAt.current = Date.now();
    try {
      const osc = audio.current.createOscillator();
      const gain = audio.current.createGain();
      osc.connect(gain); gain.connect(audio.current.destination);
      osc.frequency.setValueAtTime(manual ? 740 : 440, audio.current.currentTime);
      if (manual) osc.frequency.setValueAtTime(550, audio.current.currentTime + 0.13);
      gain.gain.setValueAtTime(0.075, audio.current.currentTime);
      gain.gain.exponentialRampToValueAtTime(0.001, audio.current.currentTime + 0.35);
      osc.start(); osc.stop(audio.current.currentTime + 0.35);
      osc.onended = () => { osc.disconnect(); gain.disconnect(); };
    } catch { setSound('failed'); }
  };

  useEffect(() => {
    const fresh = approvals.filter((a) => !notified.current.has(a.id));
    for (const a of fresh) notified.current.add(a.id);
    if (!fresh.length) return;
    setSeen(false);
    void play(true);
    if (document.hidden && typeof Notification !== 'undefined' && Notification.permission === 'granted') {
      try {
        const notice = new Notification('Keymaster · 新的应用授权请求', { body: `有 ${count} 项待处理，请回到钱包核对来源和请求。`, tag: 'keymaster-application-approval' });
        notice.onclick = () => { window.focus(); review(fresh[0].id); notice.close(); };
      } catch { setNotification('系统通知发送失败'); }
    }
  }, [pendingKey]);

  useEffect(() => {
    const freshAuto = (history || []).filter((e) => e.auto && (e.kind === 'executed' || e.kind === 'auto-approved') && !autoSeen.current.has(e.id));
    for (const e of freshAuto) autoSeen.current.add(e.id);
    if (freshAuto.length) {
      setReminder(`\u5df2\u6709\u8bb8\u53ef\u81ea\u52a8\u6267\u884c ${freshAuto.length} \u9879\uff1b\u8bf7\u5728\u5e94\u7528\u65e5\u5fd7\u67e5\u770b\u5b9e\u9645\u7ed3\u679c\u3002`);
      void play(false);
    }
  }, [history]);

  useEffect(() => {
    const icon = document.querySelector('link[rel="icon"]');
    if (originalIcon.current === null && icon) originalIcon.current = icon.href;
    document.title = count ? `(${count} 待授权) Keymaster` : originalTitle.current;
    document.body.toggleAttribute('data-application-pending', count > 0);
    if (icon) icon.href = count ? 'data:image/svg+xml,' + encodeURIComponent('<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="7" fill="#126451"/><text x="7" y="24" fill="white" font-size="23">K</text><circle cx="26" cy="6" r="6" fill="#eba53b"/></svg>') : originalIcon.current ?? icon.href;
    return () => { document.body.removeAttribute('data-application-pending'); document.title = originalTitle.current; if (icon && originalIcon.current) icon.href = originalIcon.current; };
  }, [count]);

  useEffect(() => {
    if (open && dialog.current && !dialog.current.open) dialog.current.showModal();
    if (!open && dialog.current?.open) dialog.current.close();
  }, [open]);

  useEffect(() => () => { try { audio.current?.close(); } catch { /* ignore */ } }, []);

  const current = reviewing ? approvals.find((a) => a.id === reviewing) : approvals[0];
  const currentEnded = reviewing && !current;
  const decide = async (approval, decision, grantAuto) => {
    await decideApproval(approval.id, decision, grantAuto);
  };

  return (
    <>
      <button type="button" className="ghost" onClick={onNavigateConnect} title={t('在线连接')}>{t('应用连接')} {online}</button>
      <button type="button" className="ghost" data-testid="global-pending" disabled={!count} onClick={() => review()}>{t('待处理')} {count}</button>
      <button type="button" className="ghost" onClick={async () => { try { audio.current ??= new AudioContext(); await audio.current.resume(); if (audio.current.state !== 'running') throw new Error(); setSound('ready'); await play(true); } catch { setSound('failed'); } }}>{sound === 'off' ? t('启用 / 试听提示音') : sound === 'failed' ? t('声音未启用 · 重试') : t('试听提示音')}</button>
      <button type="button" className="ghost" title={t('浏览器可能限制后台发声和系统通知；静音、勿扰与挂起无法绕过')} onClick={async () => { try { if (typeof Notification === 'undefined') { setNotification(t('浏览器不支持')); return; } const p = await Notification.requestPermission(); setNotification(p === 'granted' ? t('已允许') : t('未获允许')); } catch { setNotification(t('开启失败')); } }}>{t('系统通知')}：{notification}</button>
      {createPortal(<>
        {count ? <aside className={`application-alert ${seen ? '' : 'is-unseen'}`} aria-label={t('待处理应用授权')}><strong><i />{t('待授权')} {count} {t('项')}</strong><span>{approvals.map((a) => METHOD_LABELS[a.method] ?? a.method).join(' · ')}</span><button type="button" onClick={() => review()}>{t('查看并处理 →')}</button>{sound === 'failed' ? <small role="status">{t('提示音未播放')}</small> : null}</aside> : null}
        {reminder ? <aside className="application-automatic" role="status">{reminder}<button type="button" onClick={() => setReminder('')}>{t('关闭')}</button></aside> : null}
        <dialog ref={dialog} data-testid="connect-approval-dialog" className="application-drawer" onCancel={() => setOpen(false)} onClose={() => setOpen(false)} aria-labelledby="application-approval-title">
          <header><h2 id="application-approval-title">{t('应用授权 · 待处理')} {count} {t('项')}</h2><button type="button" aria-label={t('关闭审批，返回原工作')} onClick={() => setOpen(false)}>✕</button></header>
          <div className="application-drawer-body">
            {err && <p className="error">{err}</p>}
            {current ? <ApprovalCard approval={current} wallet={wallet} onDecide={decide} disabled={false} /> : <p>{currentEnded ? t('当前申请已结束或连接已失效。没有可批准的请求。') : t('当前申请已结束或连接已失效。没有可批准的请求。')}</p>}
            <p className="application-note">{t('逐项核对；新请求不会替换当前申请。批准不等于执行成功。')}</p>
            <h3>{t('其他待处理申请')}</h3>
            {approvals.filter((a) => a.id !== current?.id).map((a) => <button type="button" className="application-queue-item" key={a.id} onClick={() => { setReviewing(a.id); setSeen(true); }}>{a.appName || a.appId} · {METHOD_LABELS[a.method] ?? a.method}</button>)}
          </div>
        </dialog>
      </>, document.body)}
    </>
  );
}

export function AppCenter() {
  const { approvals, apps, permissions, history, err, reload } = useConnectData();
  const [selected, setSelected] = useState(undefined);
  const [tab, setTab] = useState('activity');
  const [filter, setFilter] = useState('');
  const [localErr, setLocalErr] = useState('');
  const appList = apps.applications || [];
  const app = appList.find((a) => a.applicationKey === selected) ?? appList[0];
  const connections = (apps.connections || []).filter((c) => c.applicationKey === app?.applicationKey);
  const appApprovals = approvals.filter((a) => a.origin === app?.origin && a.appId === app?.appId);

  const disconnect = async () => {
    if (!app) return;
    setLocalErr('');
    try {
      await call('connect.auth.revoke', { origin: app.origin, appPublicKey: app.appPublicKey, appId: app.appId });
      await reload();
    } catch (e) {
      setLocalErr(t('撤销连接授权失败，请重试。'));
    }
  };

  const revokePerm = async (applicationKey, method) => {
    setLocalErr('');
    try {
      await call('connect.permissions.revoke', { applicationKey, method });
      await reload();
    } catch (e) {
      setLocalErr(t('取消许可失败，请重试；未将失败伪装为成功。'));
    }
  };

  return (
    <section className="card">
      <h2>{t('应用连接')}</h2>
      <p className="hint">{t('按应用管理许可和历史，会话是应用下面的连接实例。')}</p>
      {err && <p className="error">{err}</p>}
      {localErr && <p className="error">{localErr}</p>}
      <div className="row">
        <aside style={{ minWidth: 220 }}>
          {appList.length === 0 ? <p className="hint">{t('还没有应用连接。')}</p> : appList.map((a) => (
            <button key={a.applicationKey} type="button" className={a.applicationKey === app?.applicationKey ? 'ghost' : ''} style={{ display: 'block', marginBottom: 6 }} onClick={() => setSelected(a.applicationKey)}>
              <strong>{a.appName || a.appId}</strong><br /><code className="mono">{a.origin}</code>
            </button>
          ))}
        </aside>
        <article style={{ flex: 1 }}>
          {app && (
            <>
              <div className="application-identity"><h3>{app.appName}</h3><code className="mono">{app.origin}</code><small>{app.identityDigestHex ? `${t('已验证发行人')} · ${app.appPublicKey}` : t('未提供发行人身份')}</small></div>
              <div className="application-tabs">
                {[['activity', t('活动')], ['sessions', t('会话')], ['settings', t('设置')], ['history', t('日志历史')]].map(([v, label]) => (
                  <button key={v} type="button" className={tab === v ? 'selected' : ''} onClick={() => setTab(v)}>{label}</button>
                ))}
              </div>
              {tab === 'settings' && (
                <>
                  <button type="button" data-testid="disconnect-app" onClick={disconnect}>{t('断开应用连接')}</button>
                  <p className="application-note">{t('许可属于应用，不属于某次会话。按发行人 + App 编号 + 精确来源隔离，刷新和重连后仍有效；不同操作互不通用。')}</p>
                  {err.includes('permissions') && <p className="error" role="alert">{t('许可读取失败，不能把未知状态当成未开启。')}</p>}
                  {['identity.get', 'intent.sign', 'cipher.encrypt', 'cipher.decrypt', 'payment.pay'].map((m) => {
                    const entry = permissions.find((p) => p.applicationKey === app.applicationKey);
                    const unknown = err.includes('permissions') && !entry;
                    const granted = (entry?.methods || []).includes(m);
                    return (
                      <div className="application-setting" key={m}>
                        <div><strong>{METHOD_LABELS[m]}</strong><code className="mono">{m}</code>{m === 'payment.pay' && granted ? <small className="application-warning">{t('已允许自动付款，不设金额上限。')}</small> : null}</div>
                        <span>{unknown ? t('读取中 / 未知') : granted ? t('以后自动同意') : t('每次询问')}</span>
                        {granted && !unknown ? <button type="button" data-testid={`revoke-perm-${m}`} onClick={() => revokePerm(app.applicationKey, m)}>{t('取消自动同意')}</button> : null}
                      </div>
                    );
                  })}
                  <p className="application-note">{t('只能在实际审批中勾选「以后自动同意」授予许可。取消后，新请求恢复逐次询问；已经签名或执行中的操作不会因此被撤销。')}</p>
                </>
              )}
              {tab === 'sessions' && (
                <>
                  <p className="application-note">{t('锁定、刷新或关闭只暂停运行连接，保留原应用授权；同钱包恢复后重新握手，不重放旧业务。明确断开才撤销连接授权。')}</p>
                  {connections.length ? connections.map((c) => (
                    <div className="application-session" key={c.id}><div><strong>{t('已连接')}</strong><small>{fmtTime(c.createdAt)}</small><code className="mono">{c.id}</code></div><button type="button" data-testid="disconnect-app" onClick={disconnect}>{t('断开应用连接')}</button></div>
                  )) : <p className="hint">{t('当前没有在线会话。')}</p>}
                </>
              )}
              {tab === 'activity' && (
                <>
                  <p className="application-note">{t('为避免重复审批，逐次请求统一从顶部「待处理」面板处理。')}</p>
                  {appApprovals.length ? appApprovals.map((a) => <div className="application-session" key={a.id}><strong>{METHOD_LABELS[a.method] ?? a.method}</strong><span>{a.status}</span></div>) : <p className="hint">{t('当前没有活跃请求。')}</p>}
                </>
              )}
              {tab === 'history' && (
                <>
                  <input className="application-filter" placeholder={t('筛选操作、状态或时间')} value={filter} onChange={(e) => setFilter(e.target.value)} aria-label={t('筛选应用历史')} />
                  {(history.filter((e) => e.applicationKey === app.applicationKey).filter((e) => `${e.kind} ${e.method || ''}`.includes(filter))).map((e) => (
                    <div className="application-history" key={e.id}><strong>{e.kind}</strong><p>{e.method || ''} · {fmtTime(e.at)} {e.auto ? t('(自动)') : t('(人工)')}</p></div>
                  ))}
                  <p className="application-note">{t('日志不展示密钥、token、解密明文或完整原始报文。断开连接不删除历史与未核实资金责任。')}</p>
                </>
              )}
            </>
          )}
        </article>
      </div>
    </section>
  );
}
