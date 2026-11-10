#!/usr/bin/env node
"use strict";

// Builds the single npm package: the launcher plus the already built
// asb-client of all four supported platforms under bin/<platform>/, then runs
// `npm pack` and checks the tarball against its inputs. A partial bundle is
// refused. It never publishes; publication is npm-publish.yml.
//
//   node pack.cjs --version 1.2.3 --out dist [--name @xelexis/x-broker] [--client-inputs sha256:<hex>]
//     (--assets <dir with asb-client-<version>-<os>-<arch>> | --binary <platform>=<path> x4)
const { execFileSync } = require("node:child_process");
const crypto = require("node:crypto");
const fs = require("node:fs");
const path = require("node:path");

const platforms = {
  "darwin-arm64": { asset: "darwin-arm64", binary: "asb-client" },
  "darwin-x64": { asset: "darwin-amd64", binary: "asb-client" },
  "linux-x64": { asset: "linux-amd64", binary: "asb-client" },
  "win32-x64": { asset: "windows-amd64.exe", binary: "asb-client.exe" },
};
// Public package links point to the publishing repository. npm trusted publishing requires repository.url
// to match the workflow's repository exactly. Same fixed per-version rule as scripts/release_repo.py:
// the public source repository from 1.0.3 (P4), the deploy repository before (including 0.0.0 test builds).
function publisherFor(version) {
  const [major, minor, patch] = version.split(/[.-]/).slice(0, 3).map(Number);
  const moved = major > 1 || (major === 1 && (minor > 0 || patch >= 3));
  return `https://github.com/xelexis-dev/agent-session-broker${moved ? "" : "-deploy"}`;
}
function publicLinks(version) {
  const publisher = publisherFor(version);
  return { repository: { type: "git", url: `git+${publisher}.git` }, homepage: `${publisher}#readme`, bugs: { url: `${publisher}#readme` } };
}
// Reject private origin identifiers in all package bytes; public links are
// allowed. The patterns come from a private policy file that public source
// does not contain (ASB_PRIVATE_ORIGIN_POLICY overrides its location); CI
// must have it. Count matches, never print them.
function loadPrivateSource() {
  const file = process.env.ASB_PRIVATE_ORIGIN_POLICY || path.join(__dirname, "..", "..", "scripts", "private-origin.json");
  let raw;
  try {
    raw = fs.readFileSync(file, "utf8");
  } catch (error) {
    if (error.code === "ENOENT" && !process.env.GITLAB_CI) return null;
    throw new Error("pack: private origin policy unavailable");
  }
  const patterns = JSON.parse(raw).patterns;
  if (!Array.isArray(patterns) || patterns.length === 0 || !patterns.every((p) => typeof p === "string" && p)) {
    throw new Error("pack: invalid private origin policy");
  }
  return new RegExp(`(?:${patterns.join("|")})`, "gi");
}
const privateSource = loadPrivateSource();

function privateReferences(content) {
  if (!privateSource) return 0;
  const text = Buffer.isBuffer(content) ? content.toString("latin1") : String(content);
  return text.match(privateSource)?.length ?? 0;
}

function fail(message) {
  process.stderr.write(`pack: ${message}\n`);
  process.exit(1);
}

function parse(argv) {
  const options = { name: "@xelexis/x-broker", binaries: {} };
  for (let i = 0; i < argv.length; i += 2) {
    const [flag, value] = [argv[i], argv[i + 1]];
    if (!["--version", "--out", "--name", "--assets", "--binary", "--client-inputs"].includes(flag) || value === undefined) {
      fail(`unknown or incomplete option ${flag}`);
    }
    if (flag !== "--binary") {
      options[flag.slice(2)] = value;
      continue;
    }
    const at = value.indexOf("=");
    const key = value.slice(0, at);
    if (at < 1 || !Object.hasOwn(platforms, key)) fail(`--binary needs <platform>=<path> with a platform of ${Object.keys(platforms).join(", ")}`);
    if (options.binaries[key]) fail(`duplicate binary for ${key}`);
    options.binaries[key] = value.slice(at + 1);
  }
  if (!/^\d+\.\d+\.\d+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/.test(options.version ?? "")) fail("--version must be a plain semver such as 1.2.3");
  if (!/^@[a-z0-9][a-z0-9._-]*\/[a-z0-9][a-z0-9._-]*$/.test(options.name)) fail("--name must be a scoped npm name such as @xelexis/x-broker");
  if (!options.out) fail("--out is required");
  // Release packs record scripts/client-inputs.py output so the public publisher
  // can skip npm when the client did not change.
  if (options["client-inputs"] !== undefined && !/^sha256:[0-9a-f]{64}$/.test(options["client-inputs"])) {
    fail("--client-inputs must be sha256:<64 lowercase hex>");
  }
  if (options.assets && Object.keys(options.binaries).length) fail("use either --assets or --binary");
  if (options.assets) {
    for (const [key, platform] of Object.entries(platforms)) {
      options.binaries[key] = path.join(options.assets, `asb-client-${options.version}-${platform.asset}`);
    }
  }
  if (!Object.keys(options.binaries).length) fail("no client binaries given");
  const absent = Object.keys(platforms).filter((key) => !options.binaries[key]);
  if (absent.length) fail(`all four client binaries are required; missing ${absent.join(", ")}`);
  for (const file of Object.values(options.binaries)) {
    if (!fs.statSync(file, { throwIfNoEntry: false })?.isFile()) fail(`missing ${file}`);
  }
  return options;
}

function writePackage(dir, manifest, files) {
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, "package.json"), JSON.stringify(manifest, null, 2) + "\n");
  // Explicit modes keep the tarball independent of the packing umask.
  fs.chmodSync(path.join(dir, "package.json"), 0o644);
  for (const [target, { from, text, mode }] of Object.entries(files)) {
    const file = path.join(dir, target);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    if (from) fs.copyFileSync(from, file);
    else fs.writeFileSync(file, text);
    fs.chmodSync(file, mode);
  }
}

const sha256 = (file) => crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex");

// Unpack the tarball and compare every executable with its source, so a
// packing step can never ship a different or non-executable file.
function verify(tarball, stage, sources) {
  const dir = path.join(stage, "verify");
  fs.mkdirSync(dir);
  // -p keeps the archived modes instead of applying the current umask.
  execFileSync("tar", ["-xpzf", tarball, "-C", dir], { stdio: ["ignore", "ignore", "inherit"] });
  for (const [target, from] of Object.entries(sources)) {
    const file = path.join(dir, "package", target);
    if (sha256(file) !== sha256(from)) fail(`${path.basename(tarball)}: ${target} differs from ${from}`);
    if (target.startsWith("bin/") && process.platform !== "win32" && (fs.statSync(file).mode & 0o111) !== 0o111) fail(`${path.basename(tarball)}: ${target} is not executable`);
  }
}

function npmPack(dir, out, expected) {
  const npm = process.platform === "win32" ? "npm.cmd" : "npm";
  const json = execFileSync(npm, ["pack", dir, "--pack-destination", out, "--json", "--ignore-scripts"], {
    encoding: "utf8",
    stdio: ["ignore", "pipe", "inherit"],
    shell: process.platform === "win32",
  });
  const [result] = JSON.parse(json);
  const files = result.files.map((f) => f.path).sort();
  if (JSON.stringify(files) !== JSON.stringify([...expected].sort())) {
    fail(`${result.filename} contains ${files.join(", ")} instead of ${expected.join(", ")}`);
  }
  return path.join(out, result.filename);
}

function main() {
  const { version, name, out, binaries, "client-inputs": clientInputs } = parse(process.argv.slice(2));
  const readme = fs.readFileSync(path.join(__dirname, "README.md"), "utf8").replaceAll("{{name}}", name).replaceAll("{{version}}", version);
  const files = {
    "bin/x-broker.js": { from: path.join(__dirname, "x-broker.js"), mode: 0o755 },
    "README.md": { text: readme, mode: 0o644 },
  };
  const root = path.resolve(__dirname, "../..");
  for (const name of ["THIRD_PARTY_NOTICES", "LICENSE"]) {
    const from = path.join(root, name);
    const stat = fs.lstatSync(from, { throwIfNoEntry: false });
    if (!stat?.isFile() || stat.isSymbolicLink() || !stat.size) fail(`missing or unsafe ${name}`);
    files[name] = { from, mode: 0o644 };
  }
  const sources = Object.fromEntries(Object.entries(files).filter(([, f]) => f.from).map(([name, f]) => [name, f.from]));
  for (const [key, platform] of Object.entries(platforms)) {
    const target = `bin/${key}/${platform.binary}`;
    files[target] = { from: binaries[key], mode: 0o755 };
    sources[target] = binaries[key];
  }
  const manifest = {
    name,
    version,
    // shortcut: Keep this field until a reviewed workflow update permits "MIT".
    license: "SEE LICENSE IN LICENSE",
    description: "npx launcher and bundled clients for the Agent Session Broker MCP client (asb-client)",
    bin: { "x-broker": "bin/x-broker.js" },
    files: Object.keys(files).filter((file) => file !== "README.md"),
    engines: { node: ">=22" },
    ...publicLinks(version),
    ...(clientInputs === undefined ? {} : { xBrokerClientInputs: clientInputs }),
  };
  // Refuse before anything is written.
  for (const [target, content] of [["package.json", JSON.stringify(manifest)], ...Object.entries(files).map(([t, f]) => [t, f.text ?? fs.readFileSync(f.from)])]) {
    const count = privateReferences(content);
    if (count) fail(`${target} contains ${count} private source repository reference(s); refusing to pack a public package`);
  }
  fs.mkdirSync(out, { recursive: true });
  const stage = fs.mkdtempSync(path.join(out, ".stage-"));
  try {
    writePackage(path.join(stage, "package"), manifest, files);
    const tarball = npmPack(path.join(stage, "package"), out, [...Object.keys(files), "package.json"]);
    verify(tarball, stage, sources);
    process.stdout.write(tarball + "\n");
  } finally {
    fs.rmSync(stage, { recursive: true, force: true });
  }
}

if (require.main === module) main();
module.exports = { privateReferences, publicLinks };
