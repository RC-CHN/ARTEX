#!/usr/bin/env node
// Real rendered UI and auth; runner/settings mutations use isolated fixtures.
const assert = require('node:assert/strict');
const { chromium } = require('/opt/artex-tools/node_modules/playwright');
const base = new URL(process.env.BASE_URL);
const image = 'registry.example.org/artex@sha256:' + 'a'.repeat(64);
const config = {
  enabled: true, default_mode: 'local', agent_modes: { worker: 'remote' },
  default_environment: 'default', acquire_timeout_seconds: 120,
  environments: { default: { image, direct_network: true,
    resources: { cpu_millis: 1000, memory_mib: 1024, pids: 256, gpu_count: 0 },
    labels: { trust: 'private' }, env: { CUSTOM_SETTING: 'preserved' }, warm_idle: 0 } },
  mcp: { internal: { location: 'controller' } },
};
function node(id, name, executor) {
  return { id, name, online: true, enabled: true, last_seen: new Date().toISOString(),
    active_sessions: 1, concurrency_limit: 0, effective_slots: 4,
    cpu_limit_millis: 0, memory_limit_mib: 0, effective_cpu_millis: 4000, effective_memory_mib: 8192,
    reserved_cpu_millis: 1000, reserved_memory_mib: 1024, environment_prompt: '',
    ...(executor === 'kubernetes' ? { pool_status: { idle: 1, preparing: 1, pods: 3, cpu_millis: 3000, memory_mib: 3072 } } : {}),
    capabilities: { executor, slots: 4, cpu_millis: 4000, memory_mib: 8192, environments: ['default'],
      labels: { network: executor === 'kubernetes' ? 'cluster' : 'external' },
      gpus: executor === 'docker' ? [{ id: '0', model: 'Example GPU', memory_mib: 24576 }] : [] } };
}
const state = { config, nodes: [node('pool', 'kubernetes-pool', 'kubernetes'), node('gpu', 'external-gpu', 'docker')], experimental: true };
const settings = { environment_prompt: '', workers: 5 };
const agents = ['mainagent', 'planner', 'worker', 'goals'].map((key) => ({ id: key, key, name: key, builtin: true, enabled: true, llm_profile_id: null }));
const profiles = [
  { id: '11', name: 'High capability', model: 'strong-test-model', format: 'openai', is_default: true },
  { id: '12', name: 'Low cost', model: 'cheap-test-model', format: 'openai', is_default: false },
];
const writes = [];
let failLimits = true;
let failConfig = true;
let failPrompt = true;
let failWorkerModel = true;
let reads = 0;

async function main() {
  const browser = await chromium.launch({ args: ['--no-sandbox', '--disable-dev-shm-usage'] });
  const context = await browser.newContext({ viewport: { width: 1440, height: 1100 } });
  await context.addCookies([{ name: 'artex_token', value: process.env.AUTH_TOKEN, url: base.origin }]);
  await context.addInitScript((token) => {
    localStorage.setItem('artex_token', token);
    // Exercise the HTTP clipboard fallback and inspect the generated config.
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
    document.execCommand = (command) => {
      if (command !== 'copy') return false;
      window.lastCopied = document.activeElement?.value;
      return true;
    };
  }, process.env.AUTH_TOKEN);
  await context.route('**/*', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.origin !== base.origin) return route.abort();
    if (url.pathname === '/api/update/check') return route.fulfill({ json: { current: 'smoke', has_update: false, mode: 'docker' } });
    const path = url.pathname.replace(/^\/api/, '');
    if (path === '/runners' && request.method() === 'GET') {
      reads++;
      return route.fulfill({ json: state });
    }
    if (path === '/settings' && request.method() === 'GET') return route.fulfill({ json: settings });
    if (path === '/agents' && request.method() === 'GET') return route.fulfill({ json: { agents } });
    if (path === '/llm/profiles' && request.method() === 'GET') return route.fulfill({ json: { profiles } });
    if (request.method() === 'PUT') {
      const body = request.postDataJSON();
      writes.push({ path, body });
      const role = path.match(/^\/agents\/([^/]+)\/config$/)?.[1];
      if (role) {
        assert.deepEqual(Object.keys(body), ['llm_profile_id']);
        if (role === 'worker' && failWorkerModel) {
          failWorkerModel = false;
          return route.fulfill({ status: 503, json: { error: '角色模型保存测试失败' } });
        }
        Object.assign(agents.find((agent) => agent.key === role), body);
        return route.fulfill({ json: { ok: true } });
      }
      if (path === '/runners/pool/limits') {
        if (failLimits) { failLimits = false; return route.fulfill({ status: 503, json: { error: '配额保存测试失败' } }); }
        const n = state.nodes[0];
        Object.assign(n, { concurrency_limit: body.slots, cpu_limit_millis: body.cpu_millis, memory_limit_mib: body.memory_mib,
          effective_slots: body.slots || 4, effective_cpu_millis: body.cpu_millis || 4000, effective_memory_mib: body.memory_mib || 8192 });
        return route.fulfill({ json: n });
      }
      if (path === '/runners/pool/environment-prompt') {
        Object.assign(state.nodes[0], body);
        return route.fulfill({ json: body });
      }
      if (path === '/runners/gpu/enabled') {
        Object.assign(state.nodes[1], body);
        return route.fulfill({ json: { ok: true } });
      }
      if (path === '/runners/config') {
        if (failConfig) { failConfig = false; return route.fulfill({ status: 503, json: { error: '执行配置保存测试失败' } }); }
        state.config = body;
        return route.fulfill({ json: body });
      }
      if (path === '/settings') {
        assert.deepEqual(Object.keys(body), ['environment_prompt']);
        if (failPrompt) { failPrompt = false; return route.fulfill({ status: 503, json: { error: '提示词保存测试失败' } }); }
        Object.assign(settings, body);
        return route.fulfill({ json: settings });
      }
      throw new Error('unexpected mutation: ' + path);
    }
    if (path === '/runners/registration-token' && request.method() === 'POST') {
      return route.fulfill({ json: { token: 'one-time-smoke-token', expires_at: new Date(Date.now() + 600000).toISOString() } });
    }
    if (!['GET', 'HEAD'].includes(request.method())) throw new Error('unexpected method: ' + path);
    return route.continue();
  });
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  page.setDefaultTimeout(15000);
  try {
    await page.goto(new URL('/system/runners', base).href);
    await page.getByRole('heading', { name: '集群动态执行池' }).waitFor();
    await page.getByRole('heading', { name: '独立注册节点' }).waitFor();
    await page.getByTestId('runner-gpu').getByText(/Example GPU/).waitFor();
    if (process.env.SCREENSHOT_DIR) await page.screenshot({ path: process.env.SCREENSHOT_DIR + '/runners-desktop.png', fullPage: true });

    await page.getByTestId('runner-pool').getByRole('button', { name: '管理', exact: true }).click();
    const dialog = page.getByRole('dialog');
    await dialog.getByLabel('并发上限').fill('2');
    await dialog.getByLabel('CPU 总额度（核）').fill('2');
    await dialog.getByLabel('内存总额度（MiB）').fill('4096');
    await dialog.getByRole('button', { name: '保存配额', exact: true }).click();
    await dialog.getByRole('alert').filter({ hasText: '配额保存测试失败' }).waitFor();
    assert.equal(await dialog.getByLabel('并发上限').inputValue(), '2');
    await dialog.getByRole('button', { name: '保存配额', exact: true }).click();
    await dialog.getByRole('status').filter({ hasText: '配额已保存' }).waitFor();
    assert.deepEqual(writes.find((write) => write.path.endsWith('/limits')).body, { slots: 2, cpu_millis: 2000, memory_mib: 4096 });
    await dialog.getByLabel('此 Runner 的额外环境提示词').fill('Only this pool can reach the optional proxy.');
    await page.keyboard.press('Escape');
    await page.getByRole('alertdialog').getByRole('button', { name: '继续编辑' }).click();
    assert.equal(await dialog.getByLabel('此 Runner 的额外环境提示词').inputValue(), 'Only this pool can reach the optional proxy.');
    await dialog.getByRole('button', { name: '保存节点说明' }).click();
    await dialog.getByRole('status').filter({ hasText: '节点环境说明已保存' }).waitFor();
    await page.keyboard.press('Escape');
    await dialog.waitFor({ state: 'hidden' });
    assert.equal(state.nodes[1].environment_prompt, '');

    await page.getByTestId('runner-gpu').getByRole('button', { name: '停用', exact: true }).click();
    await page.getByRole('alertdialog').getByText(/当前 1 个已预留执行会停止续租并终止/).waitFor();
    await page.getByRole('alertdialog').getByRole('button', { name: '取消', exact: true }).click();
    assert(!writes.some((write) => write.path.endsWith('/enabled')));
    await page.getByTestId('runner-gpu').getByRole('button', { name: '停用', exact: true }).click();
    await page.getByRole('alertdialog').getByRole('button', { name: '确认停用' }).click();
    await page.getByTestId('runner-gpu').getByText('已停用', { exact: true }).waitFor();

    await page.getByRole('tab', { name: '执行设置' }).click();
    await page.getByLabel('单次执行 CPU（核）').fill('1.5');
    await page.getByLabel('此执行环境的补充说明').fill('Environment-specific optional services.');
    const beforeRefresh = reads;
    const refreshed = page.waitForResponse((response) => new URL(response.url()).pathname === '/api/runners');
    await page.getByRole('button', { name: '刷新', exact: true }).click();
    await refreshed;
    await page.getByText('Runner 状态已刷新', { exact: true }).last().waitFor();
    assert(reads > beforeRefresh);
    assert.equal(await page.getByLabel('单次执行 CPU（核）').inputValue(), '1.5', 'refresh preserved dirty form');
    await page.getByRole('tab', { name: '资源概览' }).click();
    await page.getByRole('tab', { name: '执行设置' }).click();
    assert.equal(await page.getByLabel('单次执行 CPU（核）').inputValue(), '1.5', 'tab switch preserved dirty form');
    await page.getByLabel('Kubernetes 预热 Pod 上限').fill('2');
    await page.getByRole('button', { name: '保存执行配置' }).click();
    await page.getByRole('alert').filter({ hasText: '执行配置保存测试失败' }).waitFor();
    await page.getByRole('button', { name: '保存执行配置' }).click();
    await page.getByRole('status').filter({ hasText: '配置已保存，对后续执行生效' }).waitFor();
    assert.equal(state.config.environments.default.resources.cpu_millis, 1500);
    assert.equal(state.config.environments.default.warm_idle, 2);
    assert.equal(state.config.environments.default.env.CUSTOM_SETTING, 'preserved');
    assert.equal(state.config.mcp.internal.location, 'controller');
    await page.getByText('高级设置 · GPU、标签、MCP 与完整配置', { exact: true }).click();
    const raw = page.getByLabel('完整环境配置 · JSON');
    const original = await raw.inputValue();
    await raw.fill('{broken');
    const beforeInvalid = writes.length;
    await page.getByRole('button', { name: '保存执行配置' }).click();
    await page.getByRole('alert').filter({ hasText: '环境配置的 JSON 格式不正确' }).waitFor();
    assert.equal(writes.length, beforeInvalid);
    await raw.fill(original);
    await page.getByRole('button', { name: '保存执行配置' }).click();
    await page.getByRole('status').filter({ hasText: '配置已保存，对后续执行生效' }).waitFor();

    await page.getByRole('tab', { name: '全局环境说明' }).click();
    await page.getByRole('textbox', { name: '全局环境说明', exact: true }).fill('Use direct networking by default; proxy is optional.');
    await page.getByRole('button', { name: '保存环境提示词' }).click();
    await page.getByRole('alert').filter({ hasText: '提示词保存测试失败' }).waitFor();
    await page.getByRole('button', { name: '保存环境提示词' }).click();
    await page.getByRole('status').filter({ hasText: '已保存，新启动的 Agent' }).waitFor();
    assert.equal(settings.environment_prompt, 'Use direct networking by default; proxy is optional.');

    await page.getByRole('button', { name: '接入 Runner', exact: true }).click();
    await dialog.getByLabel('明确允许可信内网中的 HTTP 连接').click();
    await dialog.getByRole('button', { name: '复制 Compose 配置' }).click();
    const compose = JSON.parse(await page.evaluate(() => window.lastCopied));
    assert.equal(compose.services.runner.image, image);
    assert(compose.services.runner.volumes.includes('/var/run/docker.sock:/var/run/docker.sock'));
    assert(!JSON.stringify(compose).includes('one-time-smoke-token'));
    await dialog.getByRole('button', { name: '生成注册令牌', exact: true }).click();
    await dialog.getByLabel('注册令牌', { exact: true }).waitFor();
    assert.equal(await dialog.getByLabel('注册令牌', { exact: true }).getAttribute('type'), 'password');
    await dialog.getByRole('button', { name: /K3s \/ Kubernetes 执行池/ }).click();
    await dialog.getByRole('button', { name: '复制节点配置' }).click();
    const poolConfig = JSON.parse(await page.evaluate(() => window.lastCopied));
    assert.equal(poolConfig.executor, 'kubernetes');
    assert.equal(poolConfig.kubernetes.workspace_pvc, 'artex-runner-workspace');
    await page.keyboard.press('Escape');
    await page.getByRole('button', { name: '接入 Runner', exact: true }).click();
    assert.equal(await dialog.getByLabel('注册令牌', { exact: true }).count(), 0, 'token not retained after close');
    await page.keyboard.press('Escape');
    await page.getByRole('tab', { name: '资源概览' }).click();
    await page.setViewportSize({ width: 390, height: 844 });
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1), 'mobile horizontal overflow');
    if (process.env.SCREENSHOT_DIR) await page.screenshot({ path: process.env.SCREENSHOT_DIR + '/runners-mobile.png', fullPage: true });
    await page.setViewportSize({ width: 1440, height: 1100 });
    await page.goto(new URL('/system/llm', base).href);
    await page.getByText('按角色选择模型', { exact: true }).waitFor();
    for (const [title, model] of [['主 Agent', 'strong-test-model'], ['Planner 规划', 'strong-test-model'], ['Worker 执行', 'cheap-test-model']]) {
      const section = page.getByRole('region', { name: `${title} 模型配置`, exact: true });
      await section.getByLabel(`${title} 使用的模型`).click();
      await page.getByRole('option').filter({ hasText: model }).click();
      await section.getByRole('button', { name: `保存${title}模型`, exact: true }).click();
      if (title === 'Worker 执行') {
        await section.getByRole('alert').filter({ hasText: '角色模型保存测试失败' }).waitFor();
        await section.getByRole('button', { name: `保存${title}模型`, exact: true }).click();
      }
      await section.getByRole('status').waitFor();
    }
    assert.equal(agents.find((agent) => agent.key === 'planner').llm_profile_id, 11);
    assert.equal(agents.find((agent) => agent.key === 'worker').llm_profile_id, 12);
    assert.equal(agents.find((agent) => agent.key === 'goals').llm_profile_id, null);
    if (process.env.SCREENSHOT_DIR) await page.screenshot({ path: process.env.SCREENSHOT_DIR + '/role-models.png', fullPage: true });
    assert.deepEqual(errors, []);
    console.log('Runner UI: mixed pools/nodes, quotas, save failures/retry, dirty refresh/tabs, prompts, disable confirmation, enrollment, HTTP clipboard, mobile layout and independent role models: OK');
  } catch (error) {
    console.error(JSON.stringify({ errors, text: await page.locator('body').innerText() }).replaceAll(process.env.AUTH_TOKEN, '[redacted]'));
    throw error;
  } finally {
    await context.close();
    await browser.close();
  }
}
main().catch((error) => { console.error(error); process.exit(1); });
