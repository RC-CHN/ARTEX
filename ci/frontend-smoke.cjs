#!/usr/bin/env node
// Run inside the application image against a disposable, initialized backend:
// BASE_URL=http://smoke-app:8787 AUTH_TOKEN=<temporary-token> AUTH_PASSWORD=<temporary-password> node /smoke/frontend-smoke.cjs
// Only the login POST is sent; no LLM calls or stored-data mutations are made.
// Browser traffic is restricted to BASE_URL. The image supplies Playwright.
const assert = require('node:assert/strict');
const { chromium } = require('/opt/artex-tools/node_modules/playwright');

const base = new URL(process.env.BASE_URL || 'http://127.0.0.1:8787');
const token = process.env.AUTH_TOKEN;
const password = process.env.AUTH_PASSWORD;
assert(token, 'AUTH_TOKEN must contain the temporary smoke-test token');
assert(password, 'AUTH_PASSWORD must contain the temporary smoke-test password');
assert(['http:', 'https:'].includes(base.protocol), 'BASE_URL must use HTTP(S)');

const cases = [
  { path: '/dashboard', role: 'heading', name: '总览', endpoint: '/api/tasks' },
  { path: '/function/tasks', role: 'tab', name: '当前任务', endpoint: '/api/tasks' },
  { path: '/function/assets', role: 'heading', name: '资产', endpoint: '/api/assets/counts' },
  { path: '/system/llm', role: 'heading', name: 'LLM', endpoint: '/api/llm/profiles' },
];
const pageRoutes = new Set([...cases.map((scenario) => scenario.path), '/function/findings']);

function stubReleaseCheck(route) {
  // Release lookup reaches GitHub from the backend and can take 20 seconds.
  // Keep this UI/cache smoke deterministic and independent of external releases.
  return route.fulfill({ json: { current: 'smoke', has_update: false, mode: 'docker' } });
}

async function diagnose(page, errors) {
  const state = await page.evaluate(() => ({
    path: location.pathname,
    text: document.body.innerText.slice(0, 4000),
    tabs: [...document.querySelectorAll('[role="tab"]')].map((node) => ({ text: node.textContent, hidden: node.hidden })),
  })).catch(() => ({ path: 'unavailable' }));
  console.error(JSON.stringify({ frontend_failure: state, errors }).split(token).join('[redacted]').split(password).join('[redacted]'));
}

async function verifyAuthForms(browser) {
  const reports = [];
  for (const setup of [false, true]) {
    const context = await browser.newContext();
    const errors = [];
    await context.route('**/*', async (route) => {
      const request = route.request();
      const url = new URL(request.url());
      if (url.origin !== base.origin) {
        errors.push('unexpected external browser request');
        return route.abort();
      }
      if (url.pathname === '/api/update/check') return stubReleaseCheck(route);
      // The image smoke has already initialized this disposable backend. Expose
      // only its pre-initialization status to exercise the setup form safely.
      if (setup && url.pathname === '/api/auth/status') {
        return route.fulfill({ json: { initialized: false } });
      }
      if (request.method() !== 'GET' && url.pathname !== '/api/auth/login') {
        errors.push(`unexpected auth-form mutation: ${url.pathname}`);
        return route.abort();
      }
      return route.continue();
    });
    const page = await context.newPage();
    page.setDefaultTimeout(15_000);
    page.on('pageerror', (error) => errors.push(error.message));
    page.on('console', (message) => {
      if (message.type() === 'error' || /hydration|hydrating|did not match/i.test(message.text())) errors.push(message.text());
    });
    try {
      await page.goto(new URL(setup ? '/setup' : '/login', base).href, { waitUntil: 'domcontentloaded' });
      await page.getByRole('heading', { name: setup ? '初始化密码' : '登录', exact: true }).waitFor({ state: 'visible' });
      assert.equal(await page.getByRole('checkbox').count(), 0, 'authentication has no agreement checkbox');
      assert.equal(await page.getByText(/使用须知|免责声明|我已阅读并同意/).count(), 0, 'authentication has no agreement dialog or gate');
      await page.getByLabel(setup ? '新密码' : '密码', { exact: true }).fill(password);
      if (setup) await page.getByLabel('确认密码', { exact: true }).fill(password);
      const submit = page.getByRole('button', { name: setup ? '设置密码并登录' : '登录', exact: true });
      assert(await submit.isEnabled(), 'valid credentials enable submit without agreement');
      if (!setup) {
        await submit.click();
        await page.waitForURL(/\/function\/tasks\/?$/);
        await page.getByRole('tab', { name: '当前任务', exact: true }).waitFor({ state: 'visible' });
        assert(await page.evaluate(() => Boolean(localStorage.getItem('artex_token'))), 'login stores a real backend token');
      }
      assert.deepEqual(errors, [], 'auth form browser errors');
      reports.push({ path: setup ? '/setup' : '/login', agreement_gate: false, real_login: !setup });
    } catch (error) {
      await diagnose(page, errors);
      throw error;
    } finally {
      await context.close();
    }
  }
  return reports;
}

async function main() {
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'] });
  const reports = [];
  try {
    reports.push(...await verifyAuthForms(browser));
    for (const scenario of cases) {
      // A fresh context gives each page an empty GET cache and a deterministic
      // first read, while still mounting the real shared sidebar consumers.
      const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
      await context.addCookies([{ name: 'artex_token', value: token, url: base.origin }]);
      await context.addInitScript((value) => localStorage.setItem('artex_token', value), token);
      const errors = [];
      const counts = new Map();
      const pending = new Set();
      let cancelledPrefetches = 0;
      let lastRequest = 0;
      await context.route('**/*', async (route) => {
        const url = new URL(route.request().url());
        if (url.origin !== base.origin) {
          errors.push('unexpected external browser request');
          await route.abort();
          return;
        }
        if (url.pathname === '/api/update/check') return stubReleaseCheck(route);
        await route.continue();
      });
      const page = await context.newPage();
      page.setDefaultTimeout(15_000);
      page.on('pageerror', (error) => errors.push(error.message));
      page.on('console', (message) => {
        if (message.type() === 'error' || /hydration|hydrating|did not match/i.test(message.text())) {
          errors.push(message.text());
        }
      });
      page.on('request', (request) => {
        const url = new URL(request.url());
        if (!url.pathname.startsWith('/api/')) return;
        if (request.method() !== 'GET') errors.push(`unexpected API mutation: ${request.method()} ${url.pathname}`);
        const key = url.pathname + url.search;
        counts.set(key, (counts.get(key) || 0) + 1);
        pending.add(request);
        lastRequest = Date.now();
      });
      page.on('requestfinished', (request) => pending.delete(request));
      page.on('requestfailed', (request) => {
        pending.delete(request);
        const url = new URL(request.url());
        const failure = request.failure()?.errorText;
        // Next can cancel speculative route prefetches when a render updates
        // its links. API reads, navigation, and real transport failures still fail.
        const chain = [];
        for (let item = request; item; item = item.redirectedFrom()) {
          const target = new URL(item.url());
          chain.push({ path: target.pathname, query: target.search, accept: item.headers().accept, prefetch: item.headers()['next-router-prefetch'] });
        }
        const prefetch = chain.some((item) => item.prefetch === '1' || new URLSearchParams(item.query).has('_rsc') || item.accept === 'text/x-component');
        // Next's static-export prefetch first probes redirects with a plain HEAD
        // fetch (segment-cache/cache), with no RSC marker. Limit this exception
        // to those probes of known UI routes, never scripts or ordinary GETs.
        const staticPageFetch = request.resourceType() === 'fetch' && request.method() === 'HEAD' && pageRoutes.has(url.pathname.replace(/\/$/, ''));
        if (failure === 'net::ERR_ABORTED' && !url.pathname.startsWith('/api/') && !request.isNavigationRequest() && (prefetch || staticPageFetch)) {
          cancelledPrefetches++;
          return;
        }
        errors.push(`request failed: ${request.method()} ${url.pathname} (${failure ?? 'unknown error'}; type=${request.resourceType()}; navigation=${request.isNavigationRequest()}; redirects=${JSON.stringify(chain)})`);
      });
      page.on('response', (response) => {
        if (response.status() >= 400) errors.push(`HTTP ${response.status()}: ${new URL(response.url()).pathname}`);
      });
      try {
        const initialData = page.waitForResponse((response) =>
          new URL(response.url()).pathname === scenario.endpoint && response.status() === 200);
        const document = await page.goto(new URL(scenario.path, base).href, { waitUntil: 'domcontentloaded' });
        assert.equal(document?.status(), 200, `${scenario.path}: static page returned 200`);
        await page.getByRole(scenario.role, { name: scenario.name, exact: true }).waitFor({ state: 'visible' });
        await initialData;
        const settleDeadline = Date.now() + 10_000;
        while (pending.size || Date.now() - lastRequest < 400) {
          assert(Date.now() < settleDeadline, `${scenario.path}: initial API requests did not settle`);
          await page.waitForTimeout(100);
        }
        assert.equal(new URL(page.url()).pathname.replace(/\/$/, ''), scenario.path, 'authenticated route remained selected');
        assert.equal(counts.get(scenario.endpoint), 1, `${scenario.path}: initial endpoint was deduplicated`);
        if (scenario.path === '/dashboard') {
          assert.equal(counts.get('/api/intercept/pending'), 1, 'dashboard and sidebar share one pending-approval request');
        }
        const initialCounts = Object.fromEntries(counts);
        // Cross the former five-second polling boundary, remaining below the
        // shortest intentional idle fallback (the sidebar's 15 seconds).
        await page.waitForTimeout(6_200);
        assert.deepEqual(Object.fromEntries(counts), initialCounts, `${scenario.path}: no old 5s network polling`);
        for (const [path, count] of counts) assert.equal(count, 1, `${scenario.path}: repeated initial GET ${path}`);

        if (scenario.path === '/function/assets') {
          const refreshed = page.waitForResponse((response) =>
            new URL(response.url()).pathname === scenario.endpoint && response.status() === 200);
          await page.getByRole('button', { name: '刷新', exact: true }).click();
          await refreshed;
          assert.equal(counts.get(scenario.endpoint), 2, 'explicit refresh invalidates the shared cache');
        }
        assert.deepEqual(errors, [], `${scenario.path}: browser errors`);
        reports.push({ path: scenario.path, initial_gets: Object.keys(initialCounts).length, deduplicated: true, no_5s_poll: true, cancelled_prefetches: cancelledPrefetches });
      } catch (error) {
        await diagnose(page, errors);
        throw error;
      } finally {
        await context.close();
      }
    }
  } finally {
    await browser.close();
  }
  console.log(JSON.stringify({ frontend_smoke: 'passed', release_lookup: 'stubbed', pages: reports }));
}

const deadline = setTimeout(() => {
  console.error('Frontend smoke exceeded its 150-second deadline');
  process.exit(1);
}, 150_000);
main().catch((error) => {
  // Never echo the temporary credential in failure output.
  console.error(String(error.stack || error).split(token).join('[redacted]').split(password).join('[redacted]'));
  process.exitCode = 1;
}).finally(() => clearTimeout(deadline));
