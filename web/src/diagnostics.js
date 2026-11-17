// 诊断只保留错误与结构，不接收业务请求、附件或授权对象。
// 双端页面防污染：每次注册绑定连接代际与诊断来源，注册返回注销函数，
// 只有当前持有者能清除或更新自身状态；旧连接的迟到结果不得覆盖新状态。
function redactText(text) {
  return text.replace(/(bearer\s+)[^\s"',;]+/gi, '$1[redacted]').replace(/((?:password|passwd|private.?key|secret|mnemonic|token|credential|authorizationId|bindingId|api.?key)["']?\s*[:=]\s*)(?:"[^"\n]*"|'[^'\n]*'|[^\s"',;&]+)/gi, '$1[redacted]').replace(/\b[0-9a-f]{64}\b/gi, '[redacted-256bit]').replace(/([a-z][a-z0-9+.-]*:\/\/)[^\s/@]+:[^\s/@]+@/gi, '$1[redacted]@').replace(/\b(?:[KL][1-9A-HJ-NP-Za-km-z]{51}|5[1-9A-HJ-NP-Za-km-z]{50})\b/g, '[redacted-wif]');
}

export function safeDiagnostic(value) {
  const seen = new WeakSet();
  const summarize = value => {
    if (typeof value === 'string') return redactText(value);
    if (!value || typeof value !== 'object') return value;
    if (seen.has(value)) return '[circular]'; seen.add(value);
    if (value instanceof Error) return { name: redactText(value.name), message: redactText(value.message), stack: redactText(value.stack || ''), cause: summarize(value.cause), errors: value instanceof AggregateError ? value.errors.map(summarize) : undefined };
    if (value instanceof ArrayBuffer || ArrayBuffer.isView(value)) return { byteLength: value.byteLength };
    if (Array.isArray(value)) return value.map(summarize);
    return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, /password|private.?key|secret|mnemonic|credential|token|bindingId|authorizationId|cookie|signature|rawtx/i.test(key) ? '[redacted]' : summarize(item)]));
  };
  try {
    const text = typeof value === 'string' ? redactText(value) : JSON.stringify(summarize(value));
    return text || '';
  } catch { return '诊断信息无法序列化'; }
}

let currentGeneration = 0;
let frontendRead = null;
let frontendOwner = '';
let frontendError = '';
const listeners = new Set();

// 后台读取结果：只保留白名单 DTO，不保留完整原始错误对象、堆栈、请求参数、
// 业务内容、附件、授权对象、实际数据根路径或带查询参数的连接 URL。
let backendState = { generation: 0, status: 'unread', data: null, error: null, fetchedAt: 0, stale: true };

export const diagnosticsChanged = () => { for (const listener of listeners) listener(); };

function ownerOf(opts) {
  return opts && typeof opts.source === 'string' ? opts.source : '';
}

// 每次注册绑定所属连接代际和诊断来源；返回注销函数。
export function registerDiagnostics(read, opts) {
  const generation = opts && typeof opts.generation === 'number' ? opts.generation : currentGeneration;
  const source = ownerOf(opts);
  // 新代际接管：清除旧 Go 快照，重新取得新后台实例的状态。
  if (generation !== currentGeneration) {
    currentGeneration = generation;
    backendState = { generation, status: 'unread', data: null, error: null, fetchedAt: 0, stale: true };
  } else if (backendState.generation !== generation) {
    backendState = { generation, status: 'unread', data: null, error: null, fetchedAt: 0, stale: true };
  }
  frontendRead = typeof read === 'function' ? read : null;
  frontendOwner = source;
  frontendError = '';
  diagnosticsChanged();
  let released = false;
  return () => {
    if (released) return;
    released = true;
    // 只有当前持有者能清除自身状态。
    if (currentGeneration === generation && frontendOwner === source) {
      frontendRead = null;
      frontendOwner = '';
      frontendError = '';
      diagnosticsChanged();
    }
  };
}

export function diagnosticFailure(error, opts) {
  const generation = opts && typeof opts.generation === 'number' ? opts.generation : currentGeneration;
  const source = ownerOf(opts);
  // 非持有者的迟到失败不得污染新连接显示。
  if (generation !== currentGeneration) return;
  if (source && frontendOwner && source !== frontendOwner) return;
  frontendError = safeDiagnostic(error);
  if (import.meta.env.DEV) console.debug('[plugins] startup.failed', frontendError);
  diagnosticsChanged();
}

// 后台读取结果提交前检查请求所属连接是否仍有效；旧响应不得覆盖新状态。
export function setBackendInspect(data, opts) {
  const generation = opts && typeof opts.generation === 'number' ? opts.generation : currentGeneration;
  if (generation !== currentGeneration) return false;
  const at = opts && typeof opts.fetchedAt === 'number' ? opts.fetchedAt : Date.now();
  const clean = data && typeof data === 'object' ? {
    instanceId: typeof data.instanceId === 'string' ? data.instanceId : '',
    runtimeInstanceId: typeof data.runtimeInstanceId === 'string' ? data.runtimeInstanceId : '',
    active: data.active === true,
    units: Array.isArray(data.units) ? data.units.map(u => ({
      pluginId: String(u.pluginId || ''),
      unitId: String(u.unitId || ''),
      instanceId: String(u.instanceId || ''),
    })) : [],
    catalog: Array.isArray(data.catalog) ? data.catalog.map(c => ({
      pluginId: String(c.pluginId || ''),
      unitId: String(c.unitId || ''),
      methods: Array.isArray(c.methods) ? c.methods.map(String) : [],
    })) : [],
  } : null;
  backendState = { generation, status: clean ? 'ready' : 'error', data: clean, error: clean ? null : { stage: 'backend.inspect', code: 'BAD_RESPONSE', message: '后台诊断响应非法' }, fetchedAt: at, stale: false };
  diagnosticsChanged();
  return true;
}

export function backendInspectFailure(structuredError, opts) {
  const generation = opts && typeof opts.generation === 'number' ? opts.generation : currentGeneration;
  if (generation !== currentGeneration) return false;
  const at = opts && typeof opts.fetchedAt === 'number' ? opts.fetchedAt : Date.now();
  const clean = structuredError && typeof structuredError === 'object' ? {
    stage: String(structuredError.stage || 'backend.inspect'),
    code: String(structuredError.code || 'UNKNOWN'),
    message: String(structuredError.message || '后台诊断失败'),
  } : { stage: 'backend.inspect', code: 'UNKNOWN', message: String(structuredError || '后台诊断失败') };
  backendState = { generation, status: 'error', data: null, error: clean, fetchedAt: at, stale: false };
  diagnosticsChanged();
  return true;
}

// 连接关闭后明确标记数据失效，不能继续显示为当前正常状态。
export function markDiagnosticsDisconnected(opts) {
  const generation = opts && typeof opts.generation === 'number' ? opts.generation : currentGeneration;
  if (generation !== currentGeneration) return;
  backendState = { ...backendState, stale: true };
  diagnosticsChanged();
}

export function diagnosticsSnapshot() {
  let front = { plugins: [] };
  let frontStatus = 'unread';
  let frontDetail = null;
  if (frontendRead) {
    try {
      front = frontendRead() || { plugins: [] };
      frontStatus = frontendError ? 'failed' : 'ready';
    } catch (error) {
      front = { plugins: [] };
      frontStatus = 'failed';
      frontDetail = safeDiagnostic(error);
    }
  } else if (frontendError) {
    frontStatus = 'failed';
  }
  const backend = { ...backendState };
  // 最近一次记录允许保留用于排查，但必须标明已失效及其后台实例。
  if (backend.status === 'ready' && backend.stale) backend.status = 'stale';
  return {
    generation: currentGeneration,
    frontend: { status: frontReadActive() ? frontStatus : 'closed', detail: frontDetail, owner: frontendOwner },
    backend,
    plugins: front.plugins || [],
    pluginCount: front.pluginCount || (front.plugins || []).length,
    error: frontendError || frontDetail || '',
  };
}

function frontReadActive() {
  return frontendRead !== null || frontendError !== '';
}

export function subscribeDiagnostics(listener) { listeners.add(listener); return () => listeners.delete(listener); }

// 显式白名单复制：只包含诊断格式版本、采集时间、连接状态及代际、
// 后台实例 ID、两端 Runtime ID、快照读取时间及是否失效、插件／单元／
// 实例身份、真实运行状态、已声明依赖、方法归属差异，以及结构化错误的
// 阶段、代码和脱敏消息。调用方在传入前已做白名单，前台再做额外脱敏。
export function buildCopyPayload(input) {
  const pick = input && typeof input === 'object' ? input : {};
  return {
    formatVersion: 'plugins.inspect/v1',
    collectedAt: new Date().toISOString(),
    connection: pick.connection || null,
    backendInstanceId: pick.backendInstanceId || '',
    webRuntime: pick.webRuntime || null,
    goRuntime: pick.goRuntime || null,
    snapshotAt: pick.snapshotAt || 0,
    stale: pick.stale === true,
    plugins: Array.isArray(pick.plugins) ? pick.plugins : [],
    dependencies: Array.isArray(pick.dependencies) ? pick.dependencies : [],
    methodDiff: Array.isArray(pick.methodDiff) ? pick.methodDiff : [],
    error: pick.error || null,
  };
}
