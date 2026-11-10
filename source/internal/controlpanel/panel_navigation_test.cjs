// Run with node --test; Playwright must be available on NODE_PATH.
const { test, before, after } = require("node:test");
const assert = require("node:assert/strict");
const { chromium } = require("playwright");
const fs = require("node:fs");
const path = require("node:path");
let browser;
before(async () => { browser = await chromium.launch({ headless: true }); });
after(async () => { await browser?.close(); });

async function fixture(t, html) {
  const page = await browser.newPage();
  t.after(() => page.close());
  await page.setContent(html);
  await page.addScriptTag({ content: fs.readFileSync(path.join(__dirname, "panel.js"), "utf8") });
  return page;
}

const menuHTML = `<button id="outside">Outside</button>
<div data-user-menu><button type="button" aria-haspopup="menu" aria-expanded="false" aria-controls="user-menu-items" data-user-menu-toggle>User</button>
<div id="user-menu-items" role="menu" hidden><a role="menuitem" href="#profile">내 프로필</a><a role="menuitem" href="#admin">관리자 패널</a><form><button type="button" role="menuitem">로그아웃</button></form></div></div>`;

test("user menu opens on click, moves with arrows, closes on Escape and outside click", async (t) => {
  const page = await fixture(t, menuHTML);
  const toggle = page.locator("[data-user-menu-toggle]");
  const items = page.locator("#user-menu-items");
  assert.equal(await items.isHidden(), true);
  await toggle.click();
  assert.equal(await items.isVisible(), true);
  assert.equal(await toggle.getAttribute("aria-expanded"), "true");
  assert.equal(await page.evaluate(() => document.activeElement.textContent), "내 프로필");
  await page.keyboard.press("ArrowDown");
  assert.equal(await page.evaluate(() => document.activeElement.textContent), "관리자 패널");
  await page.keyboard.press("ArrowUp");
  await page.keyboard.press("ArrowUp");
  assert.equal(await page.evaluate(() => document.activeElement.textContent), "로그아웃");
  await page.keyboard.press("Escape");
  assert.equal(await items.isHidden(), true);
  assert.equal(await toggle.getAttribute("aria-expanded"), "false");
  assert.equal(await toggle.evaluate((el) => el === document.activeElement), true);
  await toggle.click();
  await page.locator("#outside").click();
  assert.equal(await items.isHidden(), true);
  await toggle.focus();
  await page.keyboard.press("ArrowUp");
  assert.equal(await page.evaluate(() => document.activeElement.textContent), "로그아웃");
});

const tableHTML = `<div class="table-tools"><input type="search" data-table-filter="project-table"><small data-table-filter-status role="status"></small></div>
<table id="project-table" data-row-links><tbody>
<tr class="row-link" data-filter-text="alpha"><td><a class="row-link-target" href="#alpha">alpha</a></td><td>Owner</td><td><button type="button" id="inner">Inner</button></td></tr>
<tr class="row-link" data-filter-text="beta"><td><a class="row-link-target" href="#beta">beta</a></td><td>Viewer</td><td></td></tr>
</tbody></table>`;

test("table filter hides non-matching rows and announces the count", async (t) => {
  const page = await fixture(t, tableHTML);
  const input = page.locator("[data-table-filter]");
  await input.fill("BE");
  assert.deepEqual(await page.evaluate(() => Array.from(document.querySelectorAll("tr"), (row) => row.hidden)), [true, false]);
  assert.match(await page.locator("[data-table-filter-status]").textContent(), /1개/);
  await input.fill("");
  assert.deepEqual(await page.evaluate(() => Array.from(document.querySelectorAll("tr"), (row) => row.hidden)), [false, false]);
  assert.equal(await page.locator("[data-table-filter-status]").textContent(), "");
  await input.fill("zzz");
  assert.deepEqual(await page.evaluate(() => Array.from(document.querySelectorAll("tr"), (row) => row.hidden)), [true, true]);
  assert.match(await page.locator("[data-table-filter-status]").textContent(), /0개/);
});

test("row click follows the row link except on controls", async (t) => {
  const page = await fixture(t, tableHTML);
  await page.locator("tr.row-link").nth(1).locator("td").nth(1).click();
  assert.equal(await page.evaluate(() => location.hash), "#beta");
  await page.locator("#inner").click();
  assert.equal(await page.evaluate(() => location.hash), "#beta");
  await page.locator("tr.row-link").nth(0).locator("td").nth(1).click();
  assert.equal(await page.evaluate(() => location.hash), "#alpha");
});
