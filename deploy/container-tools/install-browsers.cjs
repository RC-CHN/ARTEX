'use strict';

const { execFileSync } = require('node:child_process');
const { createRequire } = require('node:module');
const path = require('node:path');
const tools = require('./package.json').dependencies;
const installed = new Set();

for (const name of Object.keys(tools)) {
  if (name === 'puppeteer-core') continue; // Uses the root Playwright Chromium.
  const toolRequire = createRequire(path.join(__dirname, 'node_modules', name, 'package.json'));
  const manifest = toolRequire.resolve('playwright/package.json');
  const version = toolRequire(manifest).version;
  if (installed.has(version)) continue;

  console.log(`Installing Chromium for Playwright ${version} (${name})`);
  const args = [path.join(path.dirname(manifest), 'cli.js'), 'install'];
  if (installed.size === 0) args.push('--with-deps');
  args.push('chromium');
  execFileSync(process.execPath, args, { stdio: 'inherit' });
  installed.add(version);
}
