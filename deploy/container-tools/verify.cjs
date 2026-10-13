'use strict';

const assert = require('node:assert/strict');
const { createRequire } = require('node:module');
const path = require('node:path');
const tools = require('./package.json').dependencies;

(async () => {
  for (const [name, expected] of Object.entries(tools)) {
    const manifest = path.join(__dirname, 'node_modules', name, 'package.json');
    assert.equal(require(manifest).version, expected, `${name} version`);
    const toolRequire = createRequire(manifest);
    const { chromium } = toolRequire('playwright');
    const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
    try {
      const page = await browser.newPage();
      await page.setContent('<title>ARTEX browser smoke</title><p>ready</p>');
      assert.equal(await page.title(), 'ARTEX browser smoke');
      console.log(`${name}@${expected}: Chromium ${browser.version()} OK`);
    } finally {
      await browser.close();
    }
  }
})().catch(error => {
  console.error(error);
  process.exitCode = 1;
});
