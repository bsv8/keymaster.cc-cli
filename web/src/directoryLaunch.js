// 概览与应用目录复用入口选择、部署准备及工作区确认。
export function selectedDirectoryTarget(entry) {
  return entry.selected || (entry.url ? { url: entry.url } : entry.urls?.length ? { url: entry.urls[0].url } : { indexseedhashhex: entry.indexseedhashhex || entry.files?.[0]?.indexseedhashhex });
}
export async function prepareDirectoryEntry(entry, call) {
  const selected = selectedDirectoryTarget(entry);
  if (selected.url) return { url: selected.url, expectedAppPublicKeyHex: entry.appPublicKeyHex };
  if (!selected.indexseedhashhex) throw new Error('请选择有效入口');
  const list = await call('connect.deployment.list', {});
  let installed = list.deployments.find(d => d.indexseedhashhex === selected.indexseedhashhex);
  if (!installed) installed = await call('connect.deployment.install', { indexseedhashhex: selected.indexseedhashhex });
  if (entry.appPublicKeyHex && installed.appPublicKeyHex !== entry.appPublicKeyHex) throw new Error('部署公钥与目录目标不一致');
  const running = await call('connect.deployment.start', { deploymentId: installed.deploymentId });
  return { url: running.url, expectedAppPublicKeyHex: running.appPublicKeyHex, deploymentId: running.deploymentId };
}
export function directoryLaunchTarget(entry, prepare) {
  const selected = selectedDirectoryTarget(entry);
  return { title: entry.name || selected.url || entry.appPublicKeyHex || 'Files App', url: selected.url, description: selected.indexseedhashhex, prepare, testId: 'directory-iframe-view' };
}
