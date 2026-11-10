// Run with node --test; needs npm on PATH. Fake binaries keep this fast; the
// smoke test packs and runs the real clients.
const { test } = require("node:test");
const assert = require("node:assert/strict");
const { execFileSync, spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const pack = path.join(__dirname, "pack.cjs");
const platforms = { "darwin-arm64": "asb-client", "darwin-x64": "asb-client", "linux-x64": "asb-client", "win32-x64": "asb-client.exe" };
const assets = { "darwin-arm64": "darwin-arm64", "darwin-x64": "darwin-amd64", "linux-x64": "linux-amd64", "win32-x64": "windows-amd64.exe" };

function workdir(t) {
  const dir = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "x-broker-pack-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

function fakeBinaries(dir, keys = Object.keys(platforms)) {
  return keys.map((key) => {
    const file = path.join(dir, `bin-${key}`);
    fs.writeFileSync(file, `binary for ${key}\n`, { mode: 0o644 });
    return `${key}=${file}`;
  });
}

// Public tests use a synthetic private-origin policy; the real one stays private.
const syntheticPolicy = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "asb-pack-policy-")), "policy.json");
fs.writeFileSync(syntheticPolicy, JSON.stringify({ patterns: ["git\\.example\\.invalid", "example-org\\/private-repo"] }));
process.env.ASB_PRIVATE_ORIGIN_POLICY = syntheticPolicy;

function runPack(args) {
  return spawnSync(process.execPath, [pack, ...args], { encoding: "utf8" });
}

// Read a tarball's file list with modes and its package.json without npm.
function inspect(tgz) {
  const listing = execFileSync("tar", ["-tvzf", tgz], { encoding: "utf8" }).trim().split("\n");
  const files = Object.fromEntries(listing.map((line) => { const parts = line.trim().split(/\s+/); return [parts[parts.length - 1], parts[0]]; }));
  const manifest = JSON.parse(execFileSync("tar", ["-xOzf", tgz, "package/package.json"], { encoding: "utf8" }));
  return { files, manifest };
}

const bundled = Object.fromEntries(Object.entries(platforms).map(([key, binary]) => [key, `package/bin/${key}/${binary}`]));

test("packs one package with the launcher and all four clients", (t) => {
  const dir = workdir(t);
  const out = path.join(dir, "out");
  const result = runPack(["--version", "1.2.3", "--out", out, ...fakeBinaries(dir).flatMap((b) => ["--binary", b])]);
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(fs.readdirSync(out), ["xelexis-x-broker-1.2.3.tgz"]);
  assert.equal(result.stdout, path.join(out, "xelexis-x-broker-1.2.3.tgz") + "\n");

  const tgz = path.join(out, "xelexis-x-broker-1.2.3.tgz");
  const { files, manifest } = inspect(tgz);
  assert.deepEqual(Object.keys(files).sort(), ["package/README.md", "package/THIRD_PARTY_NOTICES", "package/LICENSE", "package/bin/x-broker.js", "package/package.json", ...Object.values(bundled)].sort());
  if (process.platform !== "win32") {
    for (const file of ["package/bin/x-broker.js", ...Object.values(bundled)]) assert.match(files[file], /^-rwxr-xr-x/, file);
  }
  assert.equal(manifest.name, "@xelexis/x-broker");
  assert.equal(manifest.version, "1.2.3");
  assert.deepEqual(manifest.bin, { "x-broker": "bin/x-broker.js" });
  assert.deepEqual(manifest.engines, { node: ">=22" });
  for (const key of ["optionalDependencies", "dependencies", "os", "cpu", "scripts"]) assert.equal(manifest[key], undefined, key);
  // Public links go to the publishing repository (P4: the public source repository from 1.0.3); npm requires an exact match.
  assert.deepEqual(manifest.repository, { type: "git", url: "git+https://github.com/xelexis-dev/agent-session-broker.git" });
  assert.equal(manifest.homepage, "https://github.com/xelexis-dev/agent-session-broker#readme");
  assert.deepEqual(manifest.bugs, { url: "https://github.com/xelexis-dev/agent-session-broker#readme" });
  assert.doesNotMatch(execFileSync("tar", ["-xOzf", tgz, "package/package.json"], { encoding: "utf8" }), /agent-session-broker-deploy/);
  assert.equal(manifest.license, "SEE LICENSE IN LICENSE");
  for (const name of ["THIRD_PARTY_NOTICES", "LICENSE"]) {
    assert.equal(execFileSync("tar", ["-xOzf", tgz, `package/${name}`], { encoding: "utf8" }), fs.readFileSync(path.resolve(__dirname, "../..", name), "utf8"));
  }
  const readme = execFileSync("tar", ["-xOzf", tgz, "package/README.md"], { encoding: "utf8" });
  assert.match(readme, /@xelexis\/x-broker@1\.2\.3/);
  assert.match(readme, /blob\/main\/README_AGENT\.md/);
  for (const [key, file] of Object.entries(bundled)) {
    assert.equal(execFileSync("tar", ["-xOzf", tgz, file], { encoding: "utf8" }), `binary for ${key}\n`);
  }
});

test("release asset names select the four client binaries", (t) => {
  const dir = workdir(t);
  const input = path.join(dir, "assets");
  fs.mkdirSync(input);
  for (const [key, suffix] of Object.entries(assets)) fs.writeFileSync(path.join(input, `asb-client-2.0.0-${suffix}`), key);
  fs.writeFileSync(path.join(input, "asb-server-2.0.0-linux-amd64"), "server");
  const out = path.join(dir, "out");
  const result = runPack(["--version", "2.0.0", "--out", out, "--assets", input]);
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(fs.readdirSync(out), ["xelexis-x-broker-2.0.0.tgz"]);
  for (const [key, file] of Object.entries(bundled)) {
    assert.equal(execFileSync("tar", ["-xOzf", path.join(out, "xelexis-x-broker-2.0.0.tgz"), file], { encoding: "utf8" }), key);
  }
});

test("a bundle missing any platform is refused", (t) => {
  const dir = workdir(t);
  const out = path.join(dir, "out");
  const result = runPack(["--version", "1.2.3-rc.1", "--out", out, ...fakeBinaries(dir, ["linux-x64", "win32-x64"]).flatMap((b) => ["--binary", b])]);
  assert.notEqual(result.status, 0);
  assert.equal(result.stderr, "pack: all four client binaries are required; missing darwin-arm64, darwin-x64\n");
  assert.equal(fs.existsSync(out), false);
});

test("the package name can be changed before publication", (t) => {
  const dir = workdir(t);
  const out = path.join(dir, "out");
  const result = runPack(["--version", "1.2.3", "--name", "@other/broker", "--out", out, ...fakeBinaries(dir).flatMap((b) => ["--binary", b])]);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(inspect(path.join(out, "other-broker-1.2.3.tgz")).manifest.name, "@other/broker");
});

test("invalid input is rejected without writing packages", (t) => {
  const dir = workdir(t);
  const all = fakeBinaries(dir).flatMap((b) => ["--binary", b]);
  const linux = all[all.findIndex((v) => v.startsWith("linux-x64="))];
  const cases = [
    ["--version", "v1.2.3", ...all],
    ["--version", "1.2", ...all],
    ["--version", "1.2.3", ...all, "--binary", linux.replace("linux-x64", "linux-arm64")],
    ["--version", "1.2.3", ...all.map((v) => (v === linux ? "linux-x64=" + path.join(dir, "missing") : v))],
    ["--version", "1.2.3", ...all, "--binary", linux],
    ["--version", "1.2.3"],
    ["--version", "1.2.3", ...all, "--name", "x-broker"],
    ["--version", "1.2.3", ...all, "--unknown"],
  ];
  for (const args of cases) {
    const out = path.join(dir, "out-" + cases.indexOf(args));
    const result = runPack([...args, "--out", out]);
    assert.notEqual(result.status, 0, args.join(" "));
    assert.match(result.stderr, /^pack: /, args.join(" "));
    assert.equal(fs.existsSync(out) && fs.readdirSync(out).some((f) => f.endsWith(".tgz")), false, args.join(" "));
  }
  const assetsDir = path.join(dir, "partial");
  fs.mkdirSync(assetsDir);
  fs.writeFileSync(path.join(assetsDir, "asb-client-1.2.3-linux-amd64"), "x");
  assert.match(runPack(["--version", "1.2.3", "--assets", assetsDir, "--out", path.join(dir, "o")]).stderr, /^pack: missing .*asb-client-1\.2\.3-darwin-arm64/);
});

test("a client naming a private origin is refused before packing", (t) => {
  const dir = workdir(t);
  const out = path.join(dir, "out");
  const binaries = fakeBinaries(dir);
  fs.writeFileSync(path.join(dir, "bin-linux-x64"), "path\tgit.example.invalid/example-org/private-repo/cmd/asb-client\n");
  const result = runPack(["--version", "1.2.3", "--out", out, ...binaries.flatMap((b) => ["--binary", b])]);
  assert.notEqual(result.status, 0);
  assert.equal(result.stderr, "pack: bin/linux-x64/asb-client contains 2 private source repository reference(s); refusing to pack a public package\n");
  assert.doesNotMatch(result.stderr, /example-org|example\.invalid/);
  assert.equal(fs.existsSync(out), false);
});

test("the public module path is allowed in clients", (t) => {
  const dir = workdir(t);
  const out = path.join(dir, "out");
  const binaries = fakeBinaries(dir);
  fs.writeFileSync(path.join(dir, "bin-win32-x64"), "path\tgithub.com/xelexis-dev/agent-session-broker/source/cmd/asb-client\n");
  const result = runPack(["--version", "1.2.3", "--out", out, ...binaries.flatMap((b) => ["--binary", b])]);
  assert.equal(result.status, 0, result.stderr);
});

test("private origin references are counted in text and bytes; public repositories are not", () => {
  const { privateReferences } = require(pack);
  assert.equal(privateReferences("GIT.EXAMPLE.INVALID"), 1);
  assert.equal(privateReferences("example-org/private-repo"), 1);
  assert.equal(privateReferences("git@example.invalid:Example-Org/Private-Repo.git"), 1);
  assert.equal(privateReferences(JSON.stringify({ repository: { url: "git+https://github.com/xelexis-dev/agent-session-broker.git" } })), 0);
  assert.equal(privateReferences("HTTPS://GITHUB.COM/Xelexis-Dev/Agent-Session-Broker/issues"), 0);
  assert.equal(privateReferences(Buffer.from("\0github.com/xelexis-dev/agent-session-broker/source\0github.com/xelexis-dev/agent-session-broker\0", "latin1")), 0);
  assert.equal(privateReferences("git+https://github.com/xelexis-dev/agent-session-broker-deploy.git https://github.com/xelexis-dev/agent-session-broker-deploy#readme"), 0);
});

test("package links follow the fixed per-version publishing repository (P4 from 1.0.3)", () => {
  const { publicLinks } = require("./pack.cjs");
  for (const [version, repo] of [["0.0.0", "agent-session-broker-deploy"], ["1.0.2", "agent-session-broker-deploy"],
    ["1.0.3", "agent-session-broker"], ["1.1.0", "agent-session-broker"], ["2.0.0", "agent-session-broker"]]) {
    assert.deepEqual(publicLinks(version), { repository: { type: "git", url: `git+https://github.com/xelexis-dev/${repo}.git` },
      homepage: `https://github.com/xelexis-dev/${repo}#readme`, bugs: { url: `https://github.com/xelexis-dev/${repo}#readme` } }, version);
  }
});

// CI runners may use a strict umask; packing must not depend on it.
test("packing gives the same tarball modes under umask 022 and 077", { skip: process.platform === "win32" }, (t) => {
  for (const mask of ["022", "077"]) {
    const dir = workdir(t);
    const out = path.join(dir, "out");
    const args = [pack, "--version", "1.2.3", "--out", out, ...fakeBinaries(dir).flatMap((b) => ["--binary", b])];
    const result = spawnSync("/bin/sh", ["-c", `umask ${mask}; exec "$0" "$@"`, process.execPath, ...args], { encoding: "utf8" });
    assert.equal(result.status, 0, `umask ${mask}: ${result.stderr}`);
    const { files } = inspect(path.join(out, "xelexis-x-broker-1.2.3.tgz"));
    for (const [file, mode] of Object.entries(files)) {
      assert.equal(mode.slice(0, 10), file.startsWith("package/bin/") ? "-rwxr-xr-x" : "-rw-r--r--", `umask ${mask}: ${file}`);
    }
  }
});

// The release records the client input fingerprint; branch packs omit it.
test("a client input fingerprint is recorded only when given", (t) => {
  const dir = workdir(t);
  const fingerprint = "sha256:" + "0123456789abcdef".repeat(4);
  const binaries = fakeBinaries(dir).flatMap((b) => ["--binary", b]);
  const withField = runPack(["--version", "1.2.3", "--out", path.join(dir, "with"), "--client-inputs", fingerprint, ...binaries]);
  assert.equal(withField.status, 0, withField.stderr);
  assert.equal(inspect(path.join(dir, "with", "xelexis-x-broker-1.2.3.tgz")).manifest.xBrokerClientInputs, fingerprint);
  const without = runPack(["--version", "1.2.3", "--out", path.join(dir, "without"), ...binaries]);
  assert.equal(without.status, 0, without.stderr);
  assert.equal(Object.hasOwn(inspect(path.join(dir, "without", "xelexis-x-broker-1.2.3.tgz")).manifest, "xBrokerClientInputs"), false);
});

test("an invalid client input fingerprint is refused before packing", (t) => {
  const dir = workdir(t);
  const binaries = fakeBinaries(dir).flatMap((b) => ["--binary", b]);
  for (const value of ["", "0123456789abcdef".repeat(4), "sha256:" + "0123456789ABCDEF".repeat(4), "sha256:" + "0".repeat(63), "sha512:" + "0".repeat(64), "sha256:" + "0".repeat(64) + "\n"]) {
    const out = path.join(dir, "out");
    const result = runPack(["--version", "1.2.3", "--out", out, "--client-inputs", value, ...binaries]);
    assert.notEqual(result.status, 0, JSON.stringify(value));
    assert.equal(result.stderr, "pack: --client-inputs must be sha256:<64 lowercase hex>\n", JSON.stringify(value));
    assert.equal(fs.existsSync(out), false);
  }
});

test("approved LICENSE is shipped byte-for-byte and empty text is refused", (t) => {
  const dir = workdir(t);
  const root = path.join(dir, "isolated"), npm = path.join(root, "source/npm");
  fs.mkdirSync(npm, { recursive: true });
  for (const name of ["pack.cjs", "x-broker.js", "README.md"]) fs.copyFileSync(path.join(__dirname, name), path.join(npm, name));
  fs.copyFileSync(path.resolve(__dirname, "../../THIRD_PARTY_NOTICES"), path.join(root, "THIRD_PARTY_NOTICES"));
  fs.writeFileSync(path.join(root, "LICENSE"), "Synthetic approved license fixture\n");
  const out = path.join(dir, "out"), args = ["--version", "1.2.3", "--out", out, ...fakeBinaries(dir).flatMap(b => ["--binary", b])];
  const result = spawnSync(process.execPath, [path.join(npm, "pack.cjs"), ...args], { encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
  const tgz = path.join(out, "xelexis-x-broker-1.2.3.tgz");
  assert.equal(execFileSync("tar", ["-xOzf", tgz, "package/LICENSE"], { encoding: "utf8" }), "Synthetic approved license fixture\n");
  fs.writeFileSync(path.join(root, "LICENSE"), "");
  const rejected = spawnSync(process.execPath, [path.join(npm, "pack.cjs"), ...args], { encoding: "utf8" });
  assert.notEqual(rejected.status, 0);
  assert.match(rejected.stderr, /missing or unsafe LICENSE/);
});
