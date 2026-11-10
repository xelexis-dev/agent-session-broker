#!/usr/bin/env node
'use strict';
// A disposable registry exercise, not a public publishing command.
// node registry-smoke.cjs --packages DIR --version VERSION --server FILE
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const http = require('node:http');
const crypto = require('node:crypto');
const { spawn } = require('node:child_process');
const assert = require('node:assert/strict');
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const hash = bytes => 'sha512-' + crypto.createHash('sha512').update(bytes).digest('base64');
function isolatedEnv(input) {
  return Object.fromEntries(['PATH', 'SystemRoot', 'WINDIR', 'PATHEXT', 'LANG', 'LC_ALL'].filter(k => input[k]).map(k => [k, input[k]]));
}
function registryGuard(defaultURL, scopedURL) {
  assert.match(defaultURL, /^http:\/\/127\.0\.0\.1:[1-9]\d*\/$/);
  assert.equal(scopedURL, defaultURL);
  const parsed = new URL(defaultURL); assert(parsed.port && Number(parsed.port) <= 65535);
  return defaultURL;
}
function assertNoHarness(names) {
  assert(!names.some(n => ['claude', 'codex', 'claude.exe', 'codex.exe'].includes(path.basename(n).toLowerCase())), 'supported harness ancestor: refusing MCP connection');
}
function childRun(command, args, options) {
  const child = spawn(command, args, { ...options, stdio: ['pipe', 'pipe', 'pipe'] });
  let stdout = '', stderr = '';
  child.stdout.on('data', data => { stdout += data; }); child.stderr.on('data', data => { stderr += data; });
  const result = new Promise((resolve, reject) => {
    const timer = setTimeout(() => { child.kill('SIGKILL'); reject(new Error('child command timed out')); }, 90000);
    child.once('error', err => { clearTimeout(timer); reject(err); });
    child.once('close', (code, signal) => { clearTimeout(timer); resolve({ code, signal, stdout, stderr }); });
  });
  result.catch(() => {}); // Awaiters still receive failure; background cleanup has no unhandled rejection.
  return { child, result };
}
async function run(command, args, options, expected = 0) {
  const c = childRun(command, args, options); c.child.stdin.end(); const answer = await c.result;
  assert.equal(answer.code, expected, `${path.basename(command)} failed: ${answer.stderr.slice(-1000)}`); return answer;
}
async function processRows() {
  const result = await run('ps', ['-axo', 'pid=,ppid=,comm='], { env: isolatedEnv(process.env) });
  return result.stdout.trim().split('\n').map(line => { const m = line.trim().match(/^(\d+)\s+(\d+)\s+(.+)$/); return m && { pid: +m[1], ppid: +m[2], executable: m[3] }; }).filter(Boolean);
}
async function ancestors() {
  const rows = await processRows(), names = []; let pid = process.ppid;
  for (let i = 0; pid > 1 && i < 32; i++) { const row = rows.find(r => r.pid === pid); assert(row, 'cannot inspect parent'); names.push(row.executable); pid = row.ppid; }
  return names;
}
async function until(check, ms = 15000) {
  const end = Date.now() + ms;
  while (Date.now() < end) { const value = await check(); if (value) return value; await delay(50); }
  throw new Error('scenario wait timed out');
}
async function fixture() {
  const packages = new Map(), blobs = new Map(), requests = [];
  let fault = '', external = 0, url;
  const server = http.createServer(async (req, res) => {
    if (!req.url.startsWith('/') || req.headers.host !== new URL(url).host) { external++; res.writeHead(405).end(); return; }
    const target = new URL(req.url, url).pathname;
    requests.push({ method: req.method, path: target });
    if (fault === '500') { res.writeHead(500).end('{}'); return; }
    if (req.method === 'GET' && blobs.has(target)) { res.writeHead(200, { 'content-type': 'application/octet-stream' }).end(blobs.get(target)); return; }
    const name = decodeURIComponent(target.slice(1));
    if (fault === name) { res.writeHead(404).end('{}'); return; }
    if (req.method === 'GET') {
      res.writeHead(packages.has(name) ? 200 : 404, { 'content-type': 'application/json' }).end(JSON.stringify(packages.get(name) ?? { error: 'not found' })); return;
    }
    if (req.method !== 'PUT' || name !== '@xelexis/x-broker') { res.writeHead(405).end(); return; }
    try {
      let body = ''; for await (const chunk of req) { body += chunk; assert(body.length < 100 * 1024 * 1024, 'publish body too large'); }
      const doc = JSON.parse(body); assert.equal(doc.name, name);
      if (packages.has(name)) { res.writeHead(409).end('{}'); return; }
      const versions = Object.values(doc.versions); assert.equal(versions.length, 1);
      const dist = versions[0].dist, targetURL = new URL(dist.tarball);
      assert.equal(targetURL.origin + '/', url);
      const attachments = Object.values(doc._attachments); assert.equal(attachments.length, 1);
      const bytes = Buffer.from(attachments[0].data, 'base64'); assert.equal(hash(bytes), dist.integrity);
      blobs.set(targetURL.pathname, bytes); delete doc._attachments; packages.set(name, doc);
      res.writeHead(201, { 'content-type': 'application/json' }).end('{"ok":true}');
    } catch { res.writeHead(400).end('{"error":"invalid fixture publication"}'); }
  });
  server.on('connect', (_req, socket) => { external++; socket.end('HTTP/1.1 405 Method Not Allowed\r\n\r\n'); });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve)); url = `http://127.0.0.1:${server.address().port}/`;
  return { server, url, packages, requests, blobs, setFault: value => { fault = value; }, external: () => external };
}
async function scenario(args, work) {
  assert.equal(process.platform === 'win32', false, 'this process-tree scenario requires POSIX; Windows is not validated');
  await until(() => process.ppid === 1, 5000); assertNoHarness(await ancestors());
  const registry = await fixture(), children = [], evidence = { platform: `${process.platform}-${process.arch}`, checks: [], artifacts: [] };
  const check = (condition, label) => { assert(condition, label); evidence.checks.push(label); console.log(`ok - ${label}`); };
  const empty = name => { const dir = path.join(work, name); fs.mkdirSync(dir, { recursive: true }); return dir; };
  const env = { ...isolatedEnv(process.env), HOME: empty('home'), TMPDIR: empty('tmp'), npm_config_cache: empty('publish-cache'), npm_config_prefix: empty('prefix'), npm_config_userconfig: path.join(work, 'user.npmrc'), npm_config_globalconfig: path.join(work, 'global.npmrc'), npm_config_audit: 'false', npm_config_fund: 'false', npm_config_update_notifier: 'false', npm_config_fetch_retries: '0', npm_config_proxy: registry.url, npm_config_https_proxy: registry.url, npm_config_noproxy: '127.0.0.1' };
  fs.writeFileSync(env.npm_config_globalconfig, '');
  fs.writeFileSync(env.npm_config_userconfig, `registry=${registry.url}\n@xelexis:registry=${registry.url}\n//127.0.0.1:${new URL(registry.url).port}/:_authToken=fixture-only\n`);
  const options = { cwd: work, env };
  const npm = (argv, opts = options, expected = 0) => run('npm', [...argv, '--registry', registry.url], opts, expected);
  const state = (name, file, expected = 0, stateOptions = options) => run(process.execPath, [path.join(__dirname, 'publish-state.cjs'), name, args.version, file], stateOptions, expected);
  const name = '@xelexis/x-broker', tarball = path.join(args.packages, `xelexis-x-broker-${args.version}.tgz`);
  try {
    const defaultURL = (await run('npm', ['config', 'get', 'registry'], options)).stdout.trim();
    const scopedURL = (await run('npm', ['config', 'get', '@xelexis:registry'], options)).stdout.trim();
    registryGuard(defaultURL, scopedURL); check(defaultURL === registry.url, 'both npm registry settings equal the guarded loopback fixture');
    {
      const file = tarball, spec = name;
      evidence.artifacts.push({ name: spec, integrity: hash(fs.readFileSync(file)) });
      check((await state(spec, file)).stdout.trim() === 'publish', `${spec}: missing version permits publish`);
      const puts = registry.requests.filter(r => r.method === 'PUT').length;
      await npm(['publish', file, '--ignore-scripts', '--provenance=false', '--access', 'public', '--tag', 'test', '--dry-run']);
      check(registry.requests.filter(r => r.method === 'PUT').length === puts, `${spec}: dry-run performs no PUT`);
      await npm(['publish', file, '--ignore-scripts', '--provenance=false', '--access', 'public', '--tag', 'test']);
      const doc = registry.packages.get(spec); assert.equal(doc['dist-tags'].test, args.version); assert.equal(doc['dist-tags'].latest, undefined);
      assert.equal(doc.versions[args.version].dist.integrity, evidence.artifacts.at(-1).integrity);
      check((await state(spec, file)).stdout.trim() === 'skip', `${spec}: real publication integrity matches and repeat skips`);
    }
    assert.deepEqual(registry.requests.filter(r => r.method === 'PUT').map(r => decodeURIComponent(r.path.slice(1))), [name]);
    check(true, 'one real publication of the single package; test tag only, no latest');
    const before = registry.requests.filter(r => r.method === 'PUT').length, other = path.join(work, 'other.tgz');
    fs.writeFileSync(other, crypto.randomBytes(64));
    await state(name, other, 1);
    registry.setFault('500'); await state(name, tarball, 1); registry.setFault('');
    check(registry.requests.filter(r => r.method === 'PUT').length === before, 'content mismatch and HTTP500 fail closed without publication');
    const broker = childRun(args.server, ['--listen', '127.0.0.1:0', '--memory-dir', path.join(work, 'broker')], options); children.push(broker.child);
    let log = ''; broker.child.stdout.on('data', d => { log += d; }); broker.child.stderr.on('data', d => { log += d; });
    const port = await until(() => log.match(/127\.0\.0\.1:([1-9]\d*)/)?.[1]); const base = `http://127.0.0.1:${port}`, jar = new Map();
    const request = async (method, target, form) => {
      const res = await fetch(base + target, { method, headers: { "user-agent": "asb-npm-smoke/1", cookie: [...jar].map(([k, v]) => `${k}=${v}`).join('; '), ...(form ? { 'content-type': 'application/x-www-form-urlencoded' } : {}) }, body: form && new URLSearchParams(form), redirect: 'manual' });
      for (const cookie of res.headers.getSetCookie()) { const [key, value] = cookie.split(';')[0].split('='); if (value) jar.set(key, value); else jar.delete(key); }
      assert(res.status < 400, `temporary broker ${method} ${target}: ${res.status}`); return { location: res.headers.get('location'), text: await res.text() };
    };
    const csrf = html => html.match(/name="csrf" value="([^"]+)"/)[1], password = crypto.randomBytes(24).toString('hex');
    await request('POST', '/setup', { setupCode: fs.readFileSync(path.join(work, "broker", "control", "setup-code"), "utf8").trim(), password, confirmPassword: password, csrf: csrf((await request('GET', '/setup')).text) });
    await request('POST', '/login', { username: 'admin', password, csrf: csrf((await request('GET', '/login')).text) });
    const formToken = csrf((await request('GET', '/')).text);
    await request('POST', '/projects/create', { name: 'npm-smoke', csrf: formToken });
    await request('POST', '/admin/http-policy', { allowHTTP: 'true', csrf: formToken });
    const issued = await request('POST', '/tokens/project', { project: 'npm-smoke', label: 'npm-test', csrf: formToken });
    const token = (await request('GET', issued.location)).text.match(/<code id="issued-token">([^<]+)<\/code>/)[1];
    const runtimeEnv = { ...env, npm_config_cache: empty('cold-cache') };
    const runtimeOptions = { cwd: empty('consumer'), env: runtimeEnv };
    const command = ['--yes', '--registry', registry.url, '--package', `${name}@${args.version}`, 'x-broker'];
    const clientArgs = ['--broker-url', `${base}/mcp?project=npm-smoke`, '--token', token, '--auth-dir', path.join(work, 'auth')];
    async function mcp(offline, killLauncher = false) {
      const handle = childRun('npx', [...(offline ? ['--offline'] : []), ...command, ...clientArgs], runtimeOptions); children.push(handle.child);
      let text = '', tail = ''; const messages = new Map();
      handle.child.stdout.on('data', data => { text += data; tail += data; while (tail.includes('\n')) { const at = tail.indexOf('\n'), line = tail.slice(0, at); tail = tail.slice(at + 1); try { const msg = JSON.parse(line); assert.equal(msg.jsonrpc, '2.0'); if (msg.id !== undefined) messages.set(msg.id, msg); } catch { messages.set('invalid', true); } } });
      const call = async message => { handle.child.stdin.write(JSON.stringify(message) + '\n'); return until(() => messages.get(message.id)); };
      const init = await call({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'registry-smoke', version: '1' } } }); assert(init.result?.serverInfo);
      handle.child.stdin.write('{"jsonrpc":"2.0","method":"notifications/initialized"}\n');
      const listed = await call({ jsonrpc: '2.0', id: 2, method: 'tools/list' }); const names = listed.result?.tools?.map(t => t.name) ?? [];
      assert(['attach_session', 'auth_status', 'list_sessions', 'send_message'].every(tool => names.includes(tool)));
      const rows = await processRows(), owned = [handle.child.pid]; for (let i = 0; i < owned.length; i++) for (const row of rows) if (row.ppid === owned[i] && !owned.includes(row.pid)) owned.push(row.pid);
      const client = rows.find(r => owned.includes(r.pid) && path.basename(r.executable) === 'asb-client'); assert(client, 'native client descendant must be observed');
      if (killLauncher) process.kill(client.ppid, 'SIGKILL'); else handle.child.stdin.end();
      const ended = await handle.result; if (!killLauncher) assert.equal(ended.code, 0);
      await until(async () => { const remaining = await processRows(); return owned.every(pid => !remaining.some(r => r.pid === pid)); });
      assert(!messages.has('invalid') && tail === ''); check(true, `${offline ? 'warm offline' : 'cold registry'} npx MCP JSONL initialize/tools-list and ${killLauncher ? 'launcher kill' : 'EOF'} leave no descendants`);
      evidence.mcp = { stdout: text.trim().split('\n').map(line => { const msg = JSON.parse(line); return { jsonrpc: msg.jsonrpc, id: msg.id, result: msg.id === 1 ? 'serverInfo' : 'tools' }; }), toolCount: names.length, descendants: owned.length };
    }
    const coldStart = registry.requests.length; await mcp(false);
    const coldGets = registry.requests.slice(coldStart).filter(r => r.method === 'GET' && r.path.endsWith('.tgz'));
    const expectedPath = new URL(registry.packages.get(name).versions[args.version].dist.tarball).pathname;
    assert.deepEqual(coldGets.map(r => r.path), [expectedPath]); check(true, 'empty cache downloads the single tarball exactly once');
    const version = await run('npx', ['--offline', ...command, '--version'], runtimeOptions); assert.equal(version.stdout, `asb-client ${args.version}\n`);
    const warmStart = registry.requests.length; await mcp(true); await mcp(true, true); check(registry.requests.length === warmStart, 'warm offline runs issue zero HTTP requests');
    const sessions = (await request('GET', '/projects/sessions?project=npm-smoke')).text;
    check(sessions.includes('아직 등록된 세션이 없습니다.'), 'temporary broker registered session count remains zero'); evidence.registeredSessions = 0;
    registry.setFault(name);
    const absent = await run('npx', [...command, '--version'], { cwd: empty('missing-consumer'), env: { ...env, npm_config_cache: empty('missing-cache') } }, 1);
    assert.equal(absent.stdout, ''); assert(absent.stderr.includes('E404')); registry.setFault('');
    check(true, 'a package absent from the registry fails in npm with E404 and empty stdout');
    check(registry.external() === 0, 'external proxy/absolute-URL/CONNECT request count is zero');
    await new Promise(resolve => registry.server.close(resolve));
    await state(name, tarball, 1, { ...options, env: { ...env, npm_config_cache: empty('refused-cache') } }); check(true, 'connection refusal with an empty cache fails closed');
    evidence.requestCount = registry.requests.length; evidence.externalRequests = registry.external(); console.log(JSON.stringify(evidence));
  } finally {
    for (const child of children) if (child.exitCode === null && child.signalCode === null) child.kill('SIGTERM');
    await until(() => children.every(child => child.exitCode !== null || child.signalCode !== null)).catch(() => { for (const child of children) if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL'); });
    registry.server.closeAllConnections(); if (registry.server.listening) await new Promise(resolve => registry.server.close(resolve));
  }
}
async function entry() {
  const args = Object.fromEntries(process.argv.slice(2).reduce((pairs, value, i, all) => i % 2 ? pairs : [...pairs, [value.replace(/^--/, ''), all[i + 1]]], []));
  assert(args.packages && args.version && args.server, '--packages, --version and --server are required');
  assert.match(args.version, /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/);
  for (const key of ['packages', 'server']) args[key] = fs.realpathSync(args[key]);
  if (args['worker-dir']) {
    const workerDir = args['worker-dir'], work = fs.mkdtempSync(path.join(workerDir, 'run-')); let code = 0;
    try { await scenario(args, work); } catch (err) { code = 1; console.error(`not ok - ${err.message}`); }
    finally { fs.rmSync(work, { recursive: true, force: true }); fs.writeFileSync(path.join(workerDir, 'result'), String(code)); }
    process.exitCode = code; return;
  }
  assert.notEqual(process.platform, 'win32', 'Windows process-tree execution is not validated');
  const dir = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'npm-registry-runner-'));
  let pid;
  try {
    const workerArgs = [__filename, '--packages', args.packages, '--version', args.version, '--server', args.server, '--worker-dir', dir];
    const boot = `const fs=require('fs'),{spawn}=require('child_process');const d=process.argv[1],a=JSON.parse(process.argv[2]);const fd=fs.openSync(d+'/output','w');const c=spawn(process.execPath,a,{detached:true,stdio:['ignore',fd,fd],env:process.env});fs.writeFileSync(d+'/pid',String(c.pid));c.unref();`;
    await run(process.execPath, ['-e', boot, dir, JSON.stringify(workerArgs)], { env: isolatedEnv(process.env), cwd: dir });
    pid = Number(fs.readFileSync(path.join(dir, 'pid'), 'utf8'));
    await until(() => fs.existsSync(path.join(dir, 'result')), 12 * 60 * 1000);
    process.stdout.write(fs.readFileSync(path.join(dir, 'output'))); process.exitCode = Number(fs.readFileSync(path.join(dir, 'result'), 'utf8'));
  } finally {
    if (pid) {
      const gone = () => { try { process.kill(-pid, 0); return false; } catch { return true; } };
      try { process.kill(-pid, 'SIGTERM'); } catch {}
      await until(gone, 5000).catch(async () => { try { process.kill(-pid, 'SIGKILL'); } catch {} await until(gone, 5000); });
    }
    fs.rmSync(dir, { recursive: true, force: true });
  }
}
module.exports = { isolatedEnv, registryGuard, assertNoHarness };
if (require.main === module) entry().catch(err => { console.error(`not ok - ${err.message}`); process.exitCode = 1; });
