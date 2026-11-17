import React, { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react';
import { getClient, getInstanceId } from './client.js';
import { watchTopics } from './subscribe.js';
import { issueLaunchToken, registerLaunchedApp, teardownLaunchedApp, launchedAppState } from './connect.js';

export const ApplicationWorkspace = createContext(null);
/** 所有入口使用同一个启动和视图生命周期。 */
export function useApplicationWorkspace() { return useContext(ApplicationWorkspace); }

/** 内嵌视图由工作区持有；切换只隐藏，不移动或重建 iframe，避免重放一次性令牌。 */
export function useWorkspace({ status, info, conn }) {
  const outletRef = useRef(null);
  const registry = useRef(new Map());
  const [views, setViews] = useState([]);
  const [active, setActive] = useState(null);
  const [mode, setMode] = useState(null);
  const [settingsError, setSettingsError] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const launching = useRef(false);
  const generation = useRef(0);
  const ready = conn === 'connected' && !!status?.initialized && !(info?.locked ?? status?.locked);
  const runtimeRef = useRef({});
  runtimeRef.current = { ready, instance: getInstanceId() };

  /** 发布只含展示资料的快照，不把令牌或 Receiver 暴露给菜单。 */
  const publish = useCallback(() => setViews([...registry.current.values()].map(({ id, title, mode }) => ({ id, title, mode }))), []);
  /** 先撤回传输权威，再销毁浏览上下文；关闭视图不撤销持久授权。 */
  const close = useCallback((id) => {
    const view = registry.current.get(id);
    if (!view) return;
    registry.current.delete(id);
    if (import.meta.env.DEV) console.debug("[workspace] application closed", { origin: id, mode: view.mode });
    if (view.handle) teardownLaunchedApp(view.handle);
    view.frame?.remove();
    if (view.child && !view.child.closed) view.child.close();
    setActive(current => current === id ? null : current);
    publish();
  }, [publish]);
  const select = useCallback((id) => {
    const view = registry.current.get(id);
    if (view?.mode === 'popup') { view.child.focus(); return; }
    setActive(id);
  }, []);
  useEffect(() => {
    for (const view of registry.current.values()) if (view.frame) view.frame.hidden = view.id !== active;
  }, [active, views]);
  useEffect(() => {
    if (conn !== 'connected' || !status?.initialized) return;
    let disposed = false;
    const load = async () => {
      try {
        const settings = await (await getClient()).getSettings();
        if (disposed) return;
        if (!['iframe', 'popup'].includes(settings.applicationOpenMode)) throw new Error('Application settings unavailable');
        setMode(settings.applicationOpenMode); setSettingsError('');
      } catch (cause) { if (!disposed) { setMode(null); setSettingsError(String(cause.message || cause)); } }
    };
    load();
    const stop = watchTopics(['settings'], load);
    return () => { disposed = true; stop(); };
  }, [conn, status?.initialized]);
  const instance = getInstanceId();
  useEffect(() => {
    // 后台代际变化不能把旧启动接到新钱包；仅断网不销毁已有页面。
    generation.current++;
    for (const id of [...registry.current.keys()]) close(id);
  }, [instance, close]);
  useEffect(() => { if (!ready) { generation.current++; } }, [ready]);
  useEffect(() => {
    const sweep = setInterval(() => {
      for (const view of registry.current.values()) if (view.child?.closed) close(view.id);
    }, 1000);
    const release = () => {
      generation.current++;
      for (const id of [...registry.current.keys()]) close(id);
    };
    window.addEventListener('pagehide', release);
    return () => { clearInterval(sweep); window.removeEventListener('pagehide', release); release(); };
  }, [close]);

  useEffect(() => watchTopics(['connect'], async () => {
    // 未初始化/锁定不查询应用中心：初始化前仅允许状态读取、创建/导入、订阅与只读诊断。
    if (conn !== 'connected' || !status?.initialized || (info?.locked ?? status?.locked)) return;
    const result = await (await getClient()).call(undefined, 'connect.apps.list', {});
    const ids = new Set((result.connections || []).map(item => item.authId));
    for (const view of registry.current.values()) {
      const state = launchedAppState(view.handle);
      if (state.authorizationIds?.length && !state.authorizationIds.some(id => ids.has(id))) close(view.id);
    }
  }), [close, conn, status?.initialized, info?.locked, status?.locked]);
  /** 同一 App 的已有视图优先复用；未知身份只按完整入口匹配。 */
  const requestLaunch = async target => {
    if (!ready || !mode || launching.current) return;
    const expected = target.expectedAppPublicKeyHex?.toLowerCase();
    const findExisting = () => {
      for (const view of registry.current.values()) {
        const state = launchedAppState(view.handle);
        const matches = expected ? (state.applicationKey || view.expected)?.toLowerCase() === expected
          : target.url ? view.url === new URL(target.url).href : target.indexseedhashhex && view.seed === target.indexseedhashhex;
        if (!matches) continue;
        if (!state.valid || view.child?.closed || (view.frame && !view.frame.isConnected)) { close(view.id); continue; }
        select(view.id); return true;
      }
      return false;
    };
    setError('');
    if (findExisting()) return;
    const request = { ...target, mode, instance, generation: generation.current };
    const assertCurrent = () => {
      if (!runtimeRef.current.ready || runtimeRef.current.instance !== request.instance || generation.current !== request.generation) throw new Error('Wallet runtime changed; open the application again');
    };
    let child = null, view = null;
    launching.current = true; setBusy(true);
    try {
      assertCurrent();
      if (request.mode === 'popup') { child = window.open('about:blank', '_blank'); if (!child) throw new Error('请允许浏览器打开应用窗口。'); }
      const prepared = request.prepare ? await request.prepare() : { url: request.url, expectedAppPublicKeyHex: expected };
      assertCurrent();
      const url = new URL(prepared.url);
      if (!['http:', 'https:'].includes(url.protocol) || url.origin === window.location.origin) throw new Error('应用需要独立的 HTTP 来源。');
      const appKey = prepared.expectedAppPublicKeyHex || expected;
      const id = appKey ? `app:${appKey.toLowerCase()}` : `url:${url.href}`;
      close(id);
      let frame = null;
      if (request.mode === 'iframe') {
        if (!outletRef.current?.isConnected) throw new Error('Application workspace unavailable');
        frame = document.createElement('iframe'); frame.title = request.title || url.host;
        frame.className = 'workspace-app-frame'; frame.dataset.testid = request.testId || 'application-frame';
        frame.setAttribute('sandbox', 'allow-scripts allow-same-origin allow-forms allow-downloads'); frame.referrerPolicy = 'no-referrer'; outletRef.current.append(frame);
      }
      view = { id, expected: appKey, url: url.href, seed: target.indexseedhashhex, title: request.title || url.host, mode: request.mode, frame, child };
      registry.current.set(id, view);
      const source = frame ? frame.contentWindow : child;
      if (!source) throw new Error('Application browsing context unavailable');
      view.handle = registerLaunchedApp({ origin: url.origin, getSource: () => source, kind: frame ? 'iframe' : 'window', child });
      publish();
      const issued = await issueLaunchToken(url.origin, prepared);
      assertCurrent();
      if (registry.current.get(id) !== view || source.closed) throw new Error('Application has closed');
      if (!issued?.launchToken) throw new Error('Launch token unavailable');
      url.searchParams.set('launchToken', issued.launchToken); url.searchParams.set('sessionWindowOrigin', window.location.origin); url.searchParams.set('mode', frame ? 'iframe' : 'appView');
      if (frame) { frame.src = url.href; setActive(id); } else child.location.replace(url.href);
    } catch (cause) {
      if (view && registry.current.get(view.id) === view) close(view.id); else if (child && !child.closed) child.close();
      setError('应用未能打开，请稍后重试。');
      if (import.meta.env.DEV) console.debug('[workspace] launch.failed', { name: cause?.name, message: '启动或部署准备失败' });
    } finally { launching.current = false; setBusy(false); }
  };
  return { views, active, select, close, requestLaunch, outletRef, busy, error, ready: ready && !!mode, mode, settingsError };
}

/** 标签使用普通按钮与明确的键盘导航；不依赖尚未普及的 focusgroup。 */
export function WorkspaceTabs({ workspace: w, title }) {
  const tabs = useRef(null);
  useEffect(() => {
    const selected = tabs.current?.querySelector('[role="tab"][aria-selected="true"]');
    if (selected) tabs.current.scrollLeft += selected.getBoundingClientRect().left < tabs.current.getBoundingClientRect().left
      ? selected.getBoundingClientRect().left - tabs.current.getBoundingClientRect().left
      : Math.max(0, selected.getBoundingClientRect().right - tabs.current.getBoundingClientRect().right);
  }, [w.active, w.views]);
  const onKeyDown = event => {
    const buttons = [...tabs.current.querySelectorAll('[role="tab"]')];
    const index = buttons.indexOf(document.activeElement);
    if (index < 0 || !['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
    event.preventDefault();
    const next = event.key === 'Home' ? 0 : event.key === 'End' ? buttons.length - 1 : (index + (event.key === 'ArrowLeft' ? -1 : 1) + buttons.length) % buttons.length;
    buttons[next].focus(); buttons[next].click();
  };
  return <div ref={tabs} className="workspace-tabs" role="tablist" aria-label="工作区标签" onKeyDown={onKeyDown}>
    <button id="internal-tab" type="button" role="tab" aria-selected={!w.active} tabIndex={!w.active ? 0 : -1} onClick={() => w.select(null)}>{title}</button>
    {w.views.filter(view => view.mode === 'iframe').map(view => <div className="workspace-tab-app" key={view.id}>
      <button id={`tab-${encodeURIComponent(view.id)}`} type="button" role="tab" aria-selected={w.active === view.id} tabIndex={w.active === view.id ? 0 : -1} onClick={() => w.select(view.id)}>{view.title}</button>
      <button type="button" className="workspace-close" aria-label={`关闭 ${view.title}`} onClick={() => w.close(view.id)}>×</button>
    </div>)}
  </div>;
}
