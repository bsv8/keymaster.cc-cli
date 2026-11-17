import React, { useEffect, useRef, useState } from 'react';
import { getClient } from './client.js';
import { generateKey, parseMaterial, describeKey, encryptMaterial } from './walletMaterial.js';

export function WalletFrame({ children, wide }) {
  return <main className="wallet-entry"><header className="wallet-entry-brand"><img src="/favicon.svg" alt="" width="32" height="32" /><strong>Keymaster</strong></header><section className={`wallet-entry-panel${wide ? ' wallet-entry-panel--wide' : ''}`}>{children}</section></main>;
}
function Network({ value, onChange, fixed }) {
  return <fieldset className="wallet-network"><legend>BSV 网络</legend><div className="wallet-network-options">{[['main', '主网', 'Mainnet'], ['test', '测试网', 'Testnet']].map(([network, title, subtitle]) => <label key={network}><input type="radio" name="wallet-network" value={network} checked={value === network} disabled={fixed} onChange={() => onChange(network)} /><span><strong>{title}</strong><small>{subtitle}</small></span></label>)}</div><p className="hint">{fixed ? '网络由 WIF 确定。' : ''}网络在创建钱包后不可修改。</p></fieldset>;
}

/** Secret drafts live only in this component and DOM, never in routes or browser storage. */
export function WalletOnboarding({ onImported, onImporting }) {
  const [mode, setMode] = useState('welcome');
  const [step, setStep] = useState(0);
  const [format, setFormat] = useState('wif');
  const [inputMode, setInputMode] = useState('text');
  const [network, setNetwork] = useState('main');
  const [label, setLabel] = useState('我的钱包');
  const [result, setResult] = useState(null);
  const [reuse, setReuse] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const material = useRef(null), sourcePassword = useRef(null), password = useRef(null), confirmation = useRef(null), file = useRef(null);
  const secret = useRef({ bytes: null, sourcePassword: '' });
  const generation = useRef(0), active = useRef(true), pending = useRef(false), heading = useRef(null);
  const clearSecret = () => {
    secret.current.bytes?.fill(0); secret.current = { bytes: null, sourcePassword: '' };
    for (const input of [material, sourcePassword, password, confirmation, file]) if (input.current) input.current.value = '';
  };
  useEffect(() => {
    active.current = true;
    const discard = () => { generation.current++; clearSecret(); };
    window.addEventListener('pagehide', discard);
    return () => { active.current = false; discard(); window.removeEventListener('pagehide', discard); };
  }, []);
  useEffect(() => { heading.current?.focus(); }, [mode, step]);
  const back = () => {
    if (pending.current) return;
    setError('');
    if (mode === 'new' || step === 0) { clearSecret(); setMode('welcome'); setResult(null); }
    else { if (step <= 2) { clearSecret(); setResult(null); } setStep(step - 1); }
  };
  const run = async operation => {
    if (pending.current) return;
    pending.current = true; setBusy(true); setError('');
    const token = generation.current;
    const current = () => active.current && generation.current === token;
    try { await operation(current); }
    catch (cause) { if (current()) setError(cause instanceof Error ? cause.message : String(cause)); }
    finally { pending.current = false; if (current()) setBusy(false); }
  };
  const parse = event => {
    event.preventDefault();
    void run(async current => {
      let text = material.current?.value || '';
      if (inputMode === 'file' && format === 'keyhold') {
        const selected = file.current?.files?.[0];
        if (!selected) throw new Error('请选择 KeyHold 文件');
        text = await selected.text();
      }
      if (!current()) return;
      const source = sourcePassword.current?.value || '';
      const parsed = await parseMaterial(format, text, source);
      text = '';
      if (!current()) { parsed.bytes.fill(0); return; }
      clearSecret();
      secret.current = { bytes: parsed.bytes, sourcePassword: format === 'keyhold' ? source : '' };
      if (parsed.network) setNetwork(parsed.network);
      if (parsed.label) setLabel(parsed.label);
      setResult({ format: parsed.format, network: parsed.network }); setStep(2);
    });
  };
  const finish = event => {
    event.preventDefault();
    void run(async current => {
      let lockPassword = mode === 'import' && format === 'keyhold' && reuse ? secret.current.sourcePassword : password.current?.value || '';
      const confirm = confirmation.current?.value || '';
      if (!lockPassword) throw new Error('密码不能为空');
      if (!(mode === 'import' && format === 'keyhold' && reuse) && lockPassword.length < 8) throw new Error('密码至少 8 位');
      if (!(mode === 'import' && format === 'keyhold' && reuse) && lockPassword !== confirm) throw new Error('两次密码不一致');
      let bytes = secret.current.bytes;
      if (mode === 'new') { bytes = generateKey(); secret.current.bytes = bytes; }
      if (!bytes) throw new Error('请返回并重新解析导入材料');
      let keyMaterial = '';
      try {
        keyMaterial = await encryptMaterial(bytes, lockPassword, label.trim() || '我的钱包');
        if (!current()) return;
        const client = await getClient();
        if (!current()) return;
        onImporting(true);
        await client.call(undefined, 'wallet.import', { keyMaterial, network });
        if (!current()) return;
        // Transfer ownership of the one-shot credential to the normal unlock entry.
        onImported(lockPassword);
      } finally { lockPassword = ''; keyMaterial = ''; clearSecret(); onImporting(false); }
    });
  };
  let preview = null;
  if (secret.current.bytes && step >= 2) preview = describeKey(secret.current.bytes, network);
  const title = mode === 'welcome' ? '欢迎使用 Keymaster' : mode === 'new' ? '新建钱包' : ['导入私钥：1. 选择格式', '导入私钥：2. 输入材料', '导入私钥：3. 确认解析结果', '导入私钥：4. 设置 Key 密码'][step];
  return <WalletFrame><div data-testid="wallet-init" aria-busy={busy}><h1 ref={heading} tabIndex={-1}>{title}</h1>
    {mode === 'welcome' ? <><p>欢迎。选择你要开始的流程：</p><div className="wallet-entry-cards"><button type="button" onClick={() => { setMode('new'); setError(''); }}><strong>新建钱包</strong><span>设置 Key 密码，在浏览器生成钱包 Key。</span><small>设置密码 →</small></button><button type="button" onClick={() => { setMode('import'); setStep(0); setError(''); }}><strong>导入钱包 Key</strong><span>已有 WIF / Hex / 加密 KeyHold？解析并设置密码。</span><small>开始导入 →</small></button></div><p className="hint">钱包以加密 KeyHold 保存到运行 km 后台的机器。浏览器不持久化私钥或密码；解锁时密码只发送给当前后台。</p></> : <>
    {mode === 'import' && <ol className="wallet-steps" aria-label="导入进度">{['选择格式', '输入材料', '确认结果', '设置密码'].map((name, index) => <li key={name} aria-current={index === step ? 'step' : undefined}>{index + 1}. {name}</li>)}</ol>}
    <p role="alert" className={error ? 'error' : undefined}>{error}</p>
    {mode === 'import' && step === 0 ? <><div className="wallet-entry-cards">{[['wif', 'WIF', 'BSV Base58Check 编码私钥'], ['hex', 'Hex', '32 字节十六进制私钥'], ['keyhold', '加密 KeyHold', 'JSON 文本或文件']].map(([id, name, hint]) => <button key={id} type="button" aria-pressed={format === id} onClick={() => setFormat(id)}><strong>{name}</strong><span>{hint}</span></button>)}</div><div className="row"><button type="button" onClick={() => setStep(1)}>下一步</button><button type="button" className="ghost" onClick={back}>返回</button></div></> :
    mode === 'import' && step === 1 ? <form onSubmit={parse}><fieldset disabled={busy}>
      {format === 'keyhold' && <><label htmlFor="wallet-input-mode">输入方式</label><select id="wallet-input-mode" value={inputMode} onChange={event => { clearSecret(); setInputMode(event.target.value); }}><option value="text">粘贴 JSON</option><option value="file">选择文件</option></select></>}
      {format === 'keyhold' && inputMode === 'file' ? <><label htmlFor="wallet-key-file">KeyHold 文件</label><input id="wallet-key-file" type="file" accept=".json,application/json" ref={file} required /></> : <><label htmlFor="wallet-key-material">{format === 'keyhold' ? 'KeyHold JSON' : format === 'hex' ? 'Hex 私钥' : 'WIF 私钥'}</label><textarea id="wallet-key-material" ref={material} required rows={5} spellCheck={false} autoCapitalize="none" autoComplete="off" /></>}
      {format === 'keyhold' && <><label htmlFor="wallet-source-password">导入源密码</label><input id="wallet-source-password" type="password" ref={sourcePassword} autoComplete="current-password" required /></>}
      <div className="row"><button type="submit">{busy ? '正在解析…' : '解析'}</button><button type="button" className="ghost" onClick={back}>返回</button></div></fieldset></form> :
    mode === 'import' && step === 2 ? <><dl className="kv"><dt>检测到</dt><dd>{result?.format}</dd><dt>公钥</dt><dd className="mono">{preview?.publicKey}</dd><dt>派生地址</dt><dd className="mono">{preview?.address}</dd></dl><Network value={network} onChange={setNetwork} fixed={!!result?.network} /><label htmlFor="wallet-label">标签</label><input id="wallet-label" value={label} onChange={event => setLabel(event.target.value)} /><div className="row"><button type="button" onClick={() => setStep(3)}>下一步</button><button type="button" className="ghost" onClick={back}>返回</button></div></> :
    <form onSubmit={finish}><fieldset disabled={busy}>
      {mode === 'new' && <Network value={network} onChange={setNetwork} />}
      {mode === 'import' && format === 'keyhold' && <label className="check"><input type="checkbox" checked={reuse} onChange={event => setReuse(event.target.checked)} />使用导入源密码作为 Key 密码</label>}
      {!(mode === 'import' && format === 'keyhold' && reuse) ? <><label htmlFor="wallet-new-password">新密码（至少 8 位）</label><input id="wallet-new-password" type="password" autoComplete="new-password" minLength={8} ref={password} required /><label htmlFor="wallet-confirm-password">确认密码</label><input id="wallet-confirm-password" type="password" autoComplete="new-password" minLength={8} ref={confirmation} required /></> : <p role="status">将使用第 2 步输入的导入源密码创建并解锁钱包。</p>}
      <label htmlFor="wallet-final-label">标签</label><input id="wallet-final-label" value={label} onChange={event => setLabel(event.target.value)} />
      <p className="hint">{network === 'main' ? '主网' : '测试网'} · 先保存加密 KeyHold，再用本次向导密码解锁。</p>
      <div className="row"><button type="submit" data-testid="wallet-init-submit">{busy ? '正在保存并解锁…' : mode === 'new' ? '创建钱包' : '创建并导入'}</button><button type="button" className="ghost" onClick={back}>返回</button></div></fieldset></form>}
    </>}
  </div></WalletFrame>;
}

export function WalletUnlock({ onReady, credential }) {
  const input = useRef(null), pending = useRef(false), active = useRef(true);
  const [busy, setBusy] = useState(false), [error, setError] = useState('');
  const unlock = async value => {
    if (pending.current) return;
    pending.current = true; setBusy(true); setError('');
    try { await (await getClient()).unlock(value); if (active.current) await onReady(); }
    catch (cause) { if (active.current) setError(cause instanceof Error ? cause.message : String(cause)); }
    finally { value = ''; if (credential) credential.password = ''; pending.current = false; if (active.current) setBusy(false); }
  };
  useEffect(() => {
    active.current = true;
    if (credential?.password && !pending.current) {
      const value = credential.password; credential.password = '';
      void unlock(value);
    }
    return () => { active.current = false; if (input.current) input.current.value = ''; if (credential) credential.password = ''; };
  }, []);
  return <WalletFrame><h1>钱包已锁定</h1><p>输入 Key 密码解锁后台钱包。更换身份需要先重置钱包。</p><form onSubmit={event => {
    event.preventDefault(); if (pending.current) return;
    const value = input.current.value; input.current.value = ''; void unlock(value);
  }}><label htmlFor="wallet-unlock-password">密码</label><input id="wallet-unlock-password" ref={input} type="password" autoComplete="current-password" autoFocus required disabled={busy} /><p role="alert" className={error ? 'error' : undefined}>{error}</p><button type="submit" disabled={busy}>{busy ? '正在解锁…' : '解锁'}</button></form></WalletFrame>;
}
