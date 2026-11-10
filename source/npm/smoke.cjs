#!/usr/bin/env node
"use strict";

// End-to-end check of the packed tarball on the current platform, offline and
// without publishing: install it with npm, run the real bundled asb-client
// through the launcher and npx, and exercise MCP against a loopback asb-server.
//
//   node smoke.cjs --packages <dir with .tgz> --version 1.2.3 --server <asb-server> [--name @xelexis/x-broker]
const { execFileSync, spawn, spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const readline = require("node:readline");

const args = Object.fromEntries(process.argv.slice(2).reduce((pairs, value, i, all) => (i % 2 ? pairs : [...pairs, [value.replace(/^--/, ""), all[i + 1]]]), []));
const name = args.name ?? "@xelexis/x-broker";
const key = `${process.platform}-${process.arch}`;
const tarball = path.join(args.packages, `${name.slice(1).replace("/", "-")}-${args.version}.tgz`);
const bundled = { "darwin-arm64": "asb-client", "darwin-x64": "asb-client", "linux-x64": "asb-client", "win32-x64": "asb-client.exe" };
// Realpath: the broker rejects state directories reached through symlinks (macOS /var).
const work = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "x-broker-smoke-"));
const env = { ...process.env, npm_config_cache: path.join(work, "npm-cache"), npm_config_update_notifier: "false", npm_config_fund: "false", npm_config_audit: "false" };
const children = [];

function check(condition, message) {
  if (!condition) throw new Error(message);
  console.log(`ok - ${message}`);
}

function npmRun(tool, argv) {
  const command = process.platform === "win32" ? `${tool}.cmd` : tool;
  return spawnSync(command, argv, { cwd: work, env, encoding: "utf8", shell: process.platform === "win32" });
}

async function waitFor(check, ms = 15000) {
  for (const end = Date.now() + ms; Date.now() < end; await new Promise((r) => setTimeout(r, 50))) {
    const value = await check();
    if (value) return value;
  }
  throw new Error("timed out");
}

async function startBroker() {
  let log = "";
  const broker = spawn(args.server, ["--listen", "127.0.0.1:0", "--memory-dir", path.join(work, "broker")], { stdio: ["ignore", "pipe", "pipe"] });
  children.push(broker);
  broker.stdout.on("data", (d) => { log += d; });
  broker.stderr.on("data", (d) => { log += d; });
  const port = await waitFor(() => log.match(/127\.0\.0\.1:([1-9]\d*)/)?.[1]).catch(() => { throw new Error(`broker did not start: ${log}`); });
  const base = `http://127.0.0.1:${port}`;
  const jar = new Map();
  const request = async (method, target, form) => {
    const headers = { "user-agent": "asb-npm-smoke/1", cookie: [...jar].map(([k, v]) => `${k}=${v}`).join("; ") };
    const body = form && new URLSearchParams(form).toString();
    if (form) headers["content-type"] = "application/x-www-form-urlencoded";
    const res = await fetch(base + target, { method, headers, body, redirect: "manual" });
    for (const cookie of res.headers.getSetCookie()) {
      const [pair] = cookie.split(";");
      const [k, v] = pair.split("=");
      if (v) jar.set(k, v); else jar.delete(k);
    }
    if (res.status >= 400) throw new Error(`${method} ${target} = ${res.status}`);
    return { location: res.headers.get("location"), text: await res.text() };
  };
  const csrf = (html) => html.match(/name="csrf" value="([^"]+)"/)[1];
  const password = `smoke password ${process.pid} ${Date.now()}`;
  await request("POST", "/setup", { setupCode: fs.readFileSync(path.join(work, "broker", "control", "setup-code"), "utf8").trim(), password, confirmPassword: password, csrf: csrf((await request("GET", "/setup")).text) });
  await request("POST", "/login", { username: "admin", password, csrf: csrf((await request("GET", "/login")).text) });
  const token = csrf((await request("GET", "/")).text);
  await request("POST", "/projects/create", { name: "smoke", csrf: token });
  await request("POST", "/admin/http-policy", { allowHTTP: "true", csrf: token });
  const issued = await request("POST", "/tokens/project", { project: "smoke", label: "npm-smoke", csrf: token });
  const projectToken = (await request("GET", issued.location)).text.match(/<code id="issued-token">([^<]+)<\/code>/)[1];
  return ["--broker-url", `${base}/mcp?project=smoke`, "--token", projectToken, "--auth-dir", path.join(work, "auth")];
}

function startClient(command, clientArgs) {
  const child = spawn(command, clientArgs, { env, stdio: ["pipe", "pipe", "pipe"], shell: process.platform === "win32" });
  children.push(child);
  let stderr = "";
  child.stderr.on("data", (d) => { stderr += d; });
  const lines = readline.createInterface({ input: child.stdout });
  const exited = new Promise((resolve) => child.on("exit", (code, signal) => resolve({ code, signal })));
  const call = (message) => new Promise((resolve, reject) => {
    lines.once("line", (line) => { try { resolve(JSON.parse(line)); } catch { reject(new Error(`non-JSON stdout: ${line}`)); } });
    child.stdin.write(JSON.stringify(message) + "\n");
  });
  return { child, call, exited, stderr: () => stderr };
}

const alive = (pid) => { try { process.kill(pid, 0); return true; } catch { return false; } };

async function main() {
  check(fs.existsSync(tarball), `tarball for ${name} exists`);
  const prefix = path.join(work, "prefix");
  fs.mkdirSync(prefix);
  const install = npmRun("npm", ["install", "--prefix", prefix, "--offline", "--ignore-scripts", tarball]);
  check(install.status === 0, `offline npm install of the single package (${install.stderr.trim().split("\n").pop() || "no stderr"})`);
  const installed = path.join(prefix, "node_modules", ...name.split("/"));
  check(Object.entries(bundled).every(([k, file]) => fs.statSync(path.join(installed, "bin", k, file), { throwIfNoEntry: false })?.isFile()), "installed package bundles all four clients");
  if (process.platform !== "win32") {
    // npm applies the installing user's umask, so ask whether this user can run it.
    const runnable = (() => { try { fs.accessSync(path.join(installed, "bin", key, bundled[key]), fs.constants.X_OK); return true; } catch { return false; } })();
    check(runnable, `installed ${key} client is executable`);
  }
  const bin = path.join(prefix, "node_modules", ".bin", process.platform === "win32" ? "x-broker.cmd" : "x-broker");

  const version = spawnSync(bin, ["--version"], { env, encoding: "utf8", shell: process.platform === "win32" });
  check(version.status === 0 && version.stdout === `asb-client ${args.version}\n` && version.stderr === "", `x-broker --version prints asb-client ${args.version}`);
  const help = spawnSync(bin, ["--help"], { env, encoding: "utf8", shell: process.platform === "win32" });
  check(help.status === 0 && help.stdout.startsWith("Usage: asb-client --broker-url URL --token value"), "x-broker --help prints the client usage");
  const npx = npmRun("npx", ["--yes", "--offline", "--package", tarball, "x-broker", "--version"]);
  check(npx.status === 0 && npx.stdout === `asb-client ${args.version}\n`, "npx runs the packed launcher offline");

  const clientArgs = await startBroker();
  const session = startClient(bin, clientArgs);
  const init = await session.call({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "npm-smoke", version: "0" } } });
  check(init.result?.serverInfo, "MCP initialize through the launcher");
  session.child.stdin.write(JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" }) + "\n");
  const tools = (await session.call({ jsonrpc: "2.0", id: 2, method: "tools/list" })).result?.tools?.map((t) => t.name) ?? [];
  check(["attach_session", "auth_status", "list_sessions", "send_message"].every((t) => tools.includes(t)), `MCP tools/list returns ${tools.length} broker tools`);
  session.child.stdin.end();
  const ended = await Promise.race([session.exited, new Promise((r) => setTimeout(() => r("timeout"), 15000))]);
  check(ended !== "timeout" && ended.code === 0, `stdin EOF ends the launcher and client with exit 0 (${JSON.stringify(ended)} ${session.stderr().trim()})`);

  if (process.platform !== "win32") {
    const orphan = startClient(bin, clientArgs);
    await orphan.call({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "npm-smoke", version: "0" } } });
    const usesExecve = typeof process.execve === "function";
    const clientPid = usesExecve ? orphan.child.pid : Number(execFileSync("pgrep", ["-P", String(orphan.child.pid)], { encoding: "utf8" }).trim());
    check(clientPid > 0 && alive(clientPid), usesExecve ? "asb-client replaces the launcher PID" : "asb-client runs as the launcher's child");
    orphan.child.kill("SIGKILL");
    await waitFor(() => !alive(clientPid));
    check(true, usesExecve ? "killing the replaced launcher PID ends asb-client (no orphan)" : "killing the launcher ends asb-client through stdin EOF (no orphan)");
  }
}

main()
  .then(() => console.log(`npm smoke passed on ${key}`))
  .catch((err) => { console.error(`not ok - ${err.message}`); process.exitCode = 1; })
  .finally(() => {
    for (const child of children) if (child.exitCode === null && child.signalCode === null) child.kill();
    fs.rmSync(work, { recursive: true, force: true });
  });
