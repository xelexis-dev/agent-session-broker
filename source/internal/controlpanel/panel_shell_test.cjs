// NODE_PATH must provide Playwright. PW_CHROMIUM_EXECUTABLE optionally reuses
// an installed browser; no package or browser download happens in this test.
const { test, before, after } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const http = require("node:http");
const { chromium } = require("playwright");

let browser;
let server;
let origin;
const csp = "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'";

// A narrow shell fixture, using the real shell hooks and real production CSS
// and scripts. Actual Go template rendering and authorization are covered by
// server E2E; this fixture must not be used as evidence for those contracts.
const html = `<!doctype html><html lang="ko"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="stylesheet" href="/assets/fonts/pretendard/pretendardvariable-dynamic-subset.css"><link rel="stylesheet" href="/assets/tokens.css">
<link rel="stylesheet" href="/assets/focus.css"><link rel="stylesheet" href="/assets/panel.css"><link rel="stylesheet" href="/assets/design.css">
<script src="/assets/theme.js"></script><script src="/assets/shell.js" defer></script>
</head><body class="asb"><div class="app">
<aside class="sidebar" id="panel-sidebar"><div class="sidebar__head">
<span class="brand">ASB</span>
<button type="button" data-mobile-menu-close aria-label="메뉴 닫기" hidden>닫기</button></div>
<nav class="nav"><div class="nav__group"><a class="nav__item" href="/" id="projects-link">프로젝트</a><a class="nav__item" href="/profile" id="profile-link">내 프로필</a></div></nav></aside>
<div class="workspace"><header class="topbar">
<button type="button" class="topbar__menu" data-mobile-menu-toggle aria-controls="panel-sidebar" aria-expanded="false" hidden>메뉴 열기</button>
<button type="button" class="theme-toggle" data-theme-toggle aria-label="라이트·다크 테마 전환" aria-pressed="false" hidden>테마</button>
</header><main id="content"><a href="#form" id="outside-link">로그인으로 이동</a>
<form id="form" method="post" action="/login"><input type="hidden" name="csrf" value="fixture-csrf">
<label>아이디<input name="username" required autocomplete="username"></label>
<label>비밀번호<input name="password" type="password" required autocomplete="current-password"></label>
<button type="submit">로그인</button></form></main></div></div>
</body></html>`;

before(async () => {
  const manifest = JSON.parse(fs.readFileSync(path.join(__dirname, "asset-manifest.json"), "utf8"));
  const assets = new Map(manifest.assets.map((a) => [a.path, { file: a.file, mime: a.contentType }]));
  for (const name of ["focus.css", "panel.css", "design.css", "theme.js", "shell.js"]) {
    assert.ok(fs.existsSync(path.join(__dirname, name)), `production asset missing: ${name}`);
    assets.set(`/assets/${name}`, { file: name, mime: name.endsWith(".css") ? "text/css" : "text/javascript" });
  }
  server = http.createServer((request, response) => {
    const url = new URL(request.url, "http://localhost");
    response.setHeader("Content-Security-Policy", csp);
    response.setHeader("X-Content-Type-Options", "nosniff");
    const asset = assets.get(url.pathname);
    if (asset) {
      response.writeHead(200, { "Content-Type": asset.mime });
      response.end(fs.readFileSync(path.join(__dirname, asset.file)));
      return;
    }
    if (request.method === "POST" && url.pathname === "/login") {
      let data = "";
      request.setEncoding("utf8");
      request.on("data", (chunk) => { data += chunk; });
      request.on("end", () => {
        const fields = new URLSearchParams(data);
        const valid = fields.get("csrf") === "fixture-csrf" && fields.get("username") === "sample-user" && fields.get("password") === "fixture-password";
        response.writeHead(valid ? 200 : 400, { "Content-Type": "text/html; charset=utf-8" });
        response.end(`<p id="form-result">${valid ? "native form submitted" : "invalid fixture form"}</p>`);
      });
      return;
    }
    if (request.method === "GET" && ["/", "/profile", "/login"].includes(url.pathname)) {
      response.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
      response.end(html);
      return;
    }
    response.writeHead(404);
    response.end();
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true, ...(process.env.PW_CHROMIUM_EXECUTABLE ? { executablePath: process.env.PW_CHROMIUM_EXECUTABLE } : {}) });
});

after(async () => {
  await browser?.close();
  if (server?.listening) await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
});

async function fixture(t, { colorScheme = "light", saved, blockedStorage = false, javaScriptEnabled = true, viewport = { width: 390, height: 844 } } = {}) {
  const context = await browser.newContext({ colorScheme, viewport, javaScriptEnabled,
    ...(saved === undefined ? {} : { storageState: { cookies: [], origins: [{ origin, localStorage: [{ name: "asb-theme", value: saved }] }] } }) });
  const page = await context.newPage();
  const errors = [];
  const external = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("request", (request) => { if (new URL(request.url()).origin !== origin) external.push(request.url()); });
  if (javaScriptEnabled) await page.addInitScript(({ blockedStorage }) => {
    window.fixtureCSPViolations = [];
    document.addEventListener("securitypolicyviolation", (event) => window.fixtureCSPViolations.push(event.violatedDirective));
    if (blockedStorage) Object.defineProperty(window, "localStorage", { configurable: true, get() { throw new DOMException("Storage denied", "SecurityError"); } });
  }, { blockedStorage });
  t.after(async () => {
    try {
      assert.deepEqual(errors, [], "shell should not throw when browser storage is unavailable");
      assert.deepEqual(external, [], "fixture must use only same-origin production assets");
      if (javaScriptEnabled) assert.deepEqual(await page.evaluate(() => window.fixtureCSPViolations), [], "self-only CSP must remain enforced");
    } finally { await context.close(); }
  });
  const response = await page.goto(origin);
  assert.equal(response.headers()["content-security-policy"], csp);
  return page;
}

async function theme(page) {
  return page.evaluate(() => ({ preference: document.documentElement.dataset.theme ?? null,
    canvas: getComputedStyle(document.documentElement).getPropertyValue("--color-canvas").trim(),
    pressed: document.querySelector("[data-theme-toggle]").getAttribute("aria-pressed") }));
}

test("system theme and invalid preferences use CSS defaults and track system changes", async (t) => {
  for (const saved of [undefined, "sepia"]) {
    const page = await fixture(t, { colorScheme: "light", saved });
    assert.deepEqual(await theme(page), { preference: null, canvas: "#f3f6fa", pressed: "false" });
    await page.emulateMedia({ colorScheme: "dark" });
    await page.waitForFunction(() => document.querySelector("[data-theme-toggle]").getAttribute("aria-pressed") === "true");
    assert.deepEqual(await theme(page), { preference: null, canvas: "#070b12", pressed: "true" });
  }
});

test("saved theme wins over system, toggles accessibly, and survives reload", async (t) => {
  const page = await fixture(t, { colorScheme: "dark", saved: "light" });
  const button = page.locator("[data-theme-toggle]");
  assert.equal(await button.isVisible(), true);
  assert.deepEqual(await theme(page), { preference: "light", canvas: "#f3f6fa", pressed: "false" });
  await button.click();
  assert.deepEqual(await theme(page), { preference: "dark", canvas: "#070b12", pressed: "true" });
  assert.equal(await page.evaluate(() => localStorage.getItem("asb-theme")), "dark");
  await page.reload();
  await page.emulateMedia({ colorScheme: "light" });
  assert.deepEqual(await theme(page), { preference: "dark", canvas: "#070b12", pressed: "true" });
});

test("denied localStorage still follows system and allows an in-page theme choice", async (t) => {
  const page = await fixture(t, { colorScheme: "dark", blockedStorage: true });
  assert.deepEqual(await theme(page), { preference: null, canvas: "#070b12", pressed: "true" });
  await page.locator("[data-theme-toggle]").click();
  assert.deepEqual(await theme(page), { preference: "light", canvas: "#f3f6fa", pressed: "false" });
  await page.reload();
  assert.deepEqual(await theme(page), { preference: null, canvas: "#070b12", pressed: "true" });
});

test("mobile disclosure opens, closes, restores focus, and excludes closed navigation from Tab", async (t) => {
  const page = await fixture(t);
  const toggle = page.locator("[data-mobile-menu-toggle]");
  const sidebar = page.locator("#panel-sidebar");
  assert.equal(await sidebar.isHidden(), true);
  await toggle.click();
  assert.equal(await toggle.getAttribute("aria-expanded"), "true");
  assert.equal(await sidebar.isVisible(), true);
  assert.equal(await page.evaluate(() => document.querySelector("#panel-sidebar").contains(document.activeElement)), true);
  await page.keyboard.press("Escape");
  assert.equal(await toggle.getAttribute("aria-expanded"), "false");
  assert.equal(await toggle.evaluate((node) => node === document.activeElement), true);
  await toggle.click();
  await page.locator("[data-mobile-menu-close]").click();
  assert.equal(await sidebar.isHidden(), true);
  assert.equal(await toggle.evaluate((node) => node === document.activeElement), true);
  for (let i = 0; i < 8; i++) {
    await page.keyboard.press("Tab");
    assert.equal(await page.evaluate(() => document.querySelector("#panel-sidebar").contains(document.activeElement)), false, "closed navigation must not receive keyboard focus");
  }
});

test("without JavaScript mobile navigation and native form submission remain usable", async (t) => {
  const page = await fixture(t, { javaScriptEnabled: false });
  assert.equal(await page.locator("#panel-sidebar").isVisible(), true);
  assert.equal(await page.locator("[data-theme-toggle]").isHidden(), true);
  assert.equal(await page.locator("[data-mobile-menu-toggle]").isHidden(), true);
  await page.locator("#profile-link").click();
  await page.waitForURL(`${origin}/profile`);
  await page.locator('[name="username"]').fill("sample-user");
  await page.locator('[name="password"]').fill("fixture-password");
  await page.locator('button[type="submit"]').click();
  await page.waitForURL(`${origin}/login`);
  assert.equal(await page.locator("#form-result").textContent(), "native form submitted");
});

test("keyboard focus outlines stay inside the short sidebar scrollport", async (t) => {
  for (const viewport of [{ width: 834, height: 400 }, { width: 834, height: 560 }, { width: 1440, height: 400 }]) {
    for (const colorScheme of ["light", "dark"]) {
      const page = await fixture(t, { viewport, colorScheme });
      // Model the project-admin navigation density without claiming this
      // narrow fixture verifies Go template rendering or authorization.
      await page.evaluate(() => {
        const nav = document.querySelector(".nav");
        nav.replaceChildren();
        let index = 0;
        for (const count of [3, 2, 2]) {
          const group = document.createElement("div");
          group.className = "nav__group";
          const label = document.createElement("p");
          label.className = "nav__label";
          label.textContent = "Navigation";
          group.append(label);
          for (let i = 0; i < count; i++) {
            const item = document.createElement("a");
            item.className = "nav__item";
            item.href = "/";
            item.textContent = String(++index);
            group.append(item);
          }
          nav.append(group);
        }
        const footer = document.createElement("div");
        footer.className = "sidebar-footer";
        const account = document.createElement("button");
        account.className = "user-menu-button";
        account.textContent = "Account";
        footer.append(account);
        document.querySelector(".sidebar").append(footer);
      });
      const checkOutline = async () => {
        const bounds = await page.evaluate(() => {
          const active = document.activeElement;
          const sidebar = document.querySelector(".sidebar");
          const box = active.getBoundingClientRect();
          const clip = sidebar.getBoundingClientRect();
          const style = getComputedStyle(active);
          const extent = parseFloat(style.outlineWidth) + parseFloat(style.outlineOffset);
          return { focused: sidebar.contains(active) && active.matches(":focus-visible"),
            extent, top: box.top - extent, bottom: box.bottom + extent,
            clipTop: Math.max(0, clip.top + sidebar.clientTop),
            clipBottom: Math.min(innerHeight, clip.top + sidebar.clientTop + sidebar.clientHeight) };
        });
        assert.equal(bounds.focused, true, "Tab must focus a sidebar control visibly");
        assert.ok(bounds.extent > 0, "the visible focus outline must remain enabled");
        assert.ok(bounds.top >= bounds.clipTop - 0.5 && bounds.bottom <= bounds.clipBottom + 0.5,
          `${viewport.width}x${viewport.height} ${colorScheme}: outline clipped ${JSON.stringify(bounds)}`);
      };
      for (let i = 0; i < 8; i++) {
        await page.keyboard.press("Tab");
        await checkOutline();
      }
      for (let i = 0; i < 7; i++) {
        await page.keyboard.press("Shift+Tab");
        await checkOutline();
      }
    }
  }
});

test("shared focus ring distinguishes pointer buttons and keyboard input wrappers", async (t) => {
  const page = await fixture(t, { viewport: { width: 1440, height: 900 } });
  await page.evaluate(() => {
    const wrapper = document.createElement("span");
    wrapper.className = "input";
    const input = document.querySelector('[name="username"]');
    input.before(wrapper);
    wrapper.append(input);
    const search = document.createElement("label");
    search.className = "search";
    search.innerHTML = '<input type="search" aria-label="Fixture search">';
    document.querySelector("main").append(search);
  });
  const button = page.locator('[data-theme-toggle]');
  await button.click();
  assert.equal(await button.evaluate((node) => getComputedStyle(node).outlineStyle), "none");
  await page.keyboard.press("Shift+Tab");
  await page.keyboard.press("Tab");
  assert.equal(await button.evaluate((node) => node.matches(":focus-visible") && getComputedStyle(node).outlineStyle === "solid"), true);
  const input = page.locator('[name="username"]');
  await input.focus();
  for (const forcedColors of ["none", "active"]) {
    await page.emulateMedia({ forcedColors });
    const ring = await input.evaluate((node) => {
      const parent = getComputedStyle(node.parentElement);
      return { outline: parent.outlineStyle, width: parent.outlineWidth,
        offset: parent.outlineOffset, shadow: parent.boxShadow,
        inner: getComputedStyle(node).outlineStyle };
    });
    assert.deepEqual(ring, { outline: "solid", width: "2px", offset: "2px", shadow: "none", inner: "none" });
    await page.locator('input[type="search"]').focus();
    assert.equal(await page.locator(".search").evaluate((node) => getComputedStyle(node).outlineStyle), "solid");
    await input.focus();
  }
});
