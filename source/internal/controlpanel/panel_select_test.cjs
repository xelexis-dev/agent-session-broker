// NODE_PATH must provide Playwright. PW_CHROMIUM_EXECUTABLE optionally reuses
// an installed browser; no package or browser download happens in this test.
// The shared select (select.js/select.css) on the real production CSS,
// under the panel's CSP. Template wiring is covered by Go tests and server E2E.
const { test, before, after } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const http = require("node:http");
const { chromium } = require("playwright");
const { dataBlock } = require("./i18n_fixture.cjs");

let browser;
let server;
let origin;
const csp = "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'";
const rows = Array.from({ length: 14 }, (_, i) => `<tr><td>user-${i}</td><td><div class="row-actions"><form class="role-change"><select name="role" aria-label="user-${i} 프로젝트 역할" data-select><option value="owner">owner</option><option value="developer" selected>developer</option><option value="viewer">viewer</option></select><button type="submit">적용</button></form></div></td></tr>`).join("");
const html = `<!doctype html><html lang="ko"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="stylesheet" href="/assets/tokens.css"><link rel="stylesheet" href="/assets/focus.css"><link rel="stylesheet" href="/assets/panel.css"><link rel="stylesheet" href="/assets/design.css"><link rel="stylesheet" href="/assets/select.css">
<script src="/assets/panel.js" defer></script><script type="module" src="/assets/select.js"></script>
</head><body class="asb">${dataBlock("ko")}<main>
<form id="basic" method="post" action="/submit"><input type="hidden" name="csrf" value="fixture-csrf">
<label>프로젝트 역할<select name="role" data-select><option value="viewer">viewer</option><option value="developer">developer</option><option value="owner">owner</option></select></label>
<label>필수 선택<select name="required-role" required data-select aria-describedby="required-help"><option value="">선택하세요</option><option value="user">사용자</option></select><small id="required-help">도움말</small></label>
<fieldset id="locked"><legend>잠금</legend><label>잠긴 역할<select name="locked-role" data-select><option value="user">사용자</option><option value="admin">시스템 관리자</option></select></label></fieldset>
<label>검색 역할<select name="search-role" data-select data-select-search="true"><option value="a">개발자</option><option value="b">관리자</option><option value="c">검토자</option><option value="d">운영자</option><option value="e">viewer</option></select></label>
<button type="reset" id="reset">초기화</button><button type="submit" id="submit">저장</button></form>
<div class="table-scroll"><table><tbody>${rows}</tbody></table></div>
<div class="row-actions" id="long-row"><form class="role-change"><select name="long-role" aria-label="긴 역할" data-select><option value="a">짧음</option><option value="b">아주 긴 옵션 이름 — 프로젝트 운영 담당자 겸 보안 검토자 (Primary on-call reviewer for production releases)</option></select><button type="submit">적용</button></form></div>
<button type="button" id="open-dialog">추가</button>
<dialog id="dialog"><form method="dialog"><label>시스템 역할<select name="dialog-role" data-select><option value="user">사용자</option><option value="admin">시스템 관리자</option></select></label></form></dialog>
<script src="/fixture.js"></script>
</main></body></html>`;
// Classic script for the fixture's own buttons (CSP self).
const fixtureJS = `document.getElementById("open-dialog").addEventListener("click", () => document.getElementById("dialog").showModal());
window.changes = [];
document.addEventListener("change", (e) => window.changes.push(e.target.name + "=" + e.target.value));`;

before(async () => {
  const manifest = JSON.parse(fs.readFileSync(path.join(__dirname, "asset-manifest.json"), "utf8"));
  const assets = new Map(manifest.assets.map((a) => [a.path, { file: a.file, mime: a.contentType }]));
  for (const name of ["focus.css", "panel.css", "design.css", "select.css", "select.js", "panel.js"]) {
    assets.set(`/assets/${name}`, { file: name, mime: name.endsWith(".css") ? "text/css" : "text/javascript" });
  }
  server = http.createServer((request, response) => {
    const url = new URL(request.url, "http://localhost");
    response.setHeader("Content-Security-Policy", csp);
    const asset = assets.get(url.pathname);
    if (asset && fs.existsSync(path.join(__dirname, asset.file))) {
      response.writeHead(200, { "Content-Type": asset.mime });
      response.end(fs.readFileSync(path.join(__dirname, asset.file)));
      return;
    }
    if (url.pathname === "/fixture.js") { response.writeHead(200, { "Content-Type": "text/javascript" }); response.end(fixtureJS); return; }
    if (url.pathname === "/") { response.writeHead(200, { "Content-Type": "text/html; charset=utf-8" }); response.end(html); return; }
    if (url.pathname === "/submit" && request.method === "POST") {
      let data = ""; request.setEncoding("utf8"); request.on("data", (c) => { data += c; });
      request.on("end", () => { response.writeHead(200, { "Content-Type": "text/plain" }); response.end(data); });
      return;
    }
    response.writeHead(404); response.end();
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true, ...(process.env.PW_CHROMIUM_EXECUTABLE ? { executablePath: process.env.PW_CHROMIUM_EXECUTABLE } : {}) });
});

after(async () => {
  await browser?.close();
  if (server?.listening) await new Promise((resolve) => server.close(resolve));
});

async function fixture(t, { viewport = { width: 1440, height: 900 }, javaScriptEnabled = true, colorScheme = "light" } = {}) {
  const context = await browser.newContext({ viewport, javaScriptEnabled, colorScheme });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  if (javaScriptEnabled) await page.addInitScript(() => {
    window.cspViolations = [];
    document.addEventListener("securitypolicyviolation", (e) => window.cspViolations.push(e.violatedDirective));
  });
  t.after(async () => {
    try {
      assert.deepEqual(errors, []);
      if (javaScriptEnabled) assert.deepEqual(await page.evaluate(() => window.cspViolations), [], "self-only CSP must hold");
    } finally { await context.close(); }
  });
  await page.goto(origin);
  if (javaScriptEnabled) await page.waitForSelector("#basic .select__trigger");
  return page;
}
const trigger = (page, name) => page.locator(`:is(#basic, #dialog) select[name="${name}"] ~ .select__trigger`);
const popupOf = (page, name) => page.locator(`:is(#basic, #dialog) select[name="${name}"] ~ .select__popup`);
const state = (page, name) => page.evaluate((name) => {
  const select = document.querySelector(`select[name="${name}"]`); const t = select.parentElement.querySelector(".select__trigger");
  const popup = select.parentElement.querySelector(".select__popup"); const active = document.getElementById((popup.querySelector("input") || t).getAttribute("aria-activedescendant") || "");
  return { value: select.value, text: t.querySelector(".select__value").textContent, expanded: t.getAttribute("aria-expanded"), open: popup.matches(":popover-open"),
    active: active ? active.textContent : null, focused: document.activeElement === t ? "trigger" : document.activeElement.tagName.toLowerCase() };
}, name);

test("enhances opted-in selects and keeps the native select as the form value", async (t) => {
  const page = await fixture(t);
  const info = await page.evaluate(() => {
    const select = document.querySelector('select[name="role"]'); const t = select.parentElement.querySelector(".select__trigger");
    return { nativeHidden: getComputedStyle(select).position === "absolute" && select.tabIndex === -1 && select.getAttribute("aria-hidden") === "true",
      role: t.getAttribute("role"), type: t.type, popup: t.getAttribute("aria-haspopup"), label: t.getAttribute("aria-label"),
      listInside: !!select.closest("form").querySelector('.select__popup [role="listbox"]'), searchNames: [...document.querySelectorAll(".select__popup input")].map((i) => i.name) };
  });
  assert.deepEqual(info, { nativeHidden: true, role: "combobox", type: "button", popup: "listbox", label: "프로젝트 역할", listInside: true, searchNames: [""] });
  assert.equal((await state(page, "role")).text, "viewer");
});

test("keyboard: open, move, select fires change once; Esc keeps the value; Tab selects and moves on", async (t) => {
  const page = await fixture(t);
  await trigger(page, "role").focus();
  await page.keyboard.press("ArrowDown");
  assert.deepEqual(await state(page, "role"), { value: "viewer", text: "viewer", expanded: "true", open: true, active: "viewer", focused: "trigger" });
  await page.keyboard.press("ArrowDown");
  assert.equal((await state(page, "role")).active, "developer");
  await page.keyboard.press("End");
  assert.equal((await state(page, "role")).active, "owner");
  await page.keyboard.press("Home");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  assert.deepEqual(await state(page, "role"), { value: "developer", text: "developer", expanded: "false", open: false, active: null, focused: "trigger" });
  assert.deepEqual(await page.evaluate(() => window.changes), ["role=developer"]);
  await page.keyboard.press("Space");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Escape");
  assert.deepEqual(await state(page, "role"), { value: "developer", text: "developer", expanded: "false", open: false, active: null, focused: "trigger" });
  await page.keyboard.press("Enter");
  await page.keyboard.press("ArrowUp");
  await page.keyboard.press("Tab");
  const after = await state(page, "role");
  assert.deepEqual([after.value, after.open], ["viewer", false]);
  assert.notEqual(after.focused, "trigger");
  assert.deepEqual(await page.evaluate(() => window.changes), ["role=developer", "role=viewer"]);
  assert.equal(page.url(), origin + "/", "no unintended submit");
});

test("mouse selection, outside click closes, and the form posts the native value with CSRF", async (t) => {
  const page = await fixture(t);
  await trigger(page, "role").click();
  await popupOf(page, "role").getByRole("option", { name: "owner" }).click();
  assert.equal((await state(page, "role")).value, "owner");
  await trigger(page, "role").click();
  await page.mouse.click(1300, 40);
  assert.equal((await state(page, "role")).open, false);
  await page.evaluate(() => { const s = document.querySelector('select[name="required-role"]'); s.value = "user"; s.dispatchEvent(new Event("change", { bubbles: true })); });
  await page.evaluate(() => { document.getElementById("locked").disabled = false; });
  await Promise.all([page.waitForURL(origin + "/submit"), page.click("#submit")]);
  const body = await page.locator("body").textContent();
  assert.match(body, /csrf=fixture-csrf/); assert.match(body, /role=owner/); assert.match(body, /required-role=user/);
  assert.doesNotMatch(body, /(^|&)=/, "custom trigger and search input post no fields");
});

test("search option filters locally, waits for IME composition, shows no-match, and resets on reopen", async (t) => {
  const page = await fixture(t);
  assert.equal(await popupOf(page, "role").locator("input").count(), 0, "search is off by default");
  await trigger(page, "search-role").click();
  const input = popupOf(page, "search-role").locator('input[role="combobox"]');
  assert.equal(await page.evaluate(() => document.activeElement.getAttribute("role")), "combobox");
  assert.equal(await page.evaluate(() => document.activeElement.tagName), "INPUT");
  const visible = () => popupOf(page, "search-role").locator('[role="option"]:not([hidden])').allTextContents();
  await input.fill("관리");
  assert.deepEqual(await visible(), ["관리자"]);
  await input.evaluate((el) => { el.dispatchEvent(new CompositionEvent("compositionstart")); el.value = "검"; el.dispatchEvent(new InputEvent("input", { isComposing: true })); });
  assert.deepEqual(await visible(), ["관리자"], "filter waits while composing");
  await input.evaluate((el) => { el.dispatchEvent(new CompositionEvent("compositionend", { data: "검" })); });
  assert.deepEqual(await visible(), ["검토자"]);
  await input.fill("zzz");
  assert.deepEqual(await visible(), []);
  assert.equal(await popupOf(page, "search-role").locator(".select__empty").isVisible(), true);
  await input.press("Escape");
  await trigger(page, "search-role").click();
  assert.equal(await input.inputValue(), "");
  assert.equal((await visible()).length, 5);
  await input.fill("운영");
  await input.press("Enter");
  assert.equal((await state(page, "search-role")).value, "d");
  assert.equal(page.url(), origin + "/", "Enter in search does not submit");
});

test("disabled fieldset, required feedback, reset and programmatic value stay in sync", async (t) => {
  const page = await fixture(t);
  await page.evaluate(() => { document.getElementById("locked").disabled = true; });
  await page.waitForFunction(() => document.querySelector('select[name="locked-role"] ~ .select__trigger').disabled);
  await trigger(page, "locked-role").click({ force: true });
  assert.equal((await state(page, "locked-role")).open, false);
  await page.evaluate(() => { document.getElementById("locked").disabled = false; });
  await page.waitForFunction(() => !document.querySelector('select[name="locked-role"] ~ .select__trigger').disabled);
  await page.click("#submit");
  assert.equal(page.url(), origin + "/");
  const invalid = await page.evaluate(() => { const t = document.querySelector('select[name="required-role"] ~ .select__trigger');
    const err = (t.getAttribute("aria-describedby") || "").split(/\s+/).map(id => document.getElementById(id)).find(el => el?.classList.contains("select__error"));
    return { focused: document.activeElement === t, invalid: t.getAttribute("aria-invalid"), message: err ? err.textContent.length > 0 : false }; });
  assert.deepEqual(invalid, { focused: true, invalid: "true", message: true });
  await trigger(page, "role").click();
  await popupOf(page, "role").getByRole("option", { name: "owner" }).click();
  await page.click("#reset");
  await page.waitForFunction(() => document.querySelector('select[name="role"] ~ .select__trigger .select__value').textContent === "viewer");
  await page.evaluate(() => { document.querySelector('select[name="role"]').value = "developer"; });
  await page.evaluate(async () => { const m = await import("/assets/select.js"); m.initSelects(document); });
  assert.equal((await state(page, "role")).text, "developer");
});

test("popups escape table scroll and stay in the viewport; Esc in a dialog closes only the list", async (t) => {
  for (const viewport of [{ width: 1440, height: 900 }, { width: 834, height: 700 }, { width: 390, height: 640 }]) {
    const page = await fixture(t, { viewport });
    const last = page.locator('select[name="role"][aria-label="user-13 프로젝트 역할"] ~ .select__trigger');
    await last.scrollIntoViewIfNeeded();
    await last.click();
    const box = await page.evaluate(() => { const p = document.querySelector('select[aria-label="user-13 프로젝트 역할"] ~ .select__popup').getBoundingClientRect();
      return { top: p.top, bottom: p.bottom, left: p.left, right: p.right, overflowX: document.documentElement.scrollWidth - document.documentElement.clientWidth }; });
    assert.ok(box.top >= 0 && box.bottom <= viewport.height && box.left >= 0 && box.right <= viewport.width, `${viewport.width}: ${JSON.stringify(box)}`);
    assert.equal(box.overflowX, 0);
    await page.keyboard.press("Escape");
    await page.click("#open-dialog");
    await trigger(page, "dialog-role").click();
    assert.equal((await state(page, "dialog-role")).open, true);
    const dialogBox = await popupOf(page, "dialog-role").boundingBox();
    assert.ok(dialogBox.y >= 0 && dialogBox.y + dialogBox.height <= viewport.height);
    await popupOf(page, "dialog-role").getByRole("option", { name: "시스템 관리자" }).click();
    assert.equal((await state(page, "dialog-role")).value, "admin", "options inside a modal dialog are clickable");
    await trigger(page, "dialog-role").click();
    await page.keyboard.press("Escape");
    assert.deepEqual([(await state(page, "dialog-role")).open, await page.evaluate(() => document.getElementById("dialog").open)], [false, true]);
    await page.keyboard.press("Escape");
    assert.equal(await page.evaluate(() => document.getElementById("dialog").open), false);
  }
});

// The hidden native select must stay a 1px box even where
// panel.css sizes row selects (.row-actions select), or a long option widens the page.
test("a compact select with a long option keeps the native box tiny and the page unscrolled", async (t) => {
  for (const viewport of [{ width: 390, height: 700 }, { width: 1440, height: 900 }]) {
    const page = await fixture(t, { viewport });
    const m = await page.evaluate(() => {
      const native = document.querySelector('select[name="long-role"]').getBoundingClientRect();
      return { width: native.width, height: native.height, overflowX: document.documentElement.scrollWidth - document.documentElement.clientWidth };
    });
    assert.ok(m.width <= 1 && m.height <= 1, `${viewport.width}: native box ${m.width}x${m.height}`);
    assert.equal(m.overflowX, 0, `${viewport.width}: page overflow`);
  }
});

test("without JavaScript the native select stays usable", async (t) => {
  const page = await fixture(t, { javaScriptEnabled: false });
  assert.equal(await page.locator(".select__trigger").count(), 0);
  assert.equal(await page.locator('#basic select[name="role"]').isVisible(), true);
});

test("label excludes help and required errors preserve the help description", async (t) => {
  const page = await fixture(t);
  const button = trigger(page, "required-role");
  assert.equal(await button.getAttribute("aria-label"), "필수 선택");
  assert.equal(await button.getAttribute("aria-required"), "true");
  assert.equal(await button.getAttribute("aria-describedby"), "required-help");
  await page.locator("#submit").click();
  assert.equal(await button.getAttribute("aria-invalid"), "true");
  assert.match(await button.getAttribute("aria-describedby"), /required-help/);
  await button.click();
  await popupOf(page, "required-role").getByRole("option", {name:"사용자",exact:true}).click();
  assert.equal(await button.getAttribute("aria-invalid"), null);
  assert.equal(await button.getAttribute("aria-describedby"), "required-help");
});
