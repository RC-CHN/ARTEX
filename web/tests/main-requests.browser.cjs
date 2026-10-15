// Run against tests/main-requests-preview.tsx mounted at /consultation-preview.
// Requires Playwright + Chromium; all API data is synthetic and intercepted.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const { chromium } = require(process.env.ARTEX_PLAYWRIGHT_MODULE || "playwright");
const base = process.env.ARTEX_UI_TEST_URL || "http://127.0.0.1:4318";
const out = process.env.ARTEX_UI_TEST_OUTPUT || "/tmp/artex-main-requests";
const request = (id, state, extra = {}) => ({
  id,
  state,
  intent_id: id - 72,
  call_id: `call-${id}`,
  question: "发现两个环境的接口返回不一致，是否先验证配置差异？",
  context: "已核对 **接口版本**，尚未改动配置。Worker #28 正在整理相关记录。",
  answer: "",
  answer_source: "",
  main_seg: 0,
  attempts: 1,
  last_error: "",
  created_at: "2026-10-15T02:37:00Z",
  ...extra,
});

(async () => {
  fs.mkdirSync(out, { recursive: true });
  const browser = await chromium.launch({ headless: true, args: ["--no-sandbox"] });
  const context = await browser.newContext({
    viewport: { width: 1440, height: 960 },
    locale: "zh-CN",
    timezoneId: "Asia/Shanghai",
  });
  await context.addCookies([{ name: "artex_token", value: "synthetic-ui-test", url: base }]);
  let rows = [
    request(105, "pending"),
    request(104, "processing"),
    request(103, "answered", { answer: "先整理证据，等配置核对结果。", answer_source: "mainagent" }),
    request(102, "delivered", { answer: "配置已核实，请继续。", answer_source: "human" }),
    request(101, "cancelled"),
  ];
  let writes = 0;
  let failNext = false;
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (err) => errors.push(err.message));
  await page.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(base).origin) return route.abort();
    if (!url.pathname.startsWith("/api/")) return route.continue();
    if (url.pathname === "/api/tasks/fixture/main-requests") return route.fulfill({ json: rows });
    const match = url.pathname.match(/\/main-requests\/(\d+)\/reply$/);
    if (match && route.request().method() === "POST") {
      writes++;
      const row = rows.find((r) => r.id === Number(match[1]));
      if (failNext || !row || !["pending", "processing"].includes(row.state)) {
        failNext = false;
        return route.fulfill({ status: 409, json: { error: "请示已被处理，不能覆盖已有答复" } });
      }
      row.state = "answered";
      row.answer = route.request().postDataJSON().answer;
      row.answer_source = "human";
      return route.fulfill({ json: { request_id: row.id, status: "answered" } });
    }
    return route.fulfill({ status: 404, json: { error: "unmocked fixture API" } });
  });
  const refresh = async () => {
    await page.evaluate(() => window.dispatchEvent(new Event("online")));
  };
  const strip = () => page.getByRole("button", { name: /协作请示.*查看/ });
  const card = (id) => page.locator("article").filter({ hasText: `请示 #${id} ·` });
  try {
    await page.goto(`${base}/consultation-preview`);
    await strip().waitFor();
    await page.screenshot({ path: `${out}/conversation-desktop.png`, fullPage: true });
    await strip().click();
    const dialog = page.getByRole("dialog");
    await dialog.waitFor();
    assert.equal(await dialog.locator("article").count(), 3);
    await card(104).getByRole("button", { name: "查看详情" }).click();
    await card(104).getByText("Worker 提供的上下文").waitFor();
    await page.screenshot({ path: `${out}/queue-desktop.png` });
    await card(104).getByRole("button", { name: "人工接答" }).click();
    await page.getByLabel("给 Worker 的人工回复").fill("草稿：请先核对版本，再决定下一步。");
    await card(105).getByRole("button", { name: "人工接答" }).click();
    await page.getByLabel("给 Worker 的人工回复").fill("另一个问题的草稿");
    await card(104).getByRole("button", { name: "人工接答" }).click();
    assert.equal(await page.getByLabel("给 Worker 的人工回复").inputValue(), "草稿：请先核对版本，再决定下一步。");
    // A concurrent main reply must never overwrite or silently discard a draft.
    rows.find((r) => r.id === 104).state = "answered";
    rows.find((r) => r.id === 104).answer = "主 Agent 已核实版本差异。";
    rows.find((r) => r.id === 104).answer_source = "mainagent";
    await refresh();
    await dialog.getByText("这条请示已被处理或失效。草稿仍在，不能覆盖已有答复。").waitFor();
    assert.equal(await page.getByLabel("给 Worker 的人工回复").inputValue(), "草稿：请先核对版本，再决定下一步。");
    assert.ok(await page.getByRole("button", { name: "发送回复" }).isDisabled());
    await page.setViewportSize({ width: 390, height: 844 });
    await page.screenshot({ path: `${out}/reply-race-mobile.png` });
    const box = await dialog.boundingBox();
    assert.ok(box.width >= 388 && box.x >= 0, `mobile drawer width ${JSON.stringify(box)}`);
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "mobile page overflow");
    assert.ok(await dialog.evaluate((el) => el.scrollWidth <= el.clientWidth), "drawer overflow");
    await dialog.getByRole("button", { name: "关闭", exact: true }).click();
    await card(105).getByRole("button", { name: "人工接答" }).click();
    await page.getByLabel("给 Worker 的人工回复").fill("人工回复测试：先核对已保存的配置记录。");
    failNext = true;
    await page.getByRole("button", { name: "发送回复" }).click();
    await page.getByText("请示已被处理，不能覆盖已有答复", { exact: true }).waitFor();
    assert.equal(await page.getByLabel("给 Worker 的人工回复").inputValue(), "人工回复测试：先核对已保存的配置记录。");
    // Dispatch two clicks before React renders: only one successful mutation.
    await page.waitForFunction(() =>
      Array.from(document.querySelectorAll("button")).some((b) => b.textContent.includes("发送回复") && !b.disabled),
    );
    const before = writes;
    await page.getByRole("button", { name: "发送回复" }).evaluate((el) => {
      el.click();
      el.click();
    });
    await page.getByText("人工回复已提交，将送回 Worker 会话", { exact: true }).waitFor();
    assert.equal(writes - before, 1);
    assert.equal(rows.find((r) => r.id === 105).answer_source, "human");
    await dialog.getByRole("button", { name: /已处理/ }).click();
    assert.equal(await dialog.locator("article").count(), 2);
    await card(102).getByRole("button", { name: "查看详情" }).click();
    await card(102).getByText("人工回复", { exact: true }).waitFor();
    assert.equal(await dialog.getByRole("button", { name: "人工接答" }).count(), 0);
    await page.screenshot({ path: `${out}/history-mobile.png` });
    await card(102).getByRole("button", { name: "Worker #30" }).click();
    await page.getByRole("status").filter({ hasText: "已跳转 Worker #30" }).waitFor();
    await strip().click();
    await page.keyboard.press("Escape");
    await dialog.waitFor({ state: "hidden" });
    await page.setViewportSize({ width: 1440, height: 960 });
    await page.evaluate(() => {
      document.documentElement.classList.add("dark");
      document.documentElement.dataset.themeMode = "dark";
    });
    await strip().click();
    await dialog.waitFor();
    await page.screenshot({ path: `${out}/queue-dark.png`, animations: "disabled" });
    assert.deepEqual(errors, []);
    fs.writeFileSync(
      `${out}/browser-results.json`,
      JSON.stringify(
        {
          passed: true,
          checks: [
            "desktop transcript",
            "queue and context",
            "concurrent answer preserves draft",
            "mobile width and overflow",
            "HTTP conflict preserves draft",
            "double click idempotence",
            "history sources",
            "Worker navigation",
            "keyboard dismiss",
            "dark theme",
            "no browser exceptions",
          ],
          writes,
        },
        null,
        2,
      ),
    );
    console.log("Consultation browser checks: PASS (synthetic API only)");
  } finally {
    await browser.close();
  }
})().catch((err) => {
  console.error(err);
  process.exitCode = 1;
});
