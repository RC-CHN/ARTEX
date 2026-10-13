'use strict';

const assert = require('node:assert/strict');
const { spawn } = require('node:child_process');
const readline = require('node:readline');

// Exercise the existing database's command through stdio, including a browser
// launch with the image's default browser configuration and no network access.
const child = spawn('npx', ['@playwright/mcp', '--headless', '--isolated'], {
  cwd: '/tmp',
  stdio: ['pipe', 'pipe', 'inherit'],
  env: { ...process.env, npm_config_offline: 'true' },
});
const pending = new Map();
let nextID = 0;
const lines = readline.createInterface({ input: child.stdout });
const timeout = setTimeout(() => {
  for (const { reject } of pending.values()) reject(new Error('MCP smoke timed out'));
  child.kill('SIGKILL');
}, 45000);
child.on('error', error => {
  for (const { reject } of pending.values()) reject(error);
});
child.on('exit', code => {
  for (const { reject } of pending.values()) reject(new Error(`MCP exited: ${code}`));
});
lines.on('line', line => {
  let message;
  try { message = JSON.parse(line); } catch { return; }
  const waiter = pending.get(message.id);
  if (!waiter) return;
  pending.delete(message.id);
  if (message.error) waiter.reject(new Error(JSON.stringify(message.error)));
  else waiter.resolve(message.result);
});

function request(method, params) {
  const id = ++nextID;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    child.stdin.write(`${JSON.stringify({ jsonrpc: '2.0', id, method, params })}\n`);
  });
}

(async () => {
  try {
    await request('initialize', {
      protocolVersion: '2024-11-05', capabilities: {},
      clientInfo: { name: 'artex-image-smoke', version: '1.0.0' },
    });
    child.stdin.write(`${JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' })}\n`);
    const navigation = await request('tools/call', { name: 'browser_navigate', arguments: { url: 'about:blank' } });
    assert.ok(!navigation.isError, JSON.stringify(navigation));
    const evaluation = await request('tools/call', {
      name: 'browser_evaluate',
      arguments: { function: '() => { document.title = "ARTEX MCP smoke"; return document.title; }' },
    });
    assert.ok(!evaluation.isError, JSON.stringify(evaluation));
    assert.ok(JSON.stringify(evaluation).includes('ARTEX MCP smoke'));
    console.log('MCP stdio browser navigation and evaluation OK');
  } finally {
    clearTimeout(timeout);
    lines.close();
    child.stdin.end();
    child.kill('SIGTERM');
  }
})().catch(error => {
  console.error(error);
  process.exitCode = 1;
});
