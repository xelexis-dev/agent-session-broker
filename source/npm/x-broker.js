#!/usr/bin/env node
"use strict";

// npm launcher for asb-client. It writes nothing to stdout (the MCP stdio
// channel). POSIX execve replaces this process and inherits the host's stdio
// pipes, so host EOF ends the client. The fallback relays through pipes: killing
// the launcher closes the client's stdin so it cannot outlive its host.
// Diagnostics go to stderr only.
const { spawn } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");
const { createHash } = require("node:crypto");

const binaries = {
  "darwin-arm64": "asb-client",
  "darwin-x64": "asb-client",
  "linux-x64": "asb-client",
  "win32-x64": "asb-client.exe",
};

// Every supported client is bundled under bin/<platform>/ next to this file.
function binaryPath({ platform = process.platform, arch = process.arch, dir = __dirname } = {}) {
  const key = `${platform}-${arch}`;
  if (!Object.hasOwn(binaries, key)) {
    throw new Error(`unsupported platform ${key}; supported: ${Object.keys(binaries).join(", ")}`);
  }
  return path.join(dir, key, binaries[key]);
}

// A missing or non-executable client means a damaged installation; say so
// before spawning so stdout stays empty.
function checkBinary(file) {
  const { name, version } = require("../package.json");
  const reinstall = `reinstall ${name}@${version}`;
  if (!fs.statSync(file, { throwIfNoEntry: false })?.isFile()) throw new Error(`${file} is missing; ${reinstall}`);
  if (process.platform !== "win32") {
    try {
      fs.accessSync(file, fs.constants.X_OK);
    } catch {
      throw new Error(`${file} is not executable; ${reinstall}`);
    }
  }
}

const maxBinarySize = 256 * 1024 * 1024;
const semver = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(?:\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$/;
const cacheEntry = /^(.+)-[a-f0-9]{16}(-staging-[A-Za-z0-9]+)?$/;
const fallbackWarning = "x-broker: Windows shadow cache unavailable; using the packaged client. Close running x-broker sessions before upgrading this npm package.\n";
const hash = (bytes) => createHash("sha256").update(bytes).digest("hex");
const pause = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);

function binaryBytes(file, io) {
  const stat = io.lstatSync(file);
  if (!stat.isFile() || stat.size <= 0 || stat.size > maxBinarySize) throw new Error("invalid client file");
  const bytes = io.readFileSync(file);
  if (bytes.length !== stat.size || bytes.length > maxBinarySize) throw new Error("client changed while reading");
  return bytes;
}

function validCache(file, expected, io) {
  try {
    if (!io.lstatSync(path.dirname(file)).isDirectory()) return false;
    return hash(binaryBytes(file, io)) === expected;
  } catch { return false; }
}

function removeCopy(dir, io) {
  // Never follow a link or recursively delete an unexpected cache entry.
  if (!io.lstatSync(dir).isDirectory()) throw new Error("invalid cache directory");
  try {
    const file = path.join(dir, "asb-client.exe");
    if (!io.lstatSync(file).isFile()) throw new Error("invalid cache file");
    io.unlinkSync(file);
  } catch (err) { if (err.code !== "ENOENT") throw err; }
  io.rmdirSync(dir);
}

function cleanupCache(parent, current, io) {
  try {
    let attempts = 0;
    for (const name of io.readdirSync(parent)) {
      if (attempts >= 16) break;
      const dir = path.join(parent, name);
      if (dir === current) continue;
      const match = cacheEntry.exec(name);
      if (!match || match[1].length > 100 || !semver.test(match[1])) continue;
      const staging = Boolean(match[2]);
      try {
        const stat = io.lstatSync(dir);
        const age = Date.now() - stat.mtimeMs;
        if (!stat.isDirectory() || age <= (staging ? 3600000 : 7 * 86400000)) continue;
        attempts++;
        removeCopy(dir, io);
      } catch { /* A running image may still lock this entry. */ }
    }
  } catch { /* Cache housekeeping must not stop the client. */ }
}

function prepareWindowsBinary(source, env, io, sleep) {
  let bytes;
  try { bytes = binaryBytes(source, io); }
  catch { throw new Error("packaged client is damaged; reinstall @xelexis/x-broker"); }
  const expected = hash(bytes);
  const packageDir = path.resolve(path.dirname(source), "..", "..");
  const insidePackage = (candidate, pkg) => {
    const relative = path.relative(pkg, candidate);
    return relative === "" || (!relative.startsWith(`..${path.sep}`) && relative !== ".." && !path.isAbsolute(relative));
  };
  let staging;
  try {
    const local = env.LOCALAPPDATA;
    if (!local || !path.isAbsolute(local) || insidePackage(path.resolve(local), packageDir)) throw new Error("invalid cache root");
    const { version } = JSON.parse(io.readFileSync(path.join(packageDir, "package.json"), "utf8"));
    if (typeof version !== "string" || version.length > 100 || !semver.test(version)) throw new Error("invalid package version");
    const parent = path.join(local, "xelexis", "x-broker", "client-cache", "v1", "win32-x64");
    io.mkdirSync(parent, { recursive: true });
    if (insidePackage(io.realpathSync(parent), io.realpathSync(packageDir))) throw new Error("cache inside package");
    const finalDir = path.join(parent, `${version}-${expected.slice(0, 16)}`);
    const file = path.join(finalDir, "asb-client.exe");
    if (validCache(file, expected, io)) return { file };
    staging = io.mkdtempSync(path.join(parent, `${version}-${expected.slice(0, 16)}-staging-`));
    io.writeFileSync(path.join(staging, "asb-client.exe"), bytes, { flag: "wx" });
    if (!validCache(path.join(staging, "asb-client.exe"), expected, io)) throw new Error("cache write mismatch");
    // Windows cannot replace a directory; remove only a corrupt copy, once.
    try {
      io.lstatSync(finalDir);
      if (validCache(file, expected, io)) return { file };
      removeCopy(finalDir, io);
    } catch (err) { if (err.code !== "ENOENT") throw err; }
    for (let attempt = 0; ; attempt++) {
      try {
        io.renameSync(staging, finalDir);
        staging = undefined;
        // The directory mtime is publication time, not the source file's age.
        const now = new Date();
        io.utimesSync(finalDir, now, now);
        if (!validCache(file, expected, io)) throw new Error("published cache mismatch");
        return { file, cleanup: () => cleanupCache(parent, finalDir, io) };
      } catch (err) {
        if (validCache(file, expected, io)) return { file };
        // 25+50+100+200+400+400+400+400 = 1975 ms maximum wait.
        if (!["EACCES", "EPERM", "EBUSY"].includes(err.code) || attempt >= 8) throw err;
        sleep(Math.min(25 * 2 ** attempt, 400));
      }
    }
  } catch {
    // Fallback is allowed only while the packaged bytes still match our source.
    for (let attempt = 0; ; attempt++) {
      try {
        if (hash(binaryBytes(source, io)) === expected) return { file: source, fallback: true };
      } catch { /* Recheck once if an upgrade changed the installation. */ }
      if (attempt === 1) throw new Error("packaged client changed or is damaged; reinstall @xelexis/x-broker");
    }
  } finally {
    if (staging) { try { removeCopy(staging, io); } catch {} }
  }
}

function main({ proc = process, spawnProcess = spawn, resolveBinary = binaryPath, verifyBinary = checkBinary, fsImpl = fs, sleep = pause } = {}) {
  let file, prepared, source, warned = false;
  const prepare = () => {
    prepared = prepareWindowsBinary(source, proc.env, fsImpl, sleep);
    file = prepared.file;
    if (prepared.fallback && !warned) { proc.stderr.write(fallbackWarning); warned = true; }
  };
  try {
    source = file = resolveBinary({ platform: proc.platform, arch: proc.arch });
    if (proc.platform === "win32") prepare();
    else verifyBinary(file);
  } catch (err) {
    proc.stderr.write(`x-broker: ${err.message}\n`);
    return proc.exit(1);
  }
  if (proc.platform !== "win32" && typeof proc.execve === "function") {
    try {
      proc.execve(file, [file, ...proc.argv.slice(2)], proc.env);
      return;
    } catch {
      proc.stderr.write("x-broker: execve failed; using spawn fallback.\n");
    }
  }
  const start = (retried = false) => {
    let child, started = false;
    const failed = (err) => {
      if (proc.platform === "win32" && err.code === "ENOENT" && !retried && !started) {
        try { prepare(); return start(true); }
        catch (error) { err = error; }
      }
      proc.stderr.write(`x-broker: cannot start ${file}: ${err.message}\n`);
      proc.exit(1);
    };
    try { child = spawnProcess(file, proc.argv.slice(2), { stdio: ["pipe", "pipe", "inherit"], windowsHide: true }); }
    catch (err) { failed(err); return; }
    const signals = proc.platform === "win32" ? ["SIGINT", "SIGTERM", "SIGBREAK"] : ["SIGINT", "SIGTERM", "SIGHUP"];
    const forward = (signal) => {
      if (child.exitCode === null && child.signalCode === null) child.kill(signal);
    };
    for (const signal of signals) proc.on(signal, forward);
    proc.stdin.pipe(child.stdin);
    child.stdout.pipe(proc.stdout, { end: false });
    child.stdin.on("error", () => {});
    const stdoutError = () => forward("SIGTERM");
    proc.stdout.on("error", stdoutError);
    const detach = () => {
      proc.stdin.unpipe(child.stdin);
      child.stdout.unpipe(proc.stdout);
      for (const s of signals) proc.removeListener(s, forward);
      proc.stdout.removeListener("error", stdoutError);
    };
    let errored = false;
    child.once("spawn", () => { started = true; prepared?.cleanup?.(); });
    child.on("error", (err) => {
      errored = true;
      detach();
      failed(err);
    });
    child.on("close", (code, signal) => {
      if (errored) return;
      detach();
      proc.stdin.destroy();
      // Flush relayed output before mirroring the client's exit.
      proc.stdout.write("", () => {
        if (signal) proc.kill(proc.pid, signal);
        proc.exit(code ?? 1);
      });
    });
  };
  start();
}

if (require.main === module) main();
module.exports = { binaryPath, main };
