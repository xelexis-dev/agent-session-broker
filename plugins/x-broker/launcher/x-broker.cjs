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

function directorySettings(accountHome) {
  const file = path.join(accountHome, ".config", "x-broker", "claude-plugin.json");
  const unsafe = () => new Refusal(`unsafe directory settings ${file}: it must be a regular file owned by you that group and others cannot open; fix it or delete it`);
  const safe = (st, kind, forbidden) => (kind === "dir" ? st.isDirectory() : st.isFile()) &&
    (WIN || (Number(st.uid) === process.getuid() && (Number(st.mode) & forbidden) === 0));
  let fd, present = false;
  try {
    // An absent opt-in file must not change startup, even through dotfile links.
    const before = fs.lstatSync(file, { bigint: true });
    present = true;
    for (const dir of [path.dirname(path.dirname(file)), path.dirname(file)]) {
      if (!safe(fs.lstatSync(dir), "dir", 0o022)) throw unsafe();
    }
    if (!safe(before, "file", 0o077)) throw unsafe();
    fd = fs.openSync(file, fs.constants.O_RDONLY | (fs.constants.O_NOFOLLOW || 0) | (fs.constants.O_NONBLOCK || 0));
    const opened = fs.fstatSync(fd, { bigint: true });
    if (!safe(opened, "file", 0o077) || opened.dev !== before.dev || opened.ino !== before.ino) throw unsafe();
    const buffer = Buffer.alloc(65537);
    let size = 0, count;
    while (size < buffer.length && (count = fs.readSync(fd, buffer, size, buffer.length - size, null)) > 0) size += count;
    if (size > 65536) throw new Refusal(`${file} is not valid directory settings JSON`);
    let entries;
    // shortcut: duplicate literal JSON keys use the last user-approved pair; use a strict parser if the format gains untrusted writers.
    try { entries = JSON.parse(buffer.toString("utf8", 0, size)); }
    catch { throw new Refusal(`${file} is not valid directory settings JSON`); }
    const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
    if (!object(entries)) throw new Refusal(`${file} must map absolute directory paths to settings objects`);
    const canonical = new Map();
    for (const [dir, entry] of Object.entries(entries)) {
      if (!path.isAbsolute(dir) || !object(entry)) throw new Refusal(`${file} must map absolute directory paths to settings objects`);
      if (Object.hasOwn(entry, "token")) throw new Refusal(`${file} must not hold a token; put the token in a private file and name it in token_file`);
      if (Object.keys(entry).some(key => !["broker_url", "token_file", "tool_profile"].includes(key))) throw new Refusal(`settings for ${dir} have an unknown key; allowed: broker_url, token_file, tool_profile`);
      if (typeof entry.broker_url !== "string" || !entry.broker_url || typeof entry.token_file !== "string" || !entry.token_file) throw new Refusal(`settings for ${dir} need both broker_url and token_file`);
      if (!path.isAbsolute(entry.token_file)) throw new Refusal(`token_file for ${dir} must be an absolute path`);
      if (Object.hasOwn(entry, "tool_profile") && !["full", "worker"].includes(entry.tool_profile)) throw new Refusal(`tool_profile for ${dir} must be full or worker`);
      let real;
      try { real = fs.realpathSync.native(dir); if (!fs.statSync(real).isDirectory()) continue; }
      catch (err) { if (["ENOENT", "ENOTDIR"].includes(err.code)) continue; throw unsafe(); }
      if (canonical.has(real)) throw new Refusal(`two entries in ${file} name the same directory ${dir}`);
      canonical.set(real, entry);
    }
    return canonical;
  } catch (err) {
    if (err.code === "ENOENT" && !present) return null;
    throw err instanceof Refusal ? err : unsafe();
  } finally { if (fd !== undefined) fs.closeSync(fd); }
}

function claudeArguments(args, accountHome) {
  if (!args.includes("--claude-project-dir")) return args;
  // shortcut: accept only our manifest's shape; update this check when the manifest changes.
  if (args.length !== 6 || args[0] !== "--claude-project-dir" || args[2] !== "--broker-url" || args[4] !== "--tool-profile") throw new Refusal("unexpected launcher arguments");
  process.env.AGENT_SESSION_BROKER_PLUGIN_SOURCE = "userConfig";
  const entries = directorySettings(accountHome);
  if (entries === null) return args.slice(2);
  const project = path.isAbsolute(args[1]) ? args[1] : process.env.CLAUDE_PROJECT_DIR;
  let real;
  try {
    if (!project || !path.isAbsolute(project)) throw new Error();
    real = fs.realpathSync.native(project);
    if (!fs.statSync(real).isDirectory()) throw new Error();
  } catch { throw new Refusal("Claude Code gave no usable project directory; refusing directory settings"); }
  let selected;
  for (const [dir, entry] of entries) {
    if ((real === dir || real.startsWith(dir.endsWith(path.sep) ? dir : dir + path.sep)) && (!selected || dir.length > selected.dir.length)) selected = { dir, entry };
  }
  if (!selected) return args.slice(2);
  for (const key of Object.keys(process.env)) {
    if (["AGENT_SESSION_BROKER_TOKEN", "AGENT_SESSION_BROKER_URL", "AGENT_SESSION_BROKER_TOKEN_FILE"].includes(key.toUpperCase())) delete process.env[key];
  }
  process.env.AGENT_SESSION_BROKER_PLUGIN_SOURCE = "directory";
  process.stderr.write(`x-broker plugin: directory settings for ${selected.dir.replace(/[\r\n]/g, "?")}\n`);
  return ["--broker-url", selected.entry.broker_url, "--token-file", selected.entry.token_file, "--tool-profile", selected.entry.tool_profile || args[5]];
}

function main({ accountHome = os.userInfo().homedir } = {}) {
  const [, , pin, ...originalArgs] = process.argv;
  let args;
  let ready;
  try {
    const match = PIN.exec(pin || "");
    if (!match) throw new Refusal("the first argument must be @xelexis/x-broker@<x.y.z>");
    args = claudeArguments(originalArgs, accountHome);
    const root = trustedRoot();
    const final = path.join(root, match[1]);
    if (!exists(final)) install(root, final, match[1]);
    if (!exists(final)) throw new Refusal(`could not publish ${final}; restart to retry`);
    ready = complete(final, match[1]);
  } catch (err) {
    process.stderr.write(`x-broker plugin: ${err instanceof Refusal ? err.message.replace(/[\r\n]/g, "?") : `cannot prepare the client (${err.code || err.message})`}\n`);
    return process.exit(1);
  }
  process.argv = [process.argv[0], ready.bin, ...args];
  ready.launcher.main(); // the package launcher: execve on POSIX, verified relay on Windows
}

if (require.main === module) main();
module.exports = { NPM_ARGS, MARKER, Refusal, check, trustedRoot, contents, complete, install, main };
