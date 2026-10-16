#!/usr/bin/env node
// Mutates only the disposable backend started by image-smoke.py. No model calls.
const assert = require('node:assert/strict');
const { chromium } = require('/opt/artex-tools/node_modules/playwright');
const base = new URL(process.env.BASE_URL);
const token = process.env.AUTH_TOKEN;
assert(token, 'temporary smoke token required');

async function main() {
  const browser = await chromium.launch({ args: ['--no-sandbox', '--disable-dev-shm-usage'] });
  const context = await browser.newContext({ viewport: { width: 1440, height: 1050 } });
  await context.addCookies([{ name: 'artex_token', value: token, url: base.origin }]);
  await context.addInitScript((t) => localStorage.setItem('artex_token', t), token);
  await context.route('**/*', async (route) => {
    const url = new URL(route.request().url());
    if (url.origin !== base.origin) return route.abort();
    if (url.pathname === '/api/update/check') return route.fulfill({ json: { current: 'smoke', has_update: false, mode: 'docker' } });
    return route.continue();
  });
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const name = 'Resource smoke ' + Date.now();
  const api = async (path, method = 'GET', data) => {
    const response = await context.request.fetch(new URL('/api' + path, base).href, {
      method, headers: { Authorization: 'Bearer ' + token }, ...(data === undefined ? {} : { data }),
    });
    assert(response.ok(), `${method} ${path}: ${response.status()}`);
    return response.json();
  };
  try {
    const anonymousContext = await browser.newContext();
    const anonymous = await anonymousContext.request.get(new URL('/api/resources', base).href);
    assert.equal(anonymous.status(), 401, 'resource API rejects anonymous access');
    await anonymousContext.close();
    await page.goto(new URL('/function/resources', base).href);
    await page.getByRole('heading', { name: '自有资源库', exact: true }).waitFor();
    await page.getByRole('button', { name: '登记资源' }).click();
    await page.getByLabel('名称', { exact: true }).fill(name);
    await page.getByLabel('用途与说明').fill('Synthetic text, file and endpoint fixture');
    await page.getByRole('button', { name: '保存', exact: true }).click();
    await page.getByRole('heading', { name, exact: true }).waitFor();
    await page.getByRole('button', { name: '基于最新版本建立草稿' }).click();
    const document = page.getByLabel('资源入口文档');
    await document.fill(`---\nname: ${name}\ndescription: Synthetic resource documentation\ntags: [smoke]\n---\n\n# Smoke documentation\n\nImmutable bundle fixture.\n`);
    await page.getByRole('button', { name: '保存说明', exact: true }).click();
    await page.waitForFunction(() => !document.querySelector('[aria-label="选择资源文件"]').matches(':disabled'));
    await page.getByLabel('选择资源文件').setInputFiles({ name: 'fixture.txt', mimeType: 'text/plain', buffer: Buffer.from('synthetic file only') });
    await page.getByText('fixture.txt', { exact: true }).waitFor();
    await page.getByRole('button', { name: '添加端点', exact: true }).click();
    await page.getByLabel('端点 1 名称').fill('fixture');
    await page.getByLabel('端点 1 地址').fill('https://example.invalid');
    await page.getByLabel('端点 1 凭据').fill('synthetic-secret-not-live');
    await page.getByRole('button', { name: '保存配置', exact: true }).click();
    await page.waitForFunction(() => !document.querySelector('[aria-label="选择资源文件"]').matches(':disabled'));
    await page.getByLabel('发布说明').fill('Synthetic version 1');
    await page.getByLabel('验证记录').fill('Browser round trip, no endpoint execution');
    await page.getByRole('button', { name: '发布新版本', exact: true }).click();
    await page.getByRole('tab', { name: '版本与任务' }).waitFor();
    await page.getByRole('button', { name: '下载 fixture.txt', exact: true }).waitFor();
    const [resource] = await api('/resources?q=' + encodeURIComponent(name));
    const [v1] = await api(`/resources/${resource.id}/versions`);
    assert.equal(v1.files.length, 2);
    assert(!JSON.stringify(v1).includes('synthetic-secret-not-live'), 'public manifest redacts credentials');
    await page.getByRole('tab', { name: '概览与文件' }).click();
    await page.getByText('Smoke documentation', { exact: true }).waitFor();
    const draft = await api(`/resources/${resource.id}/drafts`, 'POST', { base_version_id: v1.id });
    await api(`/resource-drafts/${draft.id}/publish`, 'POST', { revision: draft.revision, note: 'Synthetic version 2', validation: 'same fixture' });
    await page.getByText('资源已更新。当前草稿编辑内容保留；刷新后可查看新版本。').waitFor({ timeout: 25000 });
    await page.getByRole('button', { name: '查看更新', exact: true }).click();
    const [v2] = await api(`/resources/${resource.id}/versions`);
    assert(v2.id !== v1.id);
    const task = await api('/tasks', 'POST', { description: 'Synthetic resource selection', goal: 'No LLM configured; verify bindings only', resources: [{ resource_id: resource.id, version_id: v1.id }] });
    const bindings = await api(`/tasks/${task.id}/resources`);
    assert.equal(bindings[0].id, v1.id, 'task pins the explicitly selected old version');
    await page.getByRole('tab', { name: '版本与任务' }).click();
    await page.getByLabel('绑定到任务 / 显式更新任务版本').fill(String(task.id));
    await page.getByRole('button', { name: '绑定此版本', exact: true }).click();
    await page.getByText(`已为任务 #${task.id} 固定 v2`, { exact: true }).waitFor();
    assert.equal((await api(`/tasks/${task.id}/resources`))[0].id, v2.id, 'explicit owner switch uses current pin');
    await page.goto(new URL('/function/tasks', base).href);
    await page.getByRole('button', { name: '新建任务', exact: true }).click();
    await page.getByLabel('搜索可用资源').fill(name);
    await page.getByRole('checkbox', { name: new RegExp(name) }).click();
    await page.getByText(new RegExp('版本 ID ' + v2.id + '$')).waitFor();
    assert.deepEqual(errors, [], 'resource UI browser errors');
    console.log(JSON.stringify({ resources: { text_files_endpoints: true, encrypted_credentials: true, immutable_versions: true, update_notice: true, task_picker: true } }));
  } catch (error) {
    // This backend contains only synthetic fixtures, never production task data.
    console.error((await page.locator('body').innerText()).slice(0, 5000));
    throw error;
  } finally { await browser.close(); }
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
