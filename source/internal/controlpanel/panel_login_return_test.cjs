const { test, before, after } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const http = require("node:http");
const { chromium } = require("playwright");
let browser;
before(async () => { browser = await chromium.launch({ headless: true, executablePath: process.env.PW_CHROMIUM_EXECUTABLE }); });
after(async () => { await browser?.close(); });
const target = "/projects/sessions?project=demo";
const key = "asb-login-return";

// Exercise the real return script and native browser redirect/form behavior.
// The small server only supplies the handoff; Go tests cover auth and next validation.
async function fixture(t, { pending, blockedStorage = false, javaScriptEnabled = true } = {}) {
  const requests = [];
  const escape = (value) => value.replaceAll("&", "&amp;").replaceAll('"', "&quot;").replaceAll("<", "&lt;");
  const html = (next) => `<!doctype html><html><head><script src="/assets/login-return.js" defer></script></head><body>${next === undefined ? '<h1 id="anchor">Returned page</h1>' : `<form data-login-return method="post" action="/login"><input type="hidden" name="next" value="${escape(next)}"><input name="username"><input name="password" type="password"><button>Login</button></form>`}</body></html>`;
  const server = http.createServer(async (request, response) => {
    const url = new URL(request.url, "http://fixture");
    response.setHeader("Content-Security-Policy", "default-src 'none'; script-src 'self'; form-action 'self'; base-uri 'none'");
    if (url.pathname === "/assets/login-return.js") {
      response.writeHead(200, { "Content-Type": "text/javascript" });
      response.end(fs.readFileSync(path.join(__dirname, "login-return.js")));
      return;
    }
    if (request.method === "POST") {
      let body = "";
      for await (const chunk of request) body += chunk;
      const fields = new URLSearchParams(body);
      requests.push({ method: "POST", url: request.url, next: fields.get("next"), names: [...fields.keys()] });
      if (fields.get("password") !== "fixture-pass") {
        response.writeHead(401, { "Content-Type": "text/html" }).end(html(fields.get("next") || ""));
      } else {
        response.writeHead(303, { Location: fields.get("next") === target ? target : "/", "Set-Cookie": "fixture_login=done; Path=/; HttpOnly" }).end();
      }
      return;
    }
    requests.push({ method: "GET", url: request.url });
    if (url.pathname === "/projects/sessions" && !request.headers.cookie?.includes("fixture_login=done")) {
      response.writeHead(303, { Location: `/login?next=${encodeURIComponent(target)}` }).end();
      return;
    }
    response.writeHead(200, { "Content-Type": "text/html" }).end(html(url.pathname === "/login" ? url.searchParams.get("next") || "" : undefined));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const context = await browser.newContext({ javaScriptEnabled });
  if (javaScriptEnabled) await context.addInitScript(({ pending, blockedStorage, key }) => {
    if (pending !== undefined && !sessionStorage.getItem(key)) sessionStorage.setItem(key, typeof pending === "string" ? pending : JSON.stringify(pending));
    if (blockedStorage) Object.defineProperty(window, "sessionStorage", { get() { throw new DOMException("denied", "SecurityError"); } });
  }, { pending, blockedStorage, key });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  t.after(async () => { await context.close(); server.closeAllConnections(); await new Promise((resolve) => server.close(resolve)); assert.deepEqual(errors, []); });
  return { page, origin, requests };
}

async function login(page, password = "fixture-pass") {
  await page.locator('[name="username"]').fill("fixture-user");
  await page.locator('[name="password"]').fill(password);
  await Promise.all([page.waitForNavigation(), page.getByRole("button", { name: "Login" }).click()]);
}

test("return fragment survives a failed login and stays out of every server request", async (t) => {
  const { page, origin, requests } = await fixture(t);
  await page.goto(origin + target + "#anchor");
  assert.equal(new URL(page.url()).pathname, "/login");
  assert.equal(new URL(page.url()).hash, "");
  assert.equal(await page.locator('[name="next"]').inputValue(), target);
  assert.equal(await page.evaluate((key) => JSON.parse(sessionStorage.getItem(key)).fragment, key), "#anchor");
  await login(page, "wrong");
  assert.equal(await page.evaluate((key) => JSON.parse(sessionStorage.getItem(key)).fragment, key), "#anchor");
  await login(page);
  await page.waitForURL(origin + target + "#anchor");
  assert.equal(await page.evaluate((key) => sessionStorage.getItem(key), key), null);
  assert.ok(requests.every((r) => !r.url.includes("#")));
  for (const post of requests.filter((r) => r.method === "POST")) {
    assert.equal(post.next, target);
    assert.ok(!post.names.includes("fragment"));
  }
  await page.evaluate(() => history.replaceState(null, "", location.pathname + location.search));
  await page.reload();
  assert.equal(new URL(page.url()).hash, "", "the consumed anchor must not return on reload");
});

test("expired and malicious fragment records cannot navigate away", async (t) => {
  for (const pending of [{ target: "/profile", fragment: "#anchor", expires: Date.now() - 1 },
    { target: "/profile", fragment: "//example.org", expires: Date.now() + 60000 },
    { target: "/profile", fragment: "javascript:alert(1)", expires: Date.now() + 60000 }, "{broken"]) {
    const { page, origin } = await fixture(t, { pending });
    await page.goto(origin + "/profile");
    assert.equal(page.url(), origin + "/profile");
  }
});

test("only the matching query consumes an anchor and an explicit fragment wins", async (t) => {
  const pending = { target: "/profile?tab=access", fragment: "#old", expires: Date.now() + 60000 };
  const { page, origin } = await fixture(t, { pending });
  await page.goto(origin + "/profile?tab=other");
  assert.equal(new URL(page.url()).hash, "");
  await page.goto(origin + "/profile?tab=access#anchor");
  assert.equal(new URL(page.url()).hash, "#anchor");
  assert.equal(await page.evaluate((key) => sessionStorage.getItem(key), key), null);
});

test("direct login clears old return state and unsafe handoffs are ignored", async (t) => {
  for (const next of ["", "https://example.org", "//example.org", "/\\example.org", "/profile#anchor"]) {
    const { page, origin } = await fixture(t, { pending: { target: "/profile", fragment: "#old", expires: Date.now() + 60000 } });
    await page.goto(origin + "/login?next=" + encodeURIComponent(next) + "#new");
    assert.equal(await page.evaluate((key) => sessionStorage.getItem(key), key), null);
    assert.equal(new URL(page.url()).origin, origin);
  }
});

test("storage denial and disabled JavaScript preserve native path and query login", async (t) => {
  for (const options of [{ blockedStorage: true }, { javaScriptEnabled: false }]) {
    const { page, origin } = await fixture(t, options);
    await page.goto(origin + target + "#anchor");
    await login(page);
    assert.equal(new URL(page.url()).pathname + new URL(page.url()).search, target);
    assert.equal(new URL(page.url()).origin, origin);
  }
});
