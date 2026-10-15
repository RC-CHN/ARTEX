'use strict';

// Offline protocol interoperability checks against independent local fixtures.
// Run in the default tool image; ws is in its pinned browser dependency tree.
const assert = require('node:assert/strict');
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const http = require('node:http');
const https = require('node:https');
const http2 = require('node:http2');
const os = require('node:os');
const path = require('node:path');
const { createRequire } = require('node:module');
const { WebSocketServer } = createRequire('/opt/artex-tools/package.json')('ws');

function run(command, args, input = '', env = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { env: { ...process.env, ...env } });
    const stdout = [], stderr = [];
    const timer = setTimeout(() => { child.kill('SIGKILL'); }, 15000);
    child.stdout.on('data', b => stdout.push(b));
    child.stderr.on('data', b => stderr.push(b));
    child.stdin.on('error', () => {}); // Expected if a negative control fails before reading stdin.
    child.on('error', error => { clearTimeout(timer); reject(error); });
    child.on('close', (code, signal) => {
      clearTimeout(timer);
      resolve({ code, signal, stdout: Buffer.concat(stdout), stderr: Buffer.concat(stderr).toString() });
    });
    child.stdin.end(input);
  });
}

function ok(result) {
  assert.equal(result.code, 0, JSON.stringify({ ...result, stdout: result.stdout.toString() }));
  return result.stdout;
}

function failed(result, pattern) {
  assert.notEqual(result.code, null, 'fixture timed out or process was killed');
  assert.notEqual(result.code, 0, 'negative control unexpectedly succeeded');
  assert.match(result.stderr, pattern);
}

async function main() {
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'artex-protocol-smoke-'));
  const servers = [], sessions = new Set(), sockets = new Set(), wsServers = [];
  async function listen(server) {
    servers.push(server);
    server.on('connection', socket => {
      sockets.add(socket);
      socket.on('close', () => sockets.delete(socket));
    });
    await new Promise((resolve, reject) => {
      server.once('error', reject);
      server.listen(0, '127.0.0.1', resolve);
    });
    return `127.0.0.1:${server.address().port}`;
  }
  try {
    const key = path.join(temp, 'key.pem'), cert = path.join(temp, 'cert.pem');
    ok(await run('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', key,
      '-out', cert, '-subj', '/CN=localhost', '-addext', 'subjectAltName=IP:127.0.0.1,DNS:localhost', '-days', '1']));
    const tls = { key: fs.readFileSync(key), cert: fs.readFileSync(cert) };
    const proto = path.join(temp, 'fixture.proto');
    fs.writeFileSync(proto, 'syntax="proto3"; package fixture; message Message { string text=1; } ' +
      'service Echo { rpc Send(Message) returns (Message); rpc Wait(Message) returns (Message); }');

    function grpcServer(server) {
      server.on('session', session => {
        sessions.add(session);
        session.on('close', () => sessions.delete(session));
        session.on('error', () => {});
      });
      server.on('stream', (stream, headers) => {
        stream.on('error', () => {});
        if (!headers[':path'].startsWith('/fixture.Echo/')) {
          // Reflection is bidirectional: reject before waiting for request EOF.
          stream.respond({ ':status': 200, 'content-type': 'application/grpc', 'grpc-status': '12' }, { endStream: true });
          return;
        }
        const chunks = [];
        stream.on('data', b => chunks.push(b));
        stream.on('end', () => {
          if (stream.destroyed) return;
          let status = '0';
          if (headers.authorization !== 'Bearer fixture-token') status = '16';
          else if (headers[':path'].endsWith('/Wait')) return; // Deadline control.
          const request = Buffer.concat(chunks);
          stream.respond({ ':status': 200, 'content-type': 'application/grpc' }, { waitForTrailers: true });
          stream.on('wantTrailers', () => stream.sendTrailers({ 'grpc-status': status }));
          if (status === '0') {
            assert.equal(request[0], 0);
            assert.equal(request.readUInt32BE(1), request.length - 5);
            stream.end(request); // Echo the real protobuf wire message.
          } else stream.end();
        });
      });
      return server;
    }
    const plain = await listen(grpcServer(http2.createServer()));
    const secure = await listen(grpcServer(http2.createSecureServer(tls)));
    const schema = ['-import-path', temp, '-proto', 'fixture.proto'];
    const auth = ['-expand-headers', '-H', 'Authorization: Bearer ${TOKEN}'];
    const invoke = (address, transport, extra = auth, method = 'Send') =>
      run('grpcurl', ['-connect-timeout', '2', '-max-time', '3', ...transport, ...schema,
        ...extra, '-d', '@', address, 'fixture.Echo/' + method], '{"text":"ARTEX 协议 fixture"}', { TOKEN: 'fixture-token' });
    for (const [address, transport] of [[plain, ['-plaintext']], [secure, ['-cacert', cert]]]) {
      assert.equal(JSON.parse(ok(await invoke(address, transport))).text, 'ARTEX 协议 fixture');
      failed(await invoke(address, transport, []), /Unauthenticated/);
    }
    failed(await invoke(secure, []), /certificate|unknown authority/i);
    failed(await run('grpcurl', ['-plaintext', '-max-time', '3', plain, 'list']), /reflection|Unimplemented/i);
    failed(await invoke(plain, ['-plaintext', '-max-time', '0.3'], auth, 'Wait'), /DeadlineExceeded|deadline exceeded/i);
    console.log('grpcurl: plaintext/TLS protobuf calls, metadata, no reflection, auth failure, certificate rejection and deadline: OK');

    function websocketServer(server) {
      const wss = new WebSocketServer({ noServer: true });
      wsServers.push(wss);
      server.on('upgrade', (request, socket, head) => {
        if (request.headers.authorization !== 'Bearer fixture-token' || request.headers.origin !== 'https://fixture.invalid') {
          socket.end('HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\nConnection: close\r\n\r\n');
          return;
        }
        wss.handleUpgrade(request, socket, head, ws => {
          assert.equal(ws.protocol, 'fixture-v1');
          ws.on('message', (data, binary) => {
            if (request.url === '/silent') return;
            assert.equal(binary, request.url === '/binary');
            ws.send(data, { binary });
          });
        });
      });
      return server;
    }
    const ws = await listen(websocketServer(http.createServer()));
    const wss = await listen(websocketServer(https.createServer(tls)));
    const wsAuth = ['-H=Authorization: Bearer fixture-token', '--origin=https://fixture.invalid', '--protocol=fixture-v1'];
    for (const base of [`ws://${ws}`, `wss://${wss}`]) {
      const env = { SSL_CERT_FILE: cert };
      const text = Buffer.from('ARTEX WebSocket fixture\n');
      assert.deepEqual(ok(await run('websocat', ['-t', '-n', '-1', ...wsAuth, base + '/text'], text, env)), text);
      const binary = Buffer.from([0, 1, 255, 128, 10, 13, 42]);
      assert.deepEqual(ok(await run('websocat', ['-b', '-n', '-1', ...wsAuth, base + '/binary'], binary, env)), binary);
      failed(await run('websocat', ['-t', '-n', '-1', base + '/text'], text, env), /401|Unauthorized|unexpected status/i);
    }
    failed(await run('websocat', ['-t', '-n', '-1', ...wsAuth, `wss://${wss}/text`], 'fixture\n', { SSL_CERT_FILE: '' }), /certificate|tls|ssl/i);
    const timeout = await run('timeout', ['--signal=TERM', '--kill-after=2s', '0.3s', 'websocat', '-t', '-n', '-1', ...wsAuth, `ws://${ws}/silent`], 'fixture\n');
    assert.equal(timeout.code, 124, timeout.stderr);
    console.log('websocat: ws/wss text and binary round trips, Origin/subprotocol/auth, certificate rejection and timeout: OK');
  } finally {
    for (const wss of wsServers) { for (const client of wss.clients) client.terminate(); wss.close(); }
    for (const session of sessions) session.destroy();
    for (const socket of sockets) socket.destroy();
    await Promise.all(servers.map(server => new Promise(resolve => server.close(resolve))));
    fs.rmSync(temp, { recursive: true, force: true });
  }
}

main().catch(error => { console.error(error); process.exitCode = 1; });
