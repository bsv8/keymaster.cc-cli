import React, { useEffect, useId, useMemo, useRef, useState } from 'react';
import { diagnosticsSnapshot, subscribeDiagnostics, safeDiagnostic, buildCopyPayload } from './diagnostics.js';
import { getInstanceId, getConnectionStatus, onConnectionStatus, retryConnection, refreshBackendInspect, getConnectionGeneration } from './client.js';
import { t } from './i18n.js';
import catalog from '../../pluginmanifest/catalog.json';
import { PLUGIN_GRAPH } from './generated/pluginGraph.js';

function webStatusFor(pluginId, plugins) {
  const p = (plugins || []).find((x) => x.id === pluginId);
  if (!p) return { kind: 'unread', label: '未读取' };
  if (p.kind === 'enabled') return { kind: 'ready', label: '已装配', detail: p };
  if (p.kind === 'failed' || p.kind === 'blocked' || p.kind === 'cleanup-pending') {
    return { kind: p.kind, label: p.kind === 'failed' ? '失败' : p.kind === 'blocked' ? '阻塞' : p.kind, detail: p };
  }
  return { kind: p.kind || 'unknown', label: '状态未知', detail: p };
}

function goStatusFor(pluginId, backend) {
  if (!backend || backend.status === 'unread') return { kind: 'unread', label: '未读取' };
  if (backend.status === 'error') return { kind: 'unknown', label: '状态未知' };
  if (backend.status === 'stale') return { kind: 'stale', label: '已失效' };
  const units = (backend.data && backend.data.units) || [];
  const u = units.find((x) => x.pluginId === pluginId);
  if (u) return { kind: 'ready', label: '已装配', detail: u };
  return { kind: 'unknown', label: '状态未知' };
}

function formatTime(ms) {
  if (!ms) return '—';
  try {
    return new Date(ms).toLocaleString();
  } catch {
    return String(ms);
  }
}

const GROUP_W = 320;
const GROUP_H = 190;
const GRID_COLS = 3;
const GRID_GAP_X = 36;
const GRID_GAP_Y = 28;
const GRID_PAD = 24;

function layoutGroups(pluginIds) {
  const positions = new Map();
  pluginIds.forEach((id, i) => {
    const col = i % GRID_COLS;
    const row = Math.floor(i / GRID_COLS);
    positions.set(id, {
      x: GRID_PAD + col * (GROUP_W + GRID_GAP_X),
      y: GRID_PAD + row * (GROUP_H + GRID_GAP_Y),
    });
  });
  const rows = Math.ceil(pluginIds.length / GRID_COLS);
  const cols = Math.min(GRID_COLS, pluginIds.length);
  return {
    positions,
    width: GRID_PAD * 2 + cols * GROUP_W + (cols - 1) * GRID_GAP_X,
    height: GRID_PAD * 2 + rows * GROUP_H + (rows - 1) * GRID_GAP_Y,
  };
}

function GraphModal({ title, onClose, testId, children }) {
  const ref = useRef(null);
  const titleId = useId();
  useEffect(() => {
    const el = ref.current;
    el.showModal();
    return () => {
      try { el.close(); } catch { /* already closed */ }
    };
  }, []);
  return (
    <dialog
      ref={ref}
      className="ui-modal"
      data-testid={testId}
      aria-labelledby={titleId}
      onCancel={(e) => { e.preventDefault(); onClose(); }}
    >
      <div className="ui-modal__panel">
        <button type="button" className="ui-modal__close" onClick={onClose} aria-label={t('取消')}>×</button>
        <h2 id={titleId} className="ui-modal__header">{title}</h2>
        <div className="ui-modal__body">{children}</div>
      </div>
    </dialog>
  );
}

export function PluginDiagnosticsPage({ standalone = false }) {
  const [snapshot, setSnapshot] = useState(() => diagnosticsSnapshot());
  const [conn, setConn] = useState(() => getConnectionStatus());
  const [instance, setInstance] = useState(() => getInstanceId());
  const [generation, setGeneration] = useState(() => getConnectionGeneration());
  const [query, setQuery] = useState('');
  const [functionQuery, setFunctionQuery] = useState('');
  const [direction, setDirection] = useState('outgoing');
  const [selected, setSelected] = useState('');
  const [hovered, setHovered] = useState('');
  const [zoom, setZoom] = useState(1);
  const [showWeb, setShowWeb] = useState(true);
  const [showGo, setShowGo] = useState(true);
  const [showCross, setShowCross] = useState(true);
  const [showAssembly, setShowAssembly] = useState(true);
  const [collapsed, setCollapsed] = useState({});
  const [linkedMethod, setLinkedMethod] = useState('');
  const [busy, setBusy] = useState('');
  const [message, setMessage] = useState('');

  useEffect(() => subscribeDiagnostics(() => {
    setSnapshot(diagnosticsSnapshot());
    setInstance(getInstanceId());
    setGeneration(getConnectionGeneration());
  }), []);
  useEffect(() => onConnectionStatus((s) => {
    setConn(s);
    setInstance(getInstanceId());
    setGeneration(getConnectionGeneration());
    if (s === 'connected') {
      void refreshBackendInspect().catch(() => {}).finally(() => {
        setSnapshot(diagnosticsSnapshot());
        setInstance(getInstanceId());
      });
    } else {
      setSnapshot(diagnosticsSnapshot());
    }
  }), []);
  useEffect(() => {
    void refreshBackendInspect().catch(() => {}).finally(() => setSnapshot(diagnosticsSnapshot()));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const backend = snapshot.backend || { status: 'unread', stale: true };
  const frontendPlugins = snapshot.plugins || [];

  const graph = useMemo(() => PLUGIN_GRAPH, []);
  const pluginIds = useMemo(() => graph.plugins.map((p) => p.pluginId), [graph]);
  const layout = useMemo(() => layoutGroups(pluginIds), [pluginIds]);

  const webCallsByPlugin = useMemo(() => {
    const m = new Map(pluginIds.map((id) => [id, []]));
    graph.webCalls.forEach((w, i) => {
      if (m.has(w.pluginId)) m.get(w.pluginId).push({ ...w, index: i });
    });
    return m;
  }, [graph, pluginIds]);
  const goHandlersByPlugin = useMemo(() => {
    const m = new Map(pluginIds.map((id) => [id, []]));
    graph.goHandlers.forEach((h) => {
      if (m.has(h.pluginId)) m.get(h.pluginId).push(h);
    });
    return m;
  }, [graph, pluginIds]);
  const goCallsByPlugin = useMemo(() => {
    const m = new Map(pluginIds.map((id) => [id, []]));
    graph.goCalls.forEach((c) => {
      const owner = c.callerPluginId && m.has(c.callerPluginId) ? c.callerPluginId : null;
      if (owner) m.get(owner).push(c);
    });
    return m;
  }, [graph, pluginIds]);

  const needle = query.trim().toLowerCase();
  const matching = useMemo(() => {
    if (!needle) return new Set(pluginIds);
    return new Set(
      pluginIds.filter((id) => {
        const p = graph.plugins.find((x) => x.pluginId === id);
        const hay = `${id} ${p?.name || ''} ${p?.webUnitId || ''} ${p?.goUnitId || ''} ${(p?.methods || []).join(' ')} ${(webCallsByPlugin.get(id) || []).map((w) => `${w.method} ${w.caller} ${w.file}`).join(' ')} ${(goHandlersByPlugin.get(id) || []).map((h) => `${h.method} ${h.handler} ${h.implFile}`).join(' ')}`.toLowerCase();
        return hay.includes(needle);
      }),
    );
  }, [needle, pluginIds, graph, webCallsByPlugin, goHandlersByPlugin]);

  const focus = hovered || selected;
  const related = useMemo(() => {
    if (!focus) return new Set(pluginIds);
    return new Set([focus]);
  }, [focus, pluginIds]);

  const methodDiff = useMemo(() => {
    const frontByMethod = new Map();
    for (const u of catalog) for (const m of u.methods || []) frontByMethod.set(m, u.pluginId);
    const backCatalog = (backend.data && backend.data.catalog) || null;
    if (!backCatalog) return [];
    const backByMethod = new Map();
    for (const u of backCatalog) for (const m of u.methods || []) backByMethod.set(m, u.pluginId);
    const diffs = [];
    for (const [m, owner] of frontByMethod) {
      if (!backByMethod.has(m)) diffs.push({ method: m, front: owner, back: '', kind: 'missing-backend' });
      else if (backByMethod.get(m) !== owner) diffs.push({ method: m, front: owner, back: backByMethod.get(m), kind: 'owner-mismatch' });
    }
    for (const [m, owner] of backByMethod) {
      if (!frontByMethod.has(m)) diffs.push({ method: m, front: '', back: owner, kind: 'missing-front' });
    }
    return diffs;
  }, [backend]);

  const selectedPlugin = useMemo(() => graph.plugins.find((p) => p.pluginId === selected) || null, [graph, selected]);
  const selectedWebCalls = useMemo(() => (selected ? webCallsByPlugin.get(selected) || [] : []), [selected, webCallsByPlugin]);
  const selectedGoCalls = useMemo(() => (selected ? goCallsByPlugin.get(selected) || [] : []), [selected, goCallsByPlugin]);
  const selectedCross = useMemo(() => graph.crossEnd.filter((c) => c.pluginId === selected), [graph, selected]);
  const selectedWebStatus = selected ? webStatusFor(selected, frontendPlugins) : null;
  const selectedGoStatus = selected ? goStatusFor(selected, backend) : null;

  const callsForDirection = useMemo(() => {
    if (!selected) return [];
    if (direction === 'outgoing') {
      return [
        ...selectedWebCalls.map((w) => ({ side: 'web', caller: w.caller, capability: w.capability, method: w.method, providerUnit: w.providerUnit, file: w.file, line: w.line, kind: w.kind, via: w.via })),
        ...selectedGoCalls.map((c) => ({ side: 'go', caller: c.callerHandler, capability: '', method: c.callee, providerUnit: '', file: c.file, line: c.line, kind: c.kind, ownership: c.ownership })),
      ];
    }
    return selectedCross
      .filter((c) => c.goHandler)
      .map((c) => ({ side: 'cross', caller: `web:${c.pluginId}`, capability: `${c.pluginId}.api`, method: c.method, providerUnit: graph.plugins.find((p) => p.pluginId === c.pluginId)?.goUnitId || '', file: c.goHandler.implFile, line: c.goHandler.implLine, kind: 'cross', handler: c.goHandler.handler, regFile: c.goHandler.regFile, regLine: c.goHandler.regLine }));
  }, [selected, direction, selectedWebCalls, selectedGoCalls, selectedCross, graph]);

  const filteredCalls = useMemo(() => {
    const q = functionQuery.trim().toLowerCase();
    if (!q) return callsForDirection;
    return callsForDirection.filter((c) => `${c.caller} ${c.capability} ${c.method} ${c.providerUnit || ''} ${c.file}`.toLowerCase().includes(q));
  }, [callsForDirection, functionQuery]);

  const connLabel = conn === 'connected' ? t('已连接') : conn === 'connecting' ? t('连接中') : conn === 'protocol-incompatible' ? t('协议不兼容') : t('已断开');
  const backendInstance = (backend.data && backend.data.instanceId) || instance || '';
  const goRuntimeId = (backend.data && backend.data.runtimeInstanceId) || '';
  const goActive = backend.data ? backend.data.active : null;
  const backendAt = backend.fetchedAt || 0;
  const webRuntimeStatus = snapshot.frontend?.status === 'closed' ? t('已断开') : snapshot.error ? t('失败') : frontendPlugins.length ? t('已装配') : t('未读取');

  const openPlugin = (id) => {
    setSelected(id);
    setFunctionQuery('');
    setDirection('outgoing');
    setLinkedMethod('');
  };

  const doRefresh = async () => {
    setBusy('refresh');
    setMessage('');
    try {
      await refreshBackendInspect();
      setSnapshot(diagnosticsSnapshot());
      setInstance(getInstanceId());
      setGeneration(getConnectionGeneration());
    } catch (error) {
      setMessage(safeDiagnostic(error));
    } finally {
      setBusy('');
    }
  };

  const doReconnect = async () => {
    setBusy('reconnect');
    setMessage('');
    try {
      await retryConnection();
      await refreshBackendInspect().catch(() => {});
      setSnapshot(diagnosticsSnapshot());
    } catch (error) {
      setMessage(safeDiagnostic(error));
    } finally {
      setBusy('');
    }
  };

  const doCopy = async () => {
    setMessage('');
    try {
      const plugins = catalog.map((u) => {
        const w = webStatusFor(u.pluginId, frontendPlugins);
        const g = goStatusFor(u.pluginId, backend);
        const backUnits = ((backend.data && backend.data.units) || []).filter((x) => x.pluginId === u.pluginId);
        return {
          pluginId: u.pluginId,
          webUnitId: `${u.pluginId}.web`,
          goUnitId: u.unitId,
          webInstanceId: (w.detail && (w.detail.instanceId || (w.detail.units || []).map((x) => x.instanceId).filter(Boolean).join(','))) || '',
          goInstanceIds: backUnits.map((x) => x.instanceId),
          webStatus: w.kind,
          goStatus: g.kind,
          methods: u.methods || [],
        };
      });
      const payload = buildCopyPayload({
        connection: { status: conn, generation, backendInstanceId: instance },
        backendInstanceId: backendInstance,
        webRuntime: { status: snapshot.frontend?.status || 'unread', generation },
        goRuntime: { runtimeInstanceId: goRuntimeId, active: goActive },
        snapshotAt: backendAt,
        stale: backend.stale === true || backend.status === 'stale',
        plugins,
        dependencies: catalog.map((u) => ({ pluginId: u.pluginId, crossRuntime: `${u.pluginId}.web -> ${u.unitId}`, methods: u.methods || [] })),
        methodDiff,
        error: backend.error || (snapshot.error ? { stage: 'frontend', code: 'FRONTEND', message: snapshot.error.slice(0, 500) } : null),
      });
      await navigator.clipboard.writeText(safeDiagnostic(payload));
      setMessage(t('已复制'));
    } catch {
      setMessage(t('复制失败，请检查剪贴板权限。'));
    }
  };

  const crossCount = graph.crossEnd.length;
  const assemblyCount = graph.assemblyEdges.length;

  return (
    <div className="plugin-manager">
      <header className="ui-page-header">
        <div className="ui-page-header__content">
          <h1 className="ui-page-header__title">{t('插件')}</h1>
          <p className="ui-page-header__description">{t('查看插件之间的依赖；点击插件查看函数调用关系。')}</p>
        </div>
        <div className="ui-page-header__actions">
          <button type="button" className="ghost" disabled={busy !== ''} onClick={doRefresh}>{t('刷新后台状态')}</button>
          <button type="button" className="ghost" disabled={busy !== ''} onClick={doReconnect}>{t('重新连接并重建前台')}</button>
          <button type="button" className="ghost" onClick={doCopy}>{t('复制诊断信息')}</button>
        </div>
      </header>
      {message && <p role="status" className="pg-status">{message}</p>}

      <section className="card pg-context" aria-label={t('当前运行上下文')}>
        <h2>{t('当前运行上下文')}</h2>
        <dl className="kv pg-context__grid">
          <dt>{t('Web 连接')}</dt>
          <dd>{connLabel} · gen {generation} · {t('后台实例 ID')}: <code className="mono">{instance || '—'}</code></dd>
          <dt>{t('后台实例 ID')}</dt>
          <dd><code className="mono">{backendInstance || '—'}</code></dd>
          <dt>{t('Go Runtime ID')}</dt>
          <dd><code className="mono">{goRuntimeId || '—'}</code> · active: {goActive === null ? t('未读取') : String(goActive)}</dd>
          <dt>{t('当前页面 Web Runtime')}</dt>
          <dd>{webRuntimeStatus}</dd>
          <dt>{t('Go Runtime 最近读取')}</dt>
          <dd>{backend.status === 'unread' ? t('未读取') : `${formatTime(backendAt)}${backend.stale ? ` · ${t('已失效')}` : ''}`}</dd>
        </dl>
        {(snapshot.error || backend.error) && (
          <>
            {snapshot.error && (
              <div role="alert" className="pg-problem">
                <strong>{t('前台异常')}</strong>
                <pre>{snapshot.error}</pre>
              </div>
            )}
            {backend.error && (
              <div role="alert" className="pg-problem">
                <strong>{t('后台读取失败')}</strong>
                <pre>{safeDiagnostic(backend.error)}</pre>
              </div>
            )}
          </>
        )}
        {methodDiff.length > 0 && (
          <div role="alert" className="pg-problem pg-problem--warn">
            <strong>{t('目录不一致')} ({methodDiff.length})</strong>
            <pre>{safeDiagnostic(methodDiff.slice(0, 20))}</pre>
          </div>
        )}
        <p className="hint">{t('业务运行请查看任务页面，不能从插件已装配推断')}</p>
        {standalone && (
          <div className="row">
            <a className="ghost" href="/">{t('返回初始化')}</a>
            <a className="ghost" href="/">{t('返回解锁')}</a>
          </div>
        )}
      </section>

      <div className="dependency-toolbar">
        <label className="pg-search">
          <input data-testid="pg-search" value={query} onChange={(e) => setQuery(e.target.value)} placeholder={t('搜索插件（名称、ID、方法）')} aria-label={t('搜索插件')} />
        </label>
        <label className="dependency-page-toggle"><input type="checkbox" checked={showWeb} onChange={(e) => setShowWeb(e.target.checked)} />{t('显示 Web 单元')}</label>
        <label className="dependency-page-toggle"><input type="checkbox" checked={showGo} onChange={(e) => setShowGo(e.target.checked)} />{t('显示 Go 单元')}</label>
        <label className="dependency-page-toggle"><input type="checkbox" checked={showCross} onChange={(e) => setShowCross(e.target.checked)} />{t('显示跨端调用')}</label>
        <label className="dependency-page-toggle"><input type="checkbox" checked={showAssembly} onChange={(e) => setShowAssembly(e.target.checked)} />{t('显示装配依赖')}</label>
        <div className="dependency-zoom">
          <button type="button" className="ghost" aria-label={t('缩小')} disabled={zoom <= 0.6} onClick={() => setZoom((v) => Math.max(0.6, v - 0.2))}>−</button>
          <button type="button" className="ghost" onClick={() => setZoom(1)}>{Math.round(zoom * 100)}%</button>
          <button type="button" className="ghost" aria-label={t('放大')} disabled={zoom >= 2} onClick={() => setZoom((v) => Math.min(2, v + 0.2))}>+</button>
        </div>
      </div>
      <p className="dependency-legend">
        <span>{t('箭头：调用方 → 提供方 · 虚线：可选依赖 · 点击节点查看函数详情')}</span>
        <span>{graph.plugins.length} {t('个插件')} · {graph.units.length} {t('个单元')} · {crossCount} {t('条跨端关系')} · {assemblyCount} {t('条装配依赖')}</span>
      </p>
      {matching.size === 0 && <p role="status">{t('无匹配插件')}</p>}
      <div className="dependency-canvas" tabIndex={0} aria-label={t('插件依赖关系')} data-testid="pg-canvas">
        <svg className="dependency-graph" viewBox={`0 0 ${layout.width} ${layout.height}`} style={{ width: `${zoom * 100}%`, minWidth: `${zoom * Math.min(layout.width, 1100)}px` }} role="group" aria-label={t('插件依赖关系')}>
          <defs>
            <marker id="pg-arrow" markerWidth="8" markerHeight="8" refX="7" refY="4" orient="auto" markerUnits="strokeWidth">
              <path d="M0,0 L8,4 L0,8 Z" />
            </marker>
          </defs>
          {pluginIds.map((id) => {
            const pos = layout.positions.get(id);
            const p = graph.plugins.find((x) => x.pluginId === id);
            const isMatch = matching.has(id);
            const isRelated = related.has(id);
            const isFocus = focus === id;
            const isCollapsed = !!collapsed[id];
            const wStatus = webStatusFor(id, frontendPlugins);
            const gStatus = goStatusFor(id, backend);
            const dimmed = (!isMatch || !isRelated) && (needle || focus);
            return (
              <g key={id} className={`pg-group${isFocus ? ' is-focused' : ''}`} transform={`translate(${pos.x},${pos.y})`} opacity={dimmed ? 0.3 : 1}>
                <rect width={GROUP_W} height={isCollapsed ? 64 : GROUP_H} rx="10" />
                <text x="14" y="26" className="pg-group__name">{t(p.name)}</text>
                <text x="14" y="46" className="pg-group__meta">{id} · {t(wStatus.label)}/{t(gStatus.label)}</text>
                <g
                  role="button"
                  tabIndex={0}
                  aria-label={`${t(p.name)}: ${t('查看函数详情')}`}
                  data-plugin-group={id}
                  data-testid={`pg-group-${id}`}
                  onClick={() => (selected === id ? setSelected('') : openPlugin(id))}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault();
                      if (selected === id) setSelected('');
                      else openPlugin(id);
                    }
                  }}
                  onMouseEnter={() => setHovered(id)}
                  onMouseLeave={() => setHovered('')}
                >
                  <rect x="0" y="0" width={GROUP_W} height="56" rx="10" fill="transparent" />
                </g>
                <g
                  className="pg-toggle"
                  role="button"
                  tabIndex={0}
                  aria-label={isCollapsed ? t('展开分组') : t('折叠分组')}
                  transform={`translate(${GROUP_W - 64},10)`}
                  onClick={(e) => {
                    e.stopPropagation();
                    setCollapsed((c) => ({ ...c, [id]: !c[id] }));
                  }}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault();
                      e.stopPropagation();
                      setCollapsed((c) => ({ ...c, [id]: !c[id] }));
                    }
                  }}
                >
                  <rect width="50" height="22" rx="6" />
                  <text x="25" y="16" textAnchor="middle">{isCollapsed ? '+' : '−'}</text>
                </g>
                {!isCollapsed && (
                  <>
                    {showWeb && (
                      <g
                        className="pg-unit"
                        role="button"
                        tabIndex={0}
                        aria-label={`${id}.web ${t(wStatus.label)}`}
                        data-unit-node={`${id}.web`}
                        transform="translate(14,64)"
                        onClick={() => openPlugin(id)}
                        onKeyDown={(e) => {
                          if (e.key === 'Enter' || e.key === ' ') {
                            e.preventDefault();
                            openPlugin(id);
                          }
                        }}
                        onMouseEnter={() => setHovered(id)}
                        onMouseLeave={() => setHovered('')}
                      >
                        <rect width={GROUP_W - 28} height="44" rx="8" />
                        <text x="12" y="19" className="pg-unit__title">{id}.web</text>
                        <text x="12" y="36" className="pg-unit__meta">
                          {t('Web 单元')} · {t(wStatus.label)}
                          {wStatus.detail?.instanceId ? ` · ${wStatus.detail.instanceId}` : ''}
                        </text>
                        <circle cx={GROUP_W - 44} cy="22" r="4" fill={wStatus.kind === 'ready' ? 'var(--ok)' : wStatus.kind === 'failed' || wStatus.kind === 'blocked' ? 'var(--danger)' : 'var(--muted)'} />
                      </g>
                    )}
                    {showGo && (
                      <g
                        className="pg-unit"
                        role="button"
                        tabIndex={0}
                        aria-label={`${p.goUnitId} ${t(gStatus.label)}`}
                        data-unit-node={p.goUnitId}
                        transform="translate(14,118)"
                        onClick={() => openPlugin(id)}
                        onKeyDown={(e) => {
                          if (e.key === 'Enter' || e.key === ' ') {
                            e.preventDefault();
                            openPlugin(id);
                          }
                        }}
                        onMouseEnter={() => setHovered(id)}
                        onMouseLeave={() => setHovered('')}
                      >
                        <rect width={GROUP_W - 28} height="44" rx="8" />
                        <text x="12" y="19" className="pg-unit__title">{p.goUnitId}</text>
                        <text x="12" y="36" className="pg-unit__meta">
                          {t('Go 单元')} · {t(gStatus.label)}
                          {gStatus.detail?.instanceId ? ` · ${gStatus.detail.instanceId}` : ''}
                        </text>
                        <circle cx={GROUP_W - 44} cy="22" r="4" fill={gStatus.kind === 'ready' ? 'var(--ok)' : gStatus.kind === 'stale' ? 'var(--warn)' : 'var(--muted)'} />
                      </g>
                    )}
                    {showWeb && showGo && showCross && (
                      <line className="pg-cross" x1={GROUP_W / 2} y1={108} x2={GROUP_W / 2} y2={116} markerEnd="url(#pg-arrow)" />
                    )}
                  </>
                )}
              </g>
            );
          })}
          {showAssembly &&
            assemblyCount > 0 &&
            graph.assemblyEdges.map((e) => {
              const from = layout.positions.get(e.consumerUnit?.split('.')[0] || '');
              const to = layout.positions.get(e.providerUnit?.split('.')[0] || '');
              if (!from || !to) return null;
              const highlighted = focus ? e.consumerUnit?.split('.')[0] === focus || e.providerUnit?.split('.')[0] === focus : false;
              return <line key={`${e.consumerUnit}:${e.providerUnit}`} className={`pg-assembly${e.optional ? ' is-optional' : ''}${highlighted ? ' is-highlighted' : ''}`} x1={from.x + GROUP_W / 2} y1={from.y} x2={to.x + GROUP_W / 2} y2={to.y} markerEnd="url(#pg-arrow)" />;
            })}
        </svg>
      </div>
      <p className="hint">{t('静态调用来自实际源码分析；实时状态来自双端运行快照，两者明确区分。')}</p>
      <p className="hint">{t('该页面不触发创建钱包、解锁或业务初始化')}</p>
      <p className="hint">{t('重新连接并重建前台')} · {t('会影响本页持有的 Connect 连接')} · {t('不会重启 Go 后台或取消后台下载')}</p>

      {selectedPlugin && (
        <GraphModal
          title={`${t(selectedPlugin.name)} · ${selectedPlugin.pluginId}`}
          onClose={() => { setSelected(''); setHovered(''); }}
          testId="pg-detail"
        >
          <p className="pg-detail-note">{t('静态调用来自实际源码分析；实时状态来自双端运行快照，两者明确区分。')}</p>
          <div className="dependency-detail-tabs" role="group" aria-label={t('调用方向')}>
            <button type="button" className="ghost" aria-pressed={direction === 'outgoing'} onClick={() => setDirection('outgoing')}>{t('调用')}</button>
            <button type="button" className="ghost" aria-pressed={direction === 'incoming'} onClick={() => setDirection('incoming')}>{t('被调用')}</button>
            <label className="pg-func-search">
              <input value={functionQuery} onChange={(e) => setFunctionQuery(e.target.value)} placeholder={t('函数搜索')} aria-label={t('函数搜索')} />
            </label>
          </div>
          <div>
            <h3>{t('职责与边界')}</h3>
            <p>{t(selectedPlugin.duties)}</p>
            <p className="hint">{t(selectedPlugin.boundary)}</p>
          </div>
          <div>
            <h3>{t('实时运行状态')}</h3>
            <dl className="kv pg-context__grid">
              <dt>{t('Web 单元')}</dt>
              <dd><code className="mono">{selectedPlugin.webUnitId}</code> · {selectedWebStatus ? t(selectedWebStatus.label) : '—'}{selectedWebStatus?.detail?.instanceId ? ` · ${selectedWebStatus.detail.instanceId}` : ''}</dd>
              <dt>{t('Go 单元')}</dt>
              <dd><code className="mono">{selectedPlugin.goUnitId}</code> · {selectedGoStatus ? t(selectedGoStatus.label) : '—'}{selectedGoStatus?.detail?.instanceId ? ` · ${selectedGoStatus.detail.instanceId}` : ''}</dd>
            </dl>
          </div>
          <div>
            <h3>{t('对外管理方法')}</h3>
            <div className="dependency-detail-summary">
              {(selectedPlugin.methods || []).map((m) => (
                <button key={m} type="button" className={linkedMethod === m ? 'is-active' : ''} onClick={() => setLinkedMethod(linkedMethod === m ? '' : m)}>{m}</button>
              ))}
            </div>
            {linkedMethod && (
              <div role="status" className="function-dependency">
                {(() => {
                  const c = graph.crossEnd.find((x) => x.pluginId === selected && x.method === linkedMethod);
                  if (!c) return <p className="hint">{t('状态未知')}</p>;
                  return (
                    <>
                      <div className="function-dependency__flow">
                        <div className="function-dependency__caller"><span>{t('Web 单元')}</span><code>{c.webCallIndexes.length} {t('个调用点')}</code></div>
                        <span aria-hidden="true" className="function-dependency__arrow">→</span>
                        <div className="function-dependency__method"><span>{selectedPlugin.goUnitId}</span><code>{c.goHandler ? `${c.goHandler.handler}()` : t('未读取')}</code></div>
                        <span aria-hidden="true" className="function-dependency__arrow">→</span>
                        <div className="function-dependency__caller"><span>{t('Go 实现入口')}</span><code>{c.goHandler ? `${c.goHandler.implFile}:${c.goHandler.implLine}` : t('未读取')}</code></div>
                      </div>
                      <div className="function-dependency__source">
                        <code>{linkedMethod}</code>
                        <span>{t('跨端方法映射')}</span>
                      </div>
                      <div className="function-dependency-list">
                        {c.webCallIndexes.map((i) => {
                          const w = graph.webCalls[i];
                          return (
                            <div key={i} className="function-dependency">
                              <div className="function-dependency__flow">
                                <div className="function-dependency__caller"><strong>{w.caller}</strong><code>{w.file}:{w.line}</code></div>
                                <span aria-hidden="true" className="function-dependency__arrow">→</span>
                                <div className="function-dependency__method"><span>{w.capability}</span><code>{w.method}()</code></div>
                                <span aria-hidden="true" className="function-dependency__arrow">→</span>
                                <div className="function-dependency__caller"><span>{w.kind}</span><code>{w.via || '—'}</code></div>
                              </div>
                            </div>
                          );
                        })}
                      </div>
                    </>
                  );
                })()}
              </div>
            )}
          </div>
          <div>
            <h3>{t('已声明依赖与通信')}</h3>
            <p>
              <strong>{t('跨端方法映射')}</strong>
              <span className="mono"> {`${selectedPlugin.pluginId}.web -> ${selectedPlugin.goUnitId}`} · {(selectedPlugin.methods || []).length}</span>
            </p>
            <p className="hint">{t('本端已声明依赖')}: {t('无')} · {t('零条声明依赖不等于零条源码调用')}</p>
          </div>
          <div>
            <h3>{t('数据归属与限制')}</h3>
            <p>{t(selectedPlugin.data)}</p>
          </div>
          <div>
            <h3>{t('函数调用关系')}</h3>
            <p className="pg-detail-note">
              {filteredCalls.length} / {callsForDirection.length} {t('个调用点')}
            </p>
            <div className="function-dependency-list">
              {filteredCalls.map((c, i) => (
                <div key={`${c.file}:${c.line}:${c.method}:${i}`} className="function-dependency">
                  <div className="function-dependency__flow">
                    <div className="function-dependency__caller"><strong>{c.caller}</strong><code>{c.side}</code></div>
                    <span aria-hidden="true" className="function-dependency__arrow">→</span>
                    <div className="function-dependency__method"><span>{c.capability || '—'}</span><code>{c.method || c.callee}</code></div>
                    <span aria-hidden="true" className="function-dependency__arrow">→</span>
                    <div className="function-dependency__caller"><strong>{c.providerUnit || c.side}</strong><code>{c.kind}{c.via ? ` · ${c.via}` : ''}{c.ownership ? ` · ${c.ownership}` : ''}</code></div>
                  </div>
                  <div className="function-dependency__source">
                    <code>{c.file}:{c.line}</code>
                    {c.side === 'web' && c.method && (
                      <button type="button" onClick={() => setLinkedMethod(linkedMethod === c.method ? '' : c.method)}>{t('关联跨端实现')}</button>
                    )}
                  </div>
                </div>
              ))}
              {!filteredCalls.length && <p role="status">{t('无匹配调用点')}</p>}
            </div>
            {direction === 'incoming' && filteredCalls.length === 0 && (
              <p className="hint">{t('无跨插件调用，不代表无业务关联')}</p>
            )}
          </div>
          <details>
            <summary>
              {t('未解析调用')} ({graph.unresolved.length})
            </summary>
            <p className="hint">{t('无法静态解析的动态调用，列出位置和原因；不猜目标、不静默丢弃。')}</p>
            <ul>
              {graph.unresolved.slice(0, 30).map((u, i) => (
                <li key={i} className="mono hint">
                  {u.side} · {u.caller}() · {u.file}:{u.line} · {u.reason}
                </li>
              ))}
            </ul>
          </details>
        </GraphModal>
      )}
    </div>
  );
}
