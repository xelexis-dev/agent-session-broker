#!/usr/bin/env node
"use strict";

// Decides whether one packed tarball may be published.
//   publish  the registry answers E404 (no such package or version)
//   skip     the published dist.integrity equals this exact tarball
// Anything else fails closed: different published content (including a
// placeholder at the same version) or any other registry answer, so a network
// error is never taken for an unpublished version. `npm pack` output is
// byte-reproducible for the same inputs, which makes a resumed run skip.
//
//   node publish-state.cjs <name> <version> <tarball>   prints publish|skip
const { spawnSync } = require("node:child_process");
const crypto = require("node:crypto");
const fs = require("node:fs");

function npmView(spec) {
  return spawnSync(process.platform === "win32" ? "npm.cmd" : "npm", ["view", spec, "dist.integrity", "--json"], {
    encoding: "utf8",
    shell: process.platform === "win32",
  });
}

function decide({ name, version, tarball, view = npmView }) {
  const local = "sha512-" + crypto.createHash("sha512").update(fs.readFileSync(tarball)).digest("base64");
  const spec = `${name}@${version}`;
  const answer = view(spec);
  let parsed;
  try {
    parsed = JSON.parse(answer.stdout);
  } catch {
    throw new Error(`${answer.status === 0 ? "unexpected" : "cannot read"} registry answer for ${spec}: ${(answer.stderr || answer.stdout || "").trim().split("\n")[0]}`);
  }
  if (answer.status !== 0) {
    if (parsed?.error?.code === "E404") return "publish";
    throw new Error(`cannot read ${spec} from the registry: ${parsed?.error?.code ?? "unknown error"}`);
  }
  if (typeof parsed !== "string") throw new Error(`unexpected registry answer for ${spec}: ${answer.stdout.trim()}`);
  if (parsed === local) return "skip";
  throw new Error(`${spec} is already published with different content (${parsed}, local ${local}); refusing to publish over or beside it`);
}

if (require.main === module) {
  const [name, version, tarball] = process.argv.slice(2);
  if (!name || !version || !tarball || process.argv.length !== 5) {
    process.stderr.write("usage: publish-state.cjs <name> <version> <tarball>\n");
    process.exit(2);
  }
  try {
    process.stdout.write(decide({ name, version, tarball }) + "\n");
  } catch (err) {
    process.stderr.write(`publish-state: ${err.message}\n`);
    process.exit(1);
  }
}

module.exports = { decide };
