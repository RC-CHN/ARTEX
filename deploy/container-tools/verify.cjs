'use strict';

const assert = require('node:assert/strict');
const { createRequire } = require('node:module');
const path = require('node:path');
const tools = require('./package.json').dependencies;

(async () => {
  for (const [name, expected] of Object.entries(tools)) {
    const manifest = path.join(__dirname, 'node_modules', name, 'package.json');
    assert.equal(require(manifest).version, expected, `${name} version`);
    if (name === 'puppeteer-core') {
      const puppeteer = require(manifest.replace('/package.json', ''));
      const browser = await puppeteer.launch({
        executablePath: require('playwright').chromium.executablePath(),
        headless: true, args: ['--no-sandbox'],
      });
      try {
        const page = await browser.newPage();
        await page.setContent('<title>ARTEX api-recon smoke</title>');
        assert.equal(await page.title(), 'ARTEX api-recon smoke');
      } finally { await browser.close(); }
      console.log(`${name}@${expected}: offline api-recon browser OK`);
      continue;
    }
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
