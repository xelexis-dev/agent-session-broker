#!/usr/bin/env node
"use strict";
// x-broker plugin launcher. Installs the pinned @xelexis/x-broker once with npm into a
// per-version directory that only this user can reach, then runs the package's own
// launcher in this process: POSIX execve leaves only the client. Nothing goes to stdout.
const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const PIN = /^@xelexis\/x-broker@((?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*))$/;
const MARKER = ".x-broker-complete";
const WIN = process.platform === "win32";
const NPM_TIMEOUT_MS = 300000; // hard bound on POSIX; hosts give up after 30 s by default
// Pin what npm does and where, whatever npm_config_* or .npmrc say. Registry, scope
// registry, proxy, CA, auth, cache and offline settings stay the user's, as with npx.
const NPM_ARGS = ["install", "--prefix=.", "--global=false", "--location=project", "--package-lock-only=false",
  "--dry-run=false", "--install-strategy=hoisted", "--global-style=false", "--legacy-bundling=false",
  "--workspaces=false", "--ignore-scripts=true", "--audit=false", "--fund=false", "--loglevel=error"];

class Refusal extends Error {}

// POSIX: a real directory or file (lstat: never a symlink), owned by this user, without
// the forbidden mode bits. Windows: real directory or file only; access rests on the
// per-user ACL of %LOCALAPPDATA%.
function check(target, kind, forbidden, follow = false) {
  const st = (follow ? fs.statSync : fs.lstatSync)(target);
  const right = kind === "dir" ? st.isDirectory() : st.isFile();
  if (!right || (!WIN && (st.uid !== process.getuid() || (st.mode & forbidden) !== 0))) {
    throw new Refusal(`unsafe client cache ${target}: it must be a real ${kind === "dir" ? "directory" : "file"} owned by you that group and others cannot ${forbidden === 0o077 ? "open" : "write"}; fix it or delete it`);
  }
}

// Trust root: the home directory (POSIX) or %LOCALAPPDATA% (Windows). Each level below is
// created 0700 if missing and checked before the next, so no other user can rename any
// entry on the path afterwards and nothing is created inside an unchecked directory.
function trustedRoot() {
  let dir = WIN ? process.env.LOCALAPPDATA : os.homedir();
  if (!dir || !path.isAbsolute(dir)) throw new Refusal("no per-user cache directory (LOCALAPPDATA or HOME)");
  try { check(dir, "dir", 0o022, true); } catch (err) {
    throw err instanceof Refusal ? new Refusal(`unsafe cache trust root ${dir}: it must be a directory owned by you that group and others cannot write`) : err;
  }
  for (const [name, forbidden] of [...(WIN ? [] : [[".cache", 0o022]]), ["xelexis", 0o022], ["x-broker", 0o022], ["npm", 0o077]]) {
    dir = path.join(dir, name);
    try { fs.mkdirSync(dir, { mode: 0o700 }); } catch (err) { if (err.code !== "EEXIST") throw err; }
    check(dir, "dir", forbidden);
  }
  return dir;
}

// The package's launcher and this platform's client, both inside dir.
function contents(dir, version) {
  const pkg = path.join(dir, "node_modules", "@xelexis", "x-broker");
  const bin = path.join(pkg, "bin", "x-broker.js");
  const inside = (file) => fs.realpathSync(file).startsWith(fs.realpathSync(dir) + path.sep);
  if (JSON.parse(fs.readFileSync(path.join(pkg, "package.json"), "utf8")).version !== version || !inside(bin)) throw new Error("package");
  const launcher = require(bin);
  let client;
  try { client = launcher.binaryPath(); } catch (err) { throw new Refusal(err.message); } // unsupported platform
  if (typeof launcher.main !== "function" || !inside(client) || !fs.statSync(client).isFile()) throw new Error("client");
  return { launcher, bin };
}

// Complete = a safe directory whose marker (written last, before publication) names the
// version, plus contents(). Anything else is outside damage and is never replaced here.
function complete(dir, version) {
  check(dir, "dir", 0o077);
  try {
    check(path.join(dir, MARKER), "file", 0o022);
    if (fs.readFileSync(path.join(dir, MARKER), "utf8") !== `${version}\n`) throw new Error("marker");
    return contents(dir, version);
  } catch (err) {
    if (err instanceof Refusal) throw err;
    throw new Refusal(`incomplete client cache ${dir}; delete this directory and restart`);
  }
}

// Install into a private staging directory, mark it complete, publish it with one rename.
// A session that published first wins; a kill leaves only a staging directory.
function install(root, final, version, timeout = NPM_TIMEOUT_MS) {
  const staging = fs.mkdtempSync(path.join(root, `${version}-staging-`));
  try {
    fs.writeFileSync(path.join(staging, "package.json"), JSON.stringify({ private: true, dependencies: { "@xelexis/x-broker": version } }));
    // npm never needs broker settings; Windows env keys are case-insensitive.
    const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.toUpperCase().startsWith("AGENT_SESSION_BROKER_")));
    // No pipes: spawnSync waits only for npm itself, and SIGKILL cannot be ignored (POSIX).
    // --ignore-scripts starts no lifecycle children. Windows: shell kills cmd only (best effort).
    const run = spawnSync("npm", NPM_ARGS, { cwd: staging, env, stdio: ["ignore", 2, 2], shell: WIN, timeout, killSignal: "SIGKILL" }); // fixed args only, safe for cmd
    if (run.status !== 0) throw new Refusal(`npm install @xelexis/x-broker@${version} failed${run.error ? ` (${run.error.code})` : ""}`);
    try { contents(staging, version); } catch (err) { throw err instanceof Refusal ? err : new Refusal(`npm install @xelexis/x-broker@${version} left an incomplete package`); }
    fs.writeFileSync(path.join(staging, MARKER), `${version}\n`, { flag: "wx", mode: 0o600 });
    try { fs.renameSync(staging, final); } catch (err) { if (!["EEXIST", "ENOTEMPTY", "EPERM", "EACCES"].includes(err.code)) throw err; }
  } finally {
    try { fs.rmSync(staging, { recursive: true, force: true }); } catch { /* left for manual removal */ }
  }
}

function exists(file) {
  try { fs.lstatSync(file); return true; } catch (err) { if (err.code === "ENOENT") return false; throw err; }
}

function main() {
  const [, , pin, ...args] = process.argv;
  let ready;
  try {
    const match = PIN.exec(pin || "");
    if (!match) throw new Refusal("the first argument must be @xelexis/x-broker@<x.y.z>");
    const root = trustedRoot();
    const final = path.join(root, match[1]);
    if (!exists(final)) install(root, final, match[1]);
    if (!exists(final)) throw new Refusal(`could not publish ${final}; restart to retry`);
    ready = complete(final, match[1]);
  } catch (err) {
    process.stderr.write(`x-broker plugin: ${err instanceof Refusal ? err.message : `cannot prepare the client (${err.code || err.message})`}\n`);
    return process.exit(1);
  }
  process.argv = [process.argv[0], ready.bin, ...args];
  ready.launcher.main(); // the package launcher: execve on POSIX, verified relay on Windows
}

if (require.main === module) main();
module.exports = { NPM_ARGS, MARKER, Refusal, check, trustedRoot, contents, complete, install, main };
