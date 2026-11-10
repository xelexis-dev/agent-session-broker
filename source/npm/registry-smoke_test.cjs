const { test } = require('node:test');
const assert = require('node:assert/strict');
const { isolatedEnv, registryGuard, assertNoHarness } = require('./registry-smoke.cjs');
test('child environment excludes inherited harness, npm and credential configuration', () => {
  const unsafe = ['CODEX_THREAD_ID', 'CODEX_HOME', 'CLAUDE_CODE_MESSAGING_SOCKET', 'CLAUDE_OTHER', 'ASB_TOKEN', 'npm_config_registry', 'NPM_TOKEN', 'NODE_AUTH_TOKEN', 'NODE_OPTIONS', 'HTTPS_PROXY', 'http_proxy', 'HOME', 'OTHER_SECRET'];
  const env = isolatedEnv(Object.fromEntries([['PATH', '/bin'], ...unsafe.map(k => [k, 'not-for-child'])]));
  assert.equal(env.PATH, '/bin');
  for (const key of unsafe) assert.equal(env[key], undefined, key);
});
test('both registry settings must be the same strict loopback endpoint', () => {
  assert.equal(registryGuard('http://127.0.0.1:12345/', 'http://127.0.0.1:12345/'), 'http://127.0.0.1:12345/');
  for (const target of ['https://registry.npmjs.org/', 'http://localhost:12345/', 'http://127.0.0.1:12345/path', 'http://127.0.0.1:12345/?redirect=x']) {
    assert.throws(() => registryGuard(target, target));
    assert.throws(() => registryGuard('http://127.0.0.1:12345/', target));
  }
});
test('actual harness ancestors fail closed before MCP connection', () => {
  assert.doesNotThrow(() => assertNoHarness(['/sbin/launchd', '/usr/local/bin/node']));
  for (const executable of ['/usr/local/bin/claude', '/tmp/codex']) assert.throws(() => assertNoHarness([executable]));
});
