// 前后台共享业务归属目录。APIProtocol 负责传输；WebLoom 管理本端依赖和清理。
import { createWindowApp, defineCapability, definePlugin } from 'webloom-framework';
import { registerDiagnostics, diagnosticFailure, diagnosticsChanged } from './diagnostics.js';
import catalog from '../../pluginmanifest/catalog.json';

function validateCatalog(units) {
  const seenPlugin = new Map();
  const seenUnit = new Set();
  const seenMethod = new Map();
  for (const unit of units) {
    if (!unit || !unit.pluginId || !unit.unitId) throw new Error('插件目录存在空身份');
    if (seenPlugin.has(unit.pluginId)) throw new Error(`插件身份重复 ${unit.pluginId}`);
    seenPlugin.set(unit.pluginId, true);
    const webUnit = `${unit.pluginId}.web`;
    if (unit.unitId !== `${unit.pluginId}.daemon`) throw new Error(`Go 单元身份与约定不符 ${unit.unitId}`);
    if (seenUnit.has(unit.unitId) || seenUnit.has(webUnit)) throw new Error(`单元身份重复 ${unit.unitId}`);
    seenUnit.add(unit.unitId);
    seenUnit.add(webUnit);
    if (!Array.isArray(unit.methods) || unit.methods.length === 0) throw new Error(`插件 ${unit.pluginId} 缺少方法`);
    for (const method of unit.methods) {
      if (!method) throw new Error(`插件 ${unit.pluginId} 存在空方法`);
      if (seenMethod.has(method)) throw new Error(`方法归属重复 ${method}（${seenMethod.get(method)} 与 ${unit.pluginId}）`);
      seenMethod.set(method, unit.pluginId);
    }
    const doc = unit.documentation;
    if (!doc || !doc.name || !doc.summary || !doc.duties || !doc.boundary || !doc.data) {
      throw new Error(`插件 ${unit.pluginId} 的 documentation 引用缺失`);
    }
  }
  return { seenMethod };
}

const validated = validateCatalog(catalog);
const capabilities = new Map(catalog.map(unit => [unit.pluginId,
  defineCapability({ kind: 'local', id: `${unit.pluginId}.api`, version: 'v1' }),
]));
const owners = new Map(catalog.flatMap(unit => unit.methods.map(method => [method, unit.pluginId])));

// 调用者取消与本插件停止都终止等待；不宣称撤销已提交的业务副作用。
function callSignal(pluginSignal, callerSignal) {
  const controller = new AbortController();
  const signals = [pluginSignal, callerSignal].filter(Boolean);
  const abort = () => controller.abort();
  for (const signal of signals) {
    if (signal.aborted) controller.abort();
    else signal.addEventListener('abort', abort, { once: true });
  }
  return { signal: controller.signal, release() {
    for (const signal of signals) signal.removeEventListener('abort', abort);
  } };
}

/** 每条真实连接有独立前台 Runtime；销毁它不关闭 Go 后台。 */
export async function startWebPlugins(client, opts) {
  const generation = opts && typeof opts.generation === 'number' ? opts.generation : 0;
  const source = opts && typeof opts.source === 'string' ? opts.source : 'web.plugins';
  const diagOpts = { generation, source };
  const app = await createWindowApp({ id: 'km-web', plugins: catalog.map(unit => {
    const capability = capabilities.get(unit.pluginId);
    return definePlugin({ id: unit.pluginId, unitId: `${unit.pluginId}.web`, provides: [capability],
      setup(ctx) {
        async function call(options, method, params, attachments) {
          if (ctx.signal.aborted) throw new Error('前台插件已停止');
          if (!unit.methods.includes(method)) throw new Error(`方法不属于 ${unit.pluginId}`);
          const scope = callSignal(ctx.signal, options?.signal);
          try {
            const result = attachments === undefined
              ? await client.call({ signal: scope.signal }, method, params)
              : await client.callWithBinary({ signal: scope.signal }, method, params, attachments);
            if (scope.signal.aborted) throw new Error('调用已取消');
            return result;
          } finally { scope.release(); }
        }
        ctx.provide(capability, { call });
      },
    });
  }) }).catch(error => { diagnosticFailure(error, diagOpts); throw error; });
  const unregister = registerDiagnostics(() => app.inspect(), diagOpts);
  const offDiagnostics = app.subscribe(() => { const snapshot = app.inspect(); if (import.meta.env.DEV) { for (const plugin of snapshot.plugins || []) if (["failed", "blocked"].includes(plugin.kind)) diagnosticFailure(new Error(`${plugin.id}: ${plugin.error || plugin.blockedBy?.join(", ") || plugin.kind}`), diagOpts); } diagnosticsChanged(); });
  // 适配既有 SDK 便利方法。便利方法内部的 this.call 也经过插件能力。
  // 连接和订阅方法仍归 APIProtocol，不复制协议实现。
  const invoke = (options, method, params, attachments) => {
    const owner = owners.get(method);
    if (!owner) return Promise.reject(new Error(`没有登记业务插件的方法：${method}`));
    return app.capability(capabilities.get(owner)).call(options, method, params, attachments);
  };
  const facade = new Proxy(client, { get(target, property) {
    if (property === 'call') return (options, method, params) => invoke(options, method, params);
    if (property === 'callWithBinary') return (options, method, params, attachments = {}) => invoke(options, method, params, attachments);
    const value = Reflect.get(target, property);
    if (typeof value !== 'function') return value;
    // 只让业务便利方法使用 facade，底层连接方法保留真实 receiver。
    // 诊断固定只读调用由连接层经真实客户端发起，不经 facade，避免前台装配失败阻塞后台诊断。
    const lifecycle = ['subscribeEvents', 'getInstanceId', 'getConnectionId', 'getStatus', 'onConnectionChange', 'close'];
    return (...args) => Reflect.apply(value, lifecycle.includes(property) ? target : facade, args);
  } });
  let disposed = false;
  return { client: facade, dispose: () => {
    if (disposed) return Promise.resolve();
    disposed = true;
    try { unregister(); } catch { /* ignore */ }
    offDiagnostics();
    return app.dispose('APIProtocol connection closed');
  }, inspect: () => app.inspect() };
}

export function catalogMethodOwners() {
  return new Map(validated.seenMethod);
}
