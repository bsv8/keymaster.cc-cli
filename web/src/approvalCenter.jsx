import React, { useEffect, useId, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { decideApproval } from './connect.js';
import { interpretLockingScript } from 'keymaster-connect/script';
import { t } from './i18n.js';
import { useConnectData } from './useConnectData.js';
import { AppIcon, operationName, eventName } from './appPresentation.jsx';
import { useMarket, entryPresentation } from './appMarket.jsx';

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
    <div data-testid={`approval-${approval.id}`} data-method={approval.method}>
      <p className="mono">{t('钱包')} {wallet?.network || '—'} {wallet?.address ? wallet.address.slice(0, 8) + '…' : ''}</p>
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
          <p>{t('操作类型')} {operationName(approval.method)} · {t('应用隔离')} · {t('内容类型')} {params.contentType || '—'}</p>
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

function ActivityRow({ title, status, origin, at, pending = false, onInteraction, children }) {
  const detailId = useId();
  const [expandedOverride, setExpandedOverride] = useState();
  const expanded = expandedOverride ?? pending;
  return <li className="activity-row">
    <button type="button" className="application-queue-item" aria-expanded={expanded} aria-controls={detailId} onClick={() => {
      setExpandedOverride(!expanded);
      onInteraction();
    }}>
      <span className="activity-title">{title}</span><span className="activity-status">{status}</span>
      {origin && <span className="activity-origin">{origin}</span>}
      {at != null && <time>{fmtTime(at)}</time>}
    </button>
    <div id={detailId} className="activity-detail" hidden={!expanded}>{expanded && children}</div>
  </li>;
}

function ActivityGroups({ approvals, history, connections, market, wallet, onInteraction }) {
  const connectedApps = new Set(connections.map(connection => connection.applicationKey));
  const groups = new Map();
  for (const approval of approvals) {
    const key = approval.appPublicKeyHex || approval.origin;
    const group = groups.get(key) ?? { pending: [], events: [] };
    group.pending.push(approval);
    groups.set(key, group);
  }
  for (const event of history) {
    const key = event.applicationKey || event.origin;
    const group = groups.get(key) ?? { pending: [], events: [] };
    group.events.push(event);
    groups.set(key, group);
  }
  if (!groups.size) return <p className="application-note">{t('还没有应用处理记录。')}</p>;
  return <div className="activity-groups">{[...groups].map(([key, group]) => {
    const entry = market.entries.find(entry => entry.appPublicKeyHex === key);
    const display = entry ? entryPresentation(entry, market.deployments) : market.knownApplications?.[key] || {};
    const name = display.name || t('应用连接申请');
    const connected = connectedApps.has(key);
    const status = t(connected ? '已连接' : '未连接');
    return <section className="activity-group" key={key}>
      <header>
        <AppIcon icon={display.icon} url={entry?.urls?.[0]?.url} name={name} size={36} />
        <h3>{name}</h3>
        <span className="application-connection-light" data-connected={connected} role="img" aria-label={status} title={status} />
      </header>
      <ul>
        {group.pending.map(item => <ActivityRow key={`pending:${item.id}`} title={operationName(item.method)} status={t('等待授权')} origin={item.origin} at={item.createdAt} pending onInteraction={onInteraction}>
          <ApprovalCard approval={item} wallet={wallet} onDecide={decide} disabled={false} />
        </ActivityRow>)}
        {group.events.sort((a, b) => b.at - a.at).map(event => <ActivityRow key={`history:${event.id}`} title={operationName(event.method)} status={eventName(event)} origin={event.origin} at={event.at} onInteraction={onInteraction}>
          <p>{t(event.auto ? '使用已有永久授权' : '人工处理或连接事件')}</p>
        </ActivityRow>)}
      </ul>
    </section>;
  })}</div>;
}

async function decide(approval, decision, grantAuto) {
  await decideApproval(approval.id, decision, grantAuto);
}

export function GlobalApproval({ wallet, showSettings }) {
  const { approvals, apps, err, history } = useConnectData();
  const market = useMarket();
  const [settingsOutlet, setSettingsOutlet] = useState(null);
  useEffect(() => { setSettingsOutlet(showSettings ? document.getElementById('application-reminders-settings') : null); }, [showSettings]);
  const autoSeen = useRef(new Set());
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

  const review = () => {
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
        notice.onclick = () => { window.focus(); review(); notice.close(); };
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

  return (
    <>
      <button type="button" className="ghost" data-testid="global-pending" onClick={() => review()}>{t('待处理')} {count}</button>
      {settingsOutlet && createPortal(<div className="row">
      <button type="button" className="ghost" onClick={async () => { try { audio.current ??= new AudioContext(); await audio.current.resume(); if (audio.current.state !== 'running') throw new Error(); setSound('ready'); await play(true); } catch { setSound('failed'); } }}>{sound === 'off' ? t('启用 / 试听提示音') : sound === 'failed' ? t('声音未启用 · 重试') : t('试听提示音')}</button>
      <button type="button" className="ghost" title={t('浏览器可能限制后台发声和系统通知；静音、勿扰与挂起无法绕过')} onClick={async () => { try { if (typeof Notification === 'undefined') { setNotification(t('浏览器不支持')); return; } const p = await Notification.requestPermission(); setNotification(p === 'granted' ? t('已允许') : t('未获允许')); } catch { setNotification(t('开启失败')); } }}>{t('系统通知')}：{notification}</button>
      </div>, settingsOutlet)}
      {createPortal(<>
        {count ? <aside className={`application-alert ${seen ? '' : 'is-unseen'}`} aria-label={t('待处理应用授权')}><strong><i />{t('待授权')} {count} {t('项')}</strong><span>{approvals.map((a) => operationName(a.method)).join(' · ')}</span><button type="button" onClick={() => review()}>{t('查看并处理 →')}</button>{sound === 'failed' ? <small role="status">{t('提示音未播放')}</small> : null}</aside> : null}
        {reminder ? <aside className="application-automatic" role="status">{reminder}<button type="button" onClick={() => setReminder('')}>{t('关闭')}</button></aside> : null}
        <dialog ref={dialog} data-testid="connect-approval-dialog" className="application-drawer" onCancel={() => setOpen(false)} onClose={() => setOpen(false)} aria-labelledby="application-approval-title">
          <header><h2 id="application-approval-title">{t('应用授权 · 待处理')} {count} {t('项')}</h2><button type="button" aria-label={t('关闭审批，返回原工作')} onClick={() => setOpen(false)}>✕</button></header>
          <div className="application-drawer-body">
            {err && <p className="error">{err}</p>}
            {open && <ActivityGroups approvals={approvals} history={history} connections={apps.connections ?? []} market={market} wallet={wallet} onInteraction={() => setSeen(true)} />}
          </div>
        </dialog>
      </>, document.body)}
    </>
  );
}
