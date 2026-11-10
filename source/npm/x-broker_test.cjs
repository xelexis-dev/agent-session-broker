// Run with node --test. The fake client stands in for asb-client so argv,
// stdio, exit status, signals and orphan handling can be observed exactly.
const { test } = require("node:test");
const assert = require("node:assert/strict");
const { spawn } = require("node:child_process");
const crypto = require("node:crypto");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const launcher = path.join(__dirname, "x-broker.js");
const { binaryPath } = require(launcher);
const posix = process.platform !== "win32";

const fakeClient = `#!${process.execPath}
const fs = require("node:fs");
const mode = process.env.FAKE_MODE;
if (process.env.FAKE_PID) fs.writeFileSync(process.env.FAKE_PID, String(process.pid));
if (mode === "report") {
  process.stdout.write(JSON.stringify({ argv: process.argv.slice(2), cwd: process.cwd(), probe: process.env.FAKE_PROBE }));
  process.exitCode = Number(process.env.FAKE_EXIT || 0);
} else if (mode === "echo") {
  process.stdin.pipe(process.stdout);
  process.stdin.on("end", () => { process.exitCode = Number(process.env.FAKE_EXIT || 0); });
} else if (mode === "wait") {
  process.stdin.resume();
  process.stdin.on("end", () => process.exit(0));
}
`;

// Lay out node_modules the way npm installs the single package: the launcher
// and every platform's client under bin/<platform>/.
function install(t, { client = true, mode = 0o755 } = {}) {
  const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "x-broker-launcher-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const main = path.join(root, "node_modules", "@xelexis", "x-broker");
  fs.mkdirSync(path.join(main, "bin"), { recursive: true });
  fs.writeFileSync(path.join(main, "package.json"), JSON.stringify({ name: "@xelexis/x-broker", version: "1.2.3" }));
  fs.copyFileSync(launcher, path.join(main, "bin", "x-broker.js"));
  if (client) {
    const dir = path.join(main, "bin", `${process.platform}-${process.arch}`);
    fs.mkdirSync(dir);
    fs.writeFileSync(path.join(dir, "asb-client"), fakeClient);
    fs.chmodSync(path.join(dir, "asb-client"), mode);
  }
  return { root, bin: path.join(main, "bin", "x-broker.js") };
}

function run(bin, args, { env = {}, cwd, input } = {}) {
  const child = spawn(process.execPath, [bin, ...args], { cwd, env: { ...process.env, ...env }, stdio: ["pipe", "pipe", "pipe"] });
  const out = [];
  let err = "";
  child.stdout.on("data", (d) => out.push(d));
  child.stderr.on("data", (d) => { err += d; });
  if (input !== undefined) child.stdin.end(input);
  const done = new Promise((resolve) => child.on("close", (code, signal) => resolve({ code, signal, stdout: Buffer.concat(out), stderr: err })));
  return { child, done };
}

async function waitFor(check, ms = 10000) {
  for (const end = Date.now() + ms; Date.now() < end; await new Promise((r) => setTimeout(r, 25))) {
    const value = check();
    if (value) return value;
  }
  throw new Error("timed out");
}

const alive = (pid) => { try { process.kill(pid, 0); return true; } catch { return false; } };

test("supported platforms map to their bundled binary; others are rejected", () => {
  const dir = path.join("/pkg", "bin");
  assert.equal(binaryPath({ platform: "win32", arch: "x64", dir }), path.join(dir, "win32-x64", "asb-client.exe"));
  for (const [platform, arch] of [["darwin", "arm64"], ["darwin", "x64"], ["linux", "x64"]]) {
    assert.equal(binaryPath({ platform, arch, dir }), path.join(dir, `${platform}-${arch}`, "asb-client"));
  }
  for (const [platform, arch] of [["linux", "arm64"], ["win32", "arm64"], ["freebsd", "x64"]]) {
    assert.throws(() => binaryPath({ platform, arch, dir }), /unsupported platform .*supported: darwin-arm64, darwin-x64, linux-x64, win32-x64/);
  }
});

test("argv, cwd and environment reach the client unchanged", { skip: !posix }, async (t) => {
  const { root, bin } = install(t);
  const args = ["--broker-url", "https://example.test/mcp?project=a b", "--token", "asb_p_x", "", "--", "한글 값", "-h=x"];
  const { done } = run(bin, args, { cwd: root, env: { FAKE_MODE: "report", FAKE_PROBE: "env value" }, input: "" });
  const result = await done;
  assert.equal(result.code, 0, result.stderr);
  assert.deepEqual(JSON.parse(result.stdout), { argv: args, cwd: root, probe: "env value" });
  assert.equal(result.stderr, "");
});

test("stdio bytes pass through exactly and EOF ends the client", { skip: !posix }, async (t) => {
  const { bin } = install(t);
  const input = Buffer.concat([crypto.randomBytes(1 << 20), Buffer.from('\n{"jsonrpc":"2.0"}\r\n\0')]);
  const { done } = run(bin, [], { env: { FAKE_MODE: "echo" }, input });
  const result = await done;
  assert.equal(result.code, 0, result.stderr);
  assert.ok(result.stdout.equals(input), "stdout differs from stdin");
  assert.equal(result.stderr, "");
});

test("client exit status becomes the launcher exit status", { skip: !posix }, async (t) => {
  const { bin } = install(t);
  for (const code of [0, 1, 7, 255]) {
    const { done } = run(bin, [], { env: { FAKE_MODE: "report", FAKE_EXIT: String(code) }, input: "" });
    assert.equal((await done).code, code);
  }
});

test("termination signals reach the client and the launcher dies the same way", { skip: !posix }, async (t) => {
  const { root, bin } = install(t);
  for (const signal of ["SIGTERM", "SIGINT", "SIGHUP"]) {
    const pidFile = path.join(root, `${signal}.pid`);
    const { child, done } = run(bin, [], { env: { FAKE_MODE: "wait", FAKE_PID: pidFile } });
    const pid = Number(await waitFor(() => fs.existsSync(pidFile) && fs.readFileSync(pidFile, "utf8")));
    child.kill(signal);
    const result = await done;
    assert.equal(result.signal, signal, `${signal}: ${JSON.stringify(result)}`);
    await waitFor(() => !alive(pid));
  }
});

test("a killed launcher closes the client's stdin so it cannot be orphaned", { skip: !posix }, async (t) => {
  const { root, bin } = install(t);
  const pidFile = path.join(root, "orphan.pid");
  const { child, done } = run(bin, [], { env: { FAKE_MODE: "wait", FAKE_PID: pidFile } });
  const pid = Number(await waitFor(() => fs.existsSync(pidFile) && fs.readFileSync(pidFile, "utf8")));
  child.kill("SIGKILL");
  await done;
  await waitFor(() => !alive(pid));
});

test("host EOF lets the client finish before the launcher exits", { skip: !posix }, async (t) => {
  const { root, bin } = install(t);
  const pidFile = path.join(root, "eof.pid");
  const { child, done } = run(bin, [], { env: { FAKE_MODE: "wait", FAKE_PID: pidFile } });
  const pid = Number(await waitFor(() => fs.existsSync(pidFile) && fs.readFileSync(pidFile, "utf8")));
  child.stdin.end();
  assert.equal((await done).code, 0);
  assert.equal(alive(pid), false);
});

test("a missing bundled client fails on stderr only", { skip: !posix }, async (t) => {
  const { bin } = install(t, { client: false });
  const result = await run(bin, ["--version"], { input: "" }).done;
  assert.equal(result.code, 1);
  assert.equal(result.stdout.length, 0);
  const file = path.join(path.dirname(bin), `${process.platform}-${process.arch}`, "asb-client");
  assert.equal(result.stderr, `x-broker: ${file} is missing; reinstall @xelexis/x-broker@1.2.3\n`);
});

test("a bundled client without execute permission fails on stderr only", { skip: !posix }, async (t) => {
  const { bin } = install(t, { mode: 0o644 });
  const result = await run(bin, ["--version"], { input: "" }).done;
  assert.equal(result.code, 1);
  assert.equal(result.stdout.length, 0);
  const file = path.join(path.dirname(bin), `${process.platform}-${process.arch}`, "asb-client");
  assert.equal(result.stderr, `x-broker: ${file} is not executable; reinstall @xelexis/x-broker@1.2.3\n`);
});

function injectedMain(t, { platform = "darwin", execve } = {}) {
  const { EventEmitter } = require("node:events");
  const { PassThrough } = require("node:stream");
  const proc = Object.assign(new EventEmitter(), { platform, arch: "arm64", argv: ["node", "launcher", "arg one", "", "--flag"], env: { SYNTHETIC: "value" }, stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(), execve, exit: () => assert.fail("unexpected exit"), kill: () => assert.fail("unexpected kill") });
  let stdout = "", stderr = "";
  proc.stdout.on("data", (b) => { stdout += b; }); proc.stderr.on("data", (b) => { stderr += b; });
  const spawns = [];
  const child = Object.assign(new EventEmitter(), { stdin: new PassThrough(), stdout: new PassThrough(), exitCode: null, signalCode: null, kill() {} });
  t.after(() => { for (const stream of [proc.stdin, proc.stdout, proc.stderr, child.stdin, child.stdout]) stream.destroy(); });
  require(launcher).main({ proc, resolveBinary: () => "/synthetic/asb-client", verifyBinary: () => {}, spawnProcess: (...args) => { spawns.push(args); return child; } });
  return { proc, spawns, stdout: () => stdout, stderr: () => stderr };
}

test("POSIX main uses execve with argv0 and env without spawning", (t) => {
  const calls = [];
  const f = injectedMain(t, { execve: (...args) => calls.push(args) });
  assert.deepEqual(calls, [["/synthetic/asb-client", ["/synthetic/asb-client", "arg one", "", "--flag"], f.proc.env]]);
  assert.equal(f.spawns.length, 0);
  assert.equal(f.stdout(), ""); assert.equal(f.stderr(), "");
});

test("execve throw falls back with one fixed stderr line and unchanged spawn pipes", (t) => {
  const f = injectedMain(t, { execve: () => { throw new Error("synthetic-secret-error"); } });
  assert.deepEqual(f.spawns, [["/synthetic/asb-client", ["arg one", "", "--flag"], { stdio: ["pipe", "pipe", "inherit"], windowsHide: true }]]);
  assert.equal(f.stderr(), "x-broker: execve failed; using spawn fallback.\n");
  assert.equal(f.stdout(), "");
});

test("older Node retains spawn fallback without stdout or warnings", (t) => {
  for (const opts of [{ platform: "linux" }]) {
    const f = injectedMain(t, opts);
    assert.equal(f.spawns.length, 1);
    assert.equal(f.stdout(), ""); assert.equal(f.stderr(), "");
  }
});

test("Linux main uses execve and has no launcher stdout", (t) => {
  let called = 0;
  const f = injectedMain(t, { platform: "linux", execve: () => { called++; } });
  assert.equal(called, 1); assert.equal(f.spawns.length, 0); assert.equal(f.stdout(), "");
});

const { EventEmitter } = require("node:events");
const { PassThrough } = require("node:stream");
const digest = (bytes) => crypto.createHash("sha256").update(bytes).digest("hex");

function windowsInstall(t) {
  const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "x-broker-win-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const pkg = path.join(root, "package");
  const source = path.join(pkg, "bin", "win32-x64", "asb-client.exe");
  const local = path.join(root, "Local App Data 한글");
  fs.mkdirSync(path.dirname(source), { recursive: true });
  fs.writeFileSync(source, Buffer.from("synthetic PE client bytes"));
  fs.writeFileSync(path.join(pkg, "package.json"), JSON.stringify({ version: "1.2.3" }));
  const parent = path.join(local, "xelexis", "x-broker", "client-cache", "v1", "win32-x64");
  const target = () => path.join(parent, `1.2.3-${digest(fs.readFileSync(source)).slice(0, 16)}`, "asb-client.exe");
  return { root, pkg, source, local, parent, target };
}

function windowsMain(t, fixture, { fsImpl = fs, env = { LOCALAPPDATA: fixture.local }, onSpawn, sleep } = {}) {
  const proc = Object.assign(new EventEmitter(), {
    platform: "win32", arch: "x64", argv: ["node", "launcher", "--version"], env,
    stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
    exit(code) { this.exits.push(code); }, exits: [], kill(signal) { this.killed = signal; }, pid: 123,
  });
  let stdout = "", stderr = "";
  proc.stdout.on("data", (b) => { stdout += b; });
  proc.stderr.on("data", (b) => { stderr += b; });
  const children = [], spawns = [];
  t.after(() => {
    for (const stream of [proc.stdin, proc.stdout, proc.stderr, ...children.flatMap((c) => [c.stdin, c.stdout])]) stream.destroy();
  });
  require(launcher).main({ proc, fsImpl, sleep, resolveBinary: () => fixture.source, verifyBinary: () => {},
    spawnProcess: (...args) => {
      spawns.push(args);
      const child = Object.assign(new EventEmitter(), { stdin: new PassThrough(), stdout: new PassThrough(), exitCode: null, signalCode: null, kill(signal) { this.killed = signal; } });
      children.push(child);
      onSpawn?.(child, args[0], spawns.length);
      return child;
    },
  });
  return { proc, children, spawns, stdout: () => stdout, stderr: () => stderr };
}

test("Windows main spawns a verified cache outside the package (core RED)", (t) => {
  const fixture = windowsInstall(t);
  const result = windowsMain(t, fixture);
  const spawned = result.spawns[0][0];
  assert.equal(spawned, fixture.target());
  assert.ok(!spawned.startsWith(fixture.pkg + path.sep));
  assert.equal(digest(fs.readFileSync(spawned)), digest(fs.readFileSync(fixture.source)));
  assert.equal(result.stdout(), "");
  assert.equal(result.stderr(), "");
});

const fallbackLine = "x-broker: Windows shadow cache unavailable; using the packaged client. Close running x-broker sessions before upgrading this npm package.\n";
const failure = (code) => Object.assign(new Error(code), { code });
const injectedFS = (overrides) => Object.assign(Object.create(fs), overrides);
function assertFallback(result, fixture) {
  assert.equal(result.spawns[0][0], fixture.source);
  assert.equal(result.stderr(), fallbackLine);
  assert.equal(result.stdout(), "");
  assert.deepEqual(result.proc.exits, []);
}
function cacheCopy(fixture, contents = fs.readFileSync(fixture.source), dir = path.dirname(fixture.target())) {
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, "asb-client.exe"), contents);
  return dir;
}

test("Windows warm hit hashes the cache every time and never writes or cleans", (t) => {
  const f = windowsInstall(t);
  windowsMain(t, f);
  let reads = 0;
  const result = windowsMain(t, f, { fsImpl: injectedFS({
    readFileSync(file, ...args) { if (file === f.target()) reads++; return fs.readFileSync(file, ...args); },
    writeFileSync() { assert.fail("warm cache write"); },
    readdirSync() { assert.fail("warm cache cleanup"); },
  }) });
  result.children[0].emit("spawn");
  assert.equal(result.spawns[0][0], f.target());
  assert.equal(reads, 1);
  assert.equal(result.stderr(), "");
});

test("same version with different packaged bytes gets a different cache directory", (t) => {
  const f = windowsInstall(t);
  const first = windowsMain(t, f).spawns[0][0];
  fs.writeFileSync(f.source, "different bytes");
  const second = windowsMain(t, f).spawns[0][0];
  assert.notEqual(first, second);
  assert.equal(digest(fs.readFileSync(second)), digest(fs.readFileSync(f.source)));
});

test("copy hashes and writes the single source buffer with wx", (t) => {
  const f = windowsInstall(t);
  const original = fs.readFileSync(f.source);
  let reads = 0, writes = 0;
  const result = windowsMain(t, f, { fsImpl: injectedFS({
    readFileSync(file, ...args) {
      const data = fs.readFileSync(file, ...args);
      if (file === f.source) { reads++; fs.writeFileSync(f.source, "changed after read"); }
      return data;
    },
    writeFileSync(file, data, opts) { writes++; assert.equal(opts.flag, "wx"); assert.deepEqual(data, original); return fs.writeFileSync(file, data, opts); },
  }) });
  assert.equal(reads, 1); assert.equal(writes, 1);
  assert.deepEqual(fs.readFileSync(result.spawns[0][0]), original);
  assert.equal(result.stderr(), "");
});

test("publish retries EPERM twice then succeeds without fallback", (t) => {
  const f = windowsInstall(t), waits = [];
  let attempts = 0;
  const result = windowsMain(t, f, { sleep: (ms) => waits.push(ms), fsImpl: injectedFS({
    renameSync(...args) { if (++attempts <= 2) throw failure("EPERM"); return fs.renameSync(...args); },
  }) });
  assert.equal(attempts, 3); assert.deepEqual(waits, [25, 50]);
  assert.equal(result.spawns[0][0], f.target()); assert.equal(result.stderr(), "");
  assert.deepEqual(fs.readdirSync(f.parent), [path.basename(path.dirname(f.target()))]);
});

test("publish retry has a bounded two second wait budget", (t) => {
  const f = windowsInstall(t), waits = [];
  let attempts = 0;
  const result = windowsMain(t, f, { sleep: (ms) => waits.push(ms), fsImpl: injectedFS({
    renameSync() { attempts++; throw failure("EBUSY"); },
  }) });
  assertFallback(result, f);
  assert.equal(attempts, 9);
  assert.equal(waits.reduce((a, b) => a + b, 0), 1975);
  assert.deepEqual(fs.readdirSync(f.parent), []);
});

test("corrupt same-sized cache heals through unlink, rmdir, directory rename", (t) => {
  const f = windowsInstall(t), operations = [];
  cacheCopy(f, Buffer.alloc(fs.statSync(f.source).size, 120));
  const result = windowsMain(t, f, { fsImpl: injectedFS(Object.fromEntries(["unlinkSync", "rmdirSync", "renameSync"].map((method) => [method, (...args) => {
    operations.push([method, ...args]); return fs[method](...args);
  }]))) });
  assert.equal(result.spawns[0][0], f.target()); assert.equal(result.stderr(), "");
  assert.deepEqual(operations.map((o) => o[0]), ["unlinkSync", "rmdirSync", "renameSync"]);
  assert.equal(operations[2][2], path.dirname(f.target()));
  assert.equal(digest(fs.readFileSync(f.target())), digest(fs.readFileSync(f.source)));
});

test("empty final directory heals before publishing", (t) => {
  const f = windowsInstall(t);
  fs.mkdirSync(path.dirname(f.target()), { recursive: true });
  const result = windowsMain(t, f);
  assert.equal(result.spawns[0][0], f.target()); assert.equal(result.stderr(), "");
});

test("locked corrupt exe and failed directory removal each fall back once", (t) => {
  for (const method of ["unlinkSync", "rmdirSync"]) {
    const f = windowsInstall(t); cacheCopy(f, "bad");
    const result = windowsMain(t, f, { fsImpl: injectedFS({ [method](file) {
      if (file === (method === "unlinkSync" ? f.target() : path.dirname(f.target()))) throw failure("EPERM");
      return fs[method](file);
    } }) });
    assertFallback(result, f);
    assert.equal(fs.readdirSync(f.parent).filter((n) => n.includes("-staging-")).length, 0);
  }
});

test("publication race accepts only a full-hash winner and leaves no staging", (t) => {
  for (const valid of [true, false]) {
    const f = windowsInstall(t);
    const result = windowsMain(t, f, { fsImpl: injectedFS({ renameSync() {
      cacheCopy(f, valid ? fs.readFileSync(f.source) : Buffer.alloc(fs.statSync(f.source).size));
      throw failure("EEXIST");
    } }) });
    if (valid) { assert.equal(result.spawns[0][0], f.target()); assert.equal(result.stderr(), ""); }
    else assertFallback(result, f);
    assert.equal(fs.readdirSync(f.parent).filter((n) => n.includes("-staging-")).length, 0);
  }
});

test("a valid winner arriving during staging is reused without unlinking it", (t) => {
  const f = windowsInstall(t);
  const result = windowsMain(t, f, { fsImpl: injectedFS({ writeFileSync(...args) {
    fs.writeFileSync(...args); cacheCopy(f);
  }, unlinkSync(file) { assert.notEqual(file, f.target()); return fs.unlinkSync(file); } }) });
  assert.equal(result.spawns[0][0], f.target()); assert.equal(result.stderr(), "");
});

test("invalid LOCALAPPDATA and invalid versions use one fixed stderr line", (t) => {
  for (const local of [undefined, "", "relative", "package"]) {
    const f = windowsInstall(t);
    assertFallback(windowsMain(t, f, { env: { LOCALAPPDATA: local === "package" ? path.join(f.pkg, "cache") : local } }), f);
  }
  for (const version of ["../../escape", "", "1.2", "01.2.3", "1.2.3/evil"]) {
    const f = windowsInstall(t);
    fs.writeFileSync(path.join(f.pkg, "package.json"), JSON.stringify({ version }));
    assertFallback(windowsMain(t, f), f);
  }
});

test("cache permission, disk-full, write corruption and publication failures fall back", (t) => {
  for (const [method, code] of [["mkdirSync", "EACCES"], ["writeFileSync", "ENOSPC"], ["renameSync", "EINVAL"]]) {
    const f = windowsInstall(t);
    assertFallback(windowsMain(t, f, { fsImpl: injectedFS({ [method]() { throw failure(code); } }) }), f);
  }
  const f = windowsInstall(t);
  assertFallback(windowsMain(t, f, { fsImpl: injectedFS({ writeFileSync(file) { fs.writeFileSync(file, "corrupt write"); } }) }), f);
});

test("missing, nonregular, empty, oversized and unreadable source are fatal even with a cache", (t) => {
  for (const mode of ["missing", "directory", "empty", "oversized", "unreadable", "symlink"]) {
    const f = windowsInstall(t); const cached = path.join(cacheCopy(f), "asb-client.exe");
    let fsImpl = fs;
    if (mode === "missing" || mode === "directory" || mode === "symlink") fs.unlinkSync(f.source);
    if (mode === "directory") fs.mkdirSync(f.source);
    if (mode === "symlink") {
      try { fs.symlinkSync(cached, f.source); } catch (error) {
        if (process.platform === "win32" && error.code === "EPERM") { t.skip("Windows symlink privilege is unavailable"); return; }
        throw error;
      }
    }
    if (mode === "empty") fs.writeFileSync(f.source, "");
    if (mode === "oversized") fsImpl = injectedFS({ lstatSync(file) { return file === f.source ? { isFile: () => true, size: 256 * 1024 * 1024 + 1 } : fs.lstatSync(file); } });
    if (mode === "unreadable") fsImpl = injectedFS({ readFileSync(file, ...args) { if (file === f.source) throw failure("EACCES"); return fs.readFileSync(file, ...args); } });
    const result = windowsMain(t, f, { fsImpl });
    assert.equal(result.spawns.length, 0, mode); assert.deepEqual(result.proc.exits, [1]);
    assert.match(result.stderr(), /packaged client is damaged; reinstall/); assert.equal(result.stdout(), "");
  }
});

test("fallback refuses a changed source after its one recheck", (t) => {
  const f = windowsInstall(t);
  let reads = 0;
  const result = windowsMain(t, f, { env: {}, fsImpl: injectedFS({ readFileSync(file, ...args) {
    if (file === f.source && ++reads > 1) return Buffer.alloc(fs.statSync(file).size);
    return fs.readFileSync(file, ...args);
  } }) });
  assert.equal(reads, 3); assert.equal(result.spawns.length, 0);
  assert.deepEqual(result.proc.exits, [1]); assert.match(result.stderr(), /changed or is damaged/);
});

test("cache directory links are neither executed nor removed", (t) => {
  const f = windowsInstall(t), outside = path.join(f.root, "outside");
  fs.mkdirSync(outside); fs.writeFileSync(path.join(outside, "asb-client.exe"), fs.readFileSync(f.source));
  fs.mkdirSync(f.parent, { recursive: true });
  fs.symlinkSync(outside, path.dirname(f.target()), "junction");
  assertFallback(windowsMain(t, f), f);
  assert.ok(fs.existsSync(path.join(outside, "asb-client.exe")));
});

test("LOCALAPPDATA resolving into the package falls back", (t) => {
  const f = windowsInstall(t);
  fs.symlinkSync(f.pkg, f.local, "junction");
  assertFallback(windowsMain(t, f), f);
});

test("ENOENT retries preparation once, removes old handlers and ignores failed child close", (t) => {
  const f = windowsInstall(t);
  const result = windowsMain(t, f);
  const failedChild = result.children[0];
  fs.unlinkSync(f.target()); fs.rmdirSync(path.dirname(f.target()));
  failedChild.emit("error", failure("ENOENT")); failedChild.emit("close", -2, null);
  assert.equal(result.spawns.length, 2);
  assert.equal(result.spawns[1][0], f.target()); assert.ok(fs.existsSync(f.target()));
  assert.deepEqual(result.proc.exits, []); assert.equal(result.stderr(), "");
  assert.equal(result.proc.listenerCount("SIGTERM"), 1);
  result.children[1].emit("error", failure("ENOENT"));
  assert.equal(result.spawns.length, 2); assert.deepEqual(result.proc.exits, [1]);
});

test("EACCES, EPERM and post-spawn exit never rerun from another path", (t) => {
  for (const code of ["EACCES", "EPERM"]) {
    const f = windowsInstall(t), result = windowsMain(t, f);
    result.children[0].emit("error", failure(code));
    assert.equal(result.spawns.length, 1); assert.deepEqual(result.proc.exits, [1]);
    assert.match(result.stderr(), /cannot start/); assert.ok(!result.stderr().includes(fallbackLine));
  }
});

test("Windows argv/env, child stdout, pipes, signals and exit behavior remain intact", async (t) => {
  const f = windowsInstall(t), result = windowsMain(t, f);
  assert.deepEqual(result.spawns[0].slice(1), [["--version"], { stdio: ["pipe", "pipe", "inherit"], windowsHide: true }]);
  assert.equal(result.proc.env.LOCALAPPDATA, f.local);
  result.children[0].stdout.write("child version\n");
  assert.equal(result.stdout(), "child version\n");
  result.proc.emit("SIGBREAK", "SIGBREAK"); assert.equal(result.children[0].killed, "SIGBREAK");
  result.children[0].emit("close", 7, null);
  await new Promise(setImmediate);
  assert.deepEqual(result.proc.exits, [7]); assert.equal(result.spawns.length, 1);
});

function agedCopy(f, name, hours) {
  const dir = cacheCopy(f, "old", path.join(f.parent, name));
  const at = new Date(Date.now() - hours * 3600000);
  fs.utimesSync(dir, at, at);
  return dir;
}

test("new cache cleans only old recognized entries after spawn, skips locked/current/recent/unknown", (t) => {
  const f = windowsInstall(t);
  const old = agedCopy(f, "0.9.0-aaaaaaaaaaaaaaaa", 24 * 8);
  const prerelease = agedCopy(f, "0.9.0-staging-aaaaaaaaaaaaaaaa", 24 * 8);
  const locked = agedCopy(f, "0.9.1-bbbbbbbbbbbbbbbb", 24 * 8);
  const recent = agedCopy(f, "0.9.2-cccccccccccccccc", 24 * 6);
  const unknown = agedCopy(f, "unrelated", 24 * 9);
  const staleStage = agedCopy(f, "1.0.0-dddddddddddddddd-staging-ABC123", 2);
  const liveStage = agedCopy(f, "1.0.0-dddddddddddddddd-staging-DEF456", 0.5);
  const result = windowsMain(t, f, { fsImpl: injectedFS({ unlinkSync(file) {
    if (file === path.join(locked, "asb-client.exe")) throw failure("EBUSY");
    return fs.unlinkSync(file);
  } }) });
  assert.ok(fs.existsSync(old), "cleanup must wait for spawn success");
  result.children[0].emit("spawn");
  assert.equal(fs.existsSync(old), false); assert.equal(fs.existsSync(staleStage), false);
  assert.equal(fs.existsSync(prerelease), false);
  for (const dir of [locked, recent, unknown, liveStage, path.dirname(f.target())]) assert.ok(fs.existsSync(dir), dir);
  assert.equal(result.stderr(), "");
});

test("cleanup attempts at most sixteen entries, including locked ones", (t) => {
  const f = windowsInstall(t);
  for (let i = 0; i < 20; i++) agedCopy(f, `0.0.${i}-aaaaaaaaaaaaaaaa`, 24 * 8);
  let unlinks = 0;
  const result = windowsMain(t, f, { fsImpl: injectedFS({ unlinkSync() { unlinks++; throw failure("EPERM"); } }) });
  result.children[0].emit("spawn");
  assert.equal(unlinks, 16); assert.equal(result.stderr(), "");
});

test("cache uses publication age, not old source age; failed spawn never cleans", (t) => {
  const f = windowsInstall(t);
  const at = new Date(Date.now() - 30 * 86400000); fs.utimesSync(f.source, at, at);
  const old = agedCopy(f, "0.9.0-aaaaaaaaaaaaaaaa", 24 * 8);
  const result = windowsMain(t, f);
  assert.ok(Date.now() - fs.statSync(path.dirname(f.target())).mtimeMs < 10000);
  result.children[0].emit("error", failure("EACCES"));
  assert.ok(fs.existsSync(old));
});

test("POSIX execve and spawn fallback make zero injected cache fs calls", (t) => {
  for (const platform of ["darwin", "linux"]) for (const execve of [undefined, () => {}]) {
    const io = new Proxy({}, { get() { assert.fail("POSIX cache fs access"); } });
    const proc = Object.assign(new EventEmitter(), { platform, arch: "x64", argv: ["node", "launcher"], env: {}, stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(), execve, exit() {} });
    const child = Object.assign(new EventEmitter(), { stdin: new PassThrough(), stdout: new PassThrough() });
    require(launcher).main({ proc, fsImpl: io, resolveBinary: () => "/synthetic/client", verifyBinary() {}, spawnProcess: () => child });
    t.after(() => { for (const stream of [proc.stdin, proc.stdout, proc.stderr, child.stdin, child.stdout]) stream.destroy(); });
  }
});

test("two real concurrent cold starts publish one verified cache with no staging leftovers", async (t) => {
  const f = windowsInstall(t);
  // Both child processes wait for the same IPC start, then use real fs operations.
  const driver = `
    const {EventEmitter}=require('node:events'); const {PassThrough}=require('node:stream');
    process.send('ready'); process.once('message', () => {
      const proc=Object.assign(new EventEmitter(), {platform:'win32',arch:'x64',argv:['node','launcher'],env:{LOCALAPPDATA:process.argv[2]},stdin:new PassThrough(),stdout:new PassThrough(),stderr:new PassThrough(),exit:code=>process.exit(code)});
      proc.stderr.on('data', b=>process.stderr.write(b));
      require(process.argv[1]).main({proc,resolveBinary:()=>process.argv[3],spawnProcess:file=>{
        process.send(file); return Object.assign(new EventEmitter(),{stdin:new PassThrough(),stdout:new PassThrough(),exitCode:null,signalCode:null});
      }});
      process.disconnect();
    });`;
  const workers = [0, 1].map(() => spawn(process.execPath, ["-e", driver, launcher, f.local, f.source], { stdio: ["ignore", "pipe", "pipe", "ipc"] }));
  t.after(() => { for (const worker of workers) if (worker.exitCode === null) worker.kill(); });
  const paths = [], errors = ["", ""];
  const done = workers.map((worker, i) => new Promise((resolve) => {
    worker.stderr.on("data", (b) => { errors[i] += b; });
    worker.on("message", (message) => { if (message !== "ready") paths.push(message); });
    worker.on("close", resolve);
  }));
  await Promise.all(workers.map((worker) => new Promise((resolve) => worker.once("message", resolve))));
  for (const worker of workers) worker.send("go");
  assert.deepEqual(await Promise.all(done), [0, 0]);
  assert.deepEqual(paths, [f.target(), f.target()]); assert.deepEqual(errors, ["", ""]);
  assert.equal(digest(fs.readFileSync(f.target())), digest(fs.readFileSync(f.source)));
  assert.deepEqual(fs.readdirSync(f.parent), [path.basename(path.dirname(f.target()))]);
});

test("ENOENT emitted after spawn success never re-prepares or reruns", (t) => {
  const f = windowsInstall(t), result = windowsMain(t, f);
  result.children[0].emit("spawn");
  result.children[0].emit("error", failure("ENOENT"));
  assert.equal(result.spawns.length, 1);
  assert.deepEqual(result.proc.exits, [1]);
});

test("cleanup preserves unknown staging names and linked cache files/directories", (t) => {
  const f = windowsInstall(t);
  const unrelated = agedCopy(f, "unrelated-staging-ABC123", 24 * 9);
  const outside = path.join(f.root, "external"); fs.mkdirSync(outside);
  const externalFile = path.join(outside, "asb-client.exe"); fs.writeFileSync(externalFile, "external");
  const linkedDir = path.join(f.parent, "0.8.0-aaaaaaaaaaaaaaaa"); fs.symlinkSync(outside, linkedDir, "junction");
  const linkedFileDir = path.join(f.parent, "0.8.1-aaaaaaaaaaaaaaaa"); fs.mkdirSync(linkedFileDir);
  try { fs.symlinkSync(externalFile, path.join(linkedFileDir, "asb-client.exe")); } catch (error) {
    if (process.platform === "win32" && error.code === "EPERM") { t.skip("Windows symlink privilege is unavailable"); return; }
    throw error;
  }
  const age = new Date(Date.now() - 9 * 86400000); fs.utimesSync(linkedFileDir, age, age);
  const result = windowsMain(t, f); result.children[0].emit("spawn");
  for (const dir of [unrelated, linkedDir, linkedFileDir]) assert.ok(fs.existsSync(dir), dir);
  assert.equal(fs.readFileSync(externalFile, "utf8"), "external");
});
