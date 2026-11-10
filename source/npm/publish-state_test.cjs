// Run with node --test. The registry answer is injected; shapes follow npm 11
// `npm view <spec> dist.integrity --json` (E404 for a missing package or version).
const { test } = require("node:test");
const assert = require("node:assert/strict");
const { spawnSync } = require("node:child_process");
const crypto = require("node:crypto");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const script = path.join(__dirname, "publish-state.cjs");
const { decide } = require(script);

function tarball(t, content = "packed bytes") {
  const dir = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "x-broker-publish-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, "pkg.tgz");
  fs.writeFileSync(file, content);
  return { dir, file, integrity: "sha512-" + crypto.createHash("sha512").update(content).digest("base64") };
}

const npmError = (code) => ({ status: 1, stdout: JSON.stringify({ error: { code, summary: code } }), stderr: `npm error code ${code}` });

test("an unpublished package or version is published", (t) => {
  const { file } = tarball(t);
  let asked;
  const state = decide({ name: "@xelexis/x-broker-linux-x64", version: "1.2.3", tarball: file, view: (spec) => { asked = spec; return npmError("E404"); } });
  assert.equal(state, "publish");
  assert.equal(asked, "@xelexis/x-broker-linux-x64@1.2.3");
});

test("only byte-identical published content is skipped", (t) => {
  const { file, integrity } = tarball(t);
  assert.equal(decide({ name: "a", version: "1.2.3", tarball: file, view: () => ({ status: 0, stdout: JSON.stringify(integrity) + "\n", stderr: "" }) }), "skip");
  const other = "sha512-" + crypto.createHash("sha512").update("placeholder").digest("base64");
  assert.throws(() => decide({ name: "a", version: "1.2.3", tarball: file, view: () => ({ status: 0, stdout: JSON.stringify(other), stderr: "" }) }), /a@1\.2\.3 is already published with different content/);
});

test("registry failures are never mistaken for an unpublished version", (t) => {
  const { file } = tarball(t);
  for (const answer of [npmError("ECONNREFUSED"), npmError("E500"), npmError("EAI_AGAIN"), { status: 1, stdout: "", stderr: "boom" },
    { status: 0, stdout: "", stderr: "" }, { status: 0, stdout: "not json", stderr: "" }, { status: 0, stdout: "[\"sha512-x\"]", stderr: "" }]) {
    assert.throws(() => decide({ name: "a", version: "1.2.3", tarball: file, view: () => answer }), /^Error: (cannot read|unexpected)/, JSON.stringify(answer));
  }
});

test("the command prints the state and fails closed on a content mismatch", (t) => {
  const { dir, file, integrity } = tarball(t);
  // Both platform launchers answer npm view from an isolated synthetic registry.
  fs.writeFileSync(path.join(dir, "view.cjs"), 'process.stdout.write(process.env.FAKE_VIEW); process.exitCode = Number(process.env.FAKE_STATUS);');
  if (process.platform === "win32") {
    fs.writeFileSync(path.join(dir, "npm.cmd"), `@echo off\r\n"${process.execPath}" "%~dp0view.cjs"\r\n`);
  } else {
    fs.writeFileSync(path.join(dir, "npm"), `#!/bin/sh\nexec '${process.execPath.replaceAll("'", "'\\''")}' '${path.join(dir, "view.cjs").replaceAll("'", "'\\''")}'\n`, { mode: 0o755 });
  }
  const run = (stdout, status) => spawnSync(process.execPath, [script, "@xelexis/x-broker", "1.2.3", file], {
    encoding: "utf8", env: { ...process.env, PATH: `${dir}${path.delimiter}${process.env.PATH}`, FAKE_VIEW: stdout, FAKE_STATUS: String(status) },
  });
  assert.deepEqual([run(JSON.stringify({ error: { code: "E404" } }), 1).stdout, run(JSON.stringify(integrity), 0).stdout], ["publish\n", "skip\n"]);
  const mismatch = run(JSON.stringify("sha512-AAAA"), 0);
  assert.equal(mismatch.status, 1);
  assert.equal(mismatch.stdout, "");
  assert.match(mismatch.stderr, /^publish-state: @xelexis\/x-broker@1\.2\.3 is already published with different content/);
  assert.equal(run(JSON.stringify({ error: { code: "ETIMEDOUT" } }), 1).status, 1);
  assert.equal(spawnSync(process.execPath, [script, "only-name"], { encoding: "utf8" }).status, 2);
});
