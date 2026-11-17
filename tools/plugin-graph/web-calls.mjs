// Web 静态分析：解析 web/src 实际 JS/JSX 调用点，保留真实调用者位置。
// 覆盖直接 call、二进制调用、SDK 便利方法及 facade 分派；动态方法列为未解析，不猜目标。
// Program AST 无 parent 指针时用栈跟踪 enclosing 函数，不把所有记录落到通用 invoke()。
import ts from 'typescript';
import { readdirSync } from 'node:fs';
import { relative, join, sep } from 'node:path';

const CONVENIENCE = {
  status: 'status.get',
  info: 'info.get',
  unlock: 'wallet.unlock',
  walletCreate: 'wallet.create',
  walletImport: 'wallet.import',
  listRoots: 'roots.list',
  addRoot: 'roots.add',
  removeRoot: 'roots.remove',
  setDefaultRoot: 'roots.setDefault',
  listFiles: 'files.list',
  browse: 'files.browse',
  getSettings: 'settings.get',
  updateSettings: 'settings.update',
  resetSettings: 'settings.reset',
  refreshCatalog: 'catalog.refresh',
  collectorStatus: 'collector.status',
  collectorHistory: 'collector.history',
  collectorDescribe: 'collector.describe',
  collectorFlush: 'collector.flush',
  collectorRetry: 'collector.retry',
  listTasks: 'tasks.list',
  createTask: 'tasks.create',
  cancelTask: 'tasks.cancel',
  approvalsList: 'connect.approvals.list',
  approvalsDecide: 'connect.approvals.decide',
  bindingsList: 'connect.bindings.list',
  authList: 'connect.auth.list',
  authRevoke: 'connect.auth.revoke',
  permissionsList: 'connect.permissions.list',
  permissionsRevoke: 'connect.permissions.revoke',
  historyList: 'connect.history.list',
  appsList: 'connect.apps.list',
  connectionPause: 'connect.connection.pause',
  connectExec: 'connect.request.exec',
};

const LIFECYCLE = new Set(['subscribeEvents', 'getInstanceId', 'getConnectionId', 'getStatus', 'onConnectionChange', 'close', 'getClient', 'getConnectionStatus', 'getConnectionGeneration', 'refreshBackendInspect', 'retryConnection', 'onConnectionStatus']);

function sources(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    if (e.name === 'node_modules' || e.name === 'dist' || e.name === 'generated') return [];
    const p = join(dir, e.name);
    if (e.isDirectory()) return sources(p);
    return /\.[cm]?jsx?$/.test(p) && !/\.test\./.test(p) ? [p] : [];
  });
}

function repoPath(root, abs) {
  return relative(root, abs).split(sep).join('/');
}

function methodArg(args, index) {
  const n = args[index];
  if (!n) return { kind: 'missing' };
  if (ts.isStringLiteralLike(n)) return { kind: 'literal', value: n.text };
  return { kind: 'dynamic' };
}

export function analyzeWebCalls(cliRoot, methodToPlugin, goUnitOf) {
  const srcDir = join(cliRoot, 'web/src');
  const paths = sources(srcDir).sort();
  const program = ts.createProgram(paths, {
    allowJs: true,
    jsx: ts.JsxEmit.React,
    target: ts.ScriptTarget.ES2020,
    moduleResolution: ts.ModuleResolutionKind.Bundler,
  });
  const webCalls = [];
  const unresolved = [];
  for (const file of paths) {
    if (file.endsWith('/i18n.js')) continue; // 纯文案，无业务调用
    const source = program.getSourceFile(file);
    if (!source) {
      unresolved.push({ side: 'web', file: repoPath(cliRoot, file), line: 1, caller: 'module', reason: 'unparsed-file' });
      continue;
    }
    const visit = (node, stack) => {
      let pushed = null;
      if (ts.isFunctionDeclaration(node) && node.name) {
        pushed = node.name.text;
      } else if (ts.isVariableDeclaration(node) && ts.isIdentifier(node.name) && node.initializer) {
        const init = node.initializer;
        if (ts.isFunctionLike(init)) pushed = node.name.text;
        else if (ts.isCallExpression(init) && init.arguments.some((a) => ts.isFunctionLike(a))) pushed = node.name.text;
      } else if (ts.isFunctionLike(node)) {
        pushed = node.name ? node.name.getText(source) : 'anonymous';
      }
      if (pushed) stack.push(pushed);
      const caller = [...stack].reverse().find((s) => s !== 'anonymous') ?? 'module';
      if (ts.isCallExpression(node) && ts.isPropertyAccessExpression(node.expression)) {
        const prop = node.expression.name.text;
        const args = node.arguments ?? [];
        const fileRel = repoPath(cliRoot, file);
        const line = source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1;
        if (prop === 'call' || prop === 'callWithBinary') {
          const m = methodArg(args, 1);
          if (m.kind === 'literal') {
            const pluginId = methodToPlugin.get(m.value);
            if (pluginId) {
              webCalls.push({
                method: m.value,
                pluginId,
                capability: `${pluginId}.api`,
                providerUnit: goUnitOf.get(pluginId),
                runtime: 'web',
                caller,
                file: fileRel,
                line,
                kind: prop === 'callWithBinary' ? 'binary' : 'direct',
              });
            }
          } else {
            unresolved.push({
              side: 'web',
              file: fileRel,
              line,
              caller,
              reason: fileRel.endsWith('plugins.js') ? 'facade-dispatch-dynamic-method' : 'dynamic-method-argument',
            });
          }
        } else if (Object.prototype.hasOwnProperty.call(CONVENIENCE, prop) && !LIFECYCLE.has(prop)) {
          const method = CONVENIENCE[prop];
          const pluginId = methodToPlugin.get(method);
          if (pluginId) {
            webCalls.push({
              method,
              pluginId,
              capability: `${pluginId}.api`,
              providerUnit: goUnitOf.get(pluginId),
              runtime: 'web',
              caller,
              file: fileRel,
              line,
              kind: 'convenience',
              via: prop,
            });
          }
        }
      }
      ts.forEachChild(node, (c) => visit(c, stack));
      if (pushed) stack.pop();
    };
    visit(source, []);
  }
  webCalls.sort((a, b) =>
    a.method.localeCompare(b.method) || a.file.localeCompare(b.file) || a.line - b.line || a.caller.localeCompare(b.caller));
  unresolved.sort((a, b) => a.file.localeCompare(b.file) || a.line - b.line || a.caller.localeCompare(b.caller));
  return { webCalls, unresolved };
}
