// Run with Node's test runner and an installed Playwright on NODE_PATH:
// node --test source/internal/controlpanel/panel_submit_test.cjs
const { test, before, after } = require("node:test");
const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { createServer } = require("node:http");
const { join } = require("node:path");
const { chromium } = require("playwright");

let browser;
before(async () => {
  browser = await chromium.launch({ ignoreDefaultArgs: ["--disable-back-forward-cache"] });
});
after(async () => { await browser?.close(); });

async function fixture(t, { action = "/tokens/issue", failOnce = false } = {}) {
  const script = readFileSync(join(__dirname, "panel.js"));
  const posts = [];
  const form = `<form method="post" action="${action}" aria-busy="false">
    <input name="nickname" required>
    <input type="hidden" name="csrf" value="test-fixture">
    <button name="intent" value="issue">Submit</button>
    <button name="intent" value="alternate">Alternative</button>
    <button id="unavailable" disabled>Unavailable</button>
    <button type="button" id="cancel">Cancel</button>
  </form>`;
  const html = (content = "") => `<!doctype html><html><head>
    <script src="/panel.js" defer></script></head><body>${content}${form}</body></html>`;
  const server = createServer(async (req, res) => {
    if (req.url === "/panel.js") {
      res.writeHead(200, { "Content-Type": "text/javascript" }).end(script);
      return;
    }
    if (req.method === "POST") {
      let body = "";
      for await (const chunk of req) body += chunk;
      posts.push(new URLSearchParams(body));
      const failed = failOnce ? posts.length === 1 : posts.length > 1;
      await new Promise((resolve) => setTimeout(resolve, 1200));
      res.writeHead(failed ? 400 : 200, { "Content-Type": "text/html" });
      res.end(html(failed ? '<p role="alert">Try again</p>' : '<p id="issued-token">Issued once</p>'));
      return;
    }
    res.writeHead(200, { "Content-Type": "text/html" }).end(html());
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const context = await browser.newContext();
  const page = await context.newPage();
  page.setDefaultTimeout(4000);
  t.after(async () => {
    await context.close();
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
  });
  await page.goto(`http://127.0.0.1:${server.address().port}/form`);
  return { page, posts, action };
}

async function observePending(page, restore = false) {
  let resolveState;
  const state = new Promise((resolve) => { resolveState = resolve; });
  await page.exposeFunction("reportSubmissionState", resolveState);
  await page.evaluate((restore) => {
    window.addEventListener("submit", () => setTimeout(() => {
      const read = () => ({
        busy: document.querySelector("form").getAttribute("aria-busy"),
        buttons: Array.from(document.querySelectorAll("button"), (button) => button.disabled),
      });
      const pending = read();
      if (restore) window.dispatchEvent(new PageTransitionEvent("pageshow", { persisted: true }));
      window.reportSubmissionState({ pending, restored: restore ? read() : null });
    }, 20), { once: true });
  }, restore);
  return { state };
}

for (const action of ["/tokens/issue", "/projects/create"]) {
  test(`${action}: delayed double click sends once and retains success`, { timeout: 10000 }, async (t) => {
    const { page, posts } = await fixture(t, { action });
    const observation = await observePending(page);
    await page.locator("[name=nickname]").fill("example");
    const response = page.waitForResponse((r) => r.request().method() === "POST");
    // Raw pointer input lets us observe the pending state before navigation.
    const button = await page.getByRole("button", { name: "Submit", exact: true }).boundingBox();
    await page.mouse.dblclick(button.x + button.width / 2, button.y + button.height / 2, { delay: 100 });
    // Observe inside the old document: Playwright DOM queries can wait for
    // the pending navigation and otherwise inspect the response document.
    assert.deepEqual((await observation.state).pending, {
      busy: "true", buttons: [true, true, true, false],
    });
    assert.equal((await response).status(), 200);
    await page.locator("#issued-token").waitFor();
    assert.equal(posts.length, 1);
    assert.equal(posts[0].get("nickname"), "example");
    assert.equal(posts[0].get("csrf"), "test-fixture");
    assert.deepEqual(posts[0].getAll("intent"), ["issue"]);
    await page.goBack({ waitUntil: "commit" });
    assert.equal(await page.locator("form").getAttribute("aria-busy"), "false");
    assert.equal(await page.getByRole("button", { name: "Submit", exact: true }).isEnabled(), true);
  });
}

test("invalid input stays editable and Enter submits after correction", { timeout: 10000 }, async (t) => {
  const { page, posts } = await fixture(t);
  await page.getByRole("button", { name: "Submit", exact: true }).click();
  assert.equal(await page.locator("form").getAttribute("aria-busy"), "false");
  assert.equal(await page.getByRole("button", { name: "Submit", exact: true }).isEnabled(), true);
  assert.equal(posts.length, 0);
  await page.locator("[name=nickname]").fill("corrected");
  await page.locator("[name=nickname]").press("Enter");
  await page.locator("#issued-token").waitFor();
  assert.equal(posts.length, 1);
  assert.equal(posts[0].get("intent"), "issue");
});

test("cancelled document confirmation does not lock the next submission", { timeout: 10000 }, async (t) => {
  const { page, posts } = await fixture(t);
  await page.evaluate(() => {
    document.addEventListener("submit", (event) => event.preventDefault(), { once: true });
  });
  await page.locator("[name=nickname]").fill("confirmed");
  await page.getByRole("button", { name: "Submit", exact: true }).click();
  assert.equal(await page.locator("form").getAttribute("aria-busy"), "false");
  assert.equal(await page.getByRole("button", { name: "Submit", exact: true }).isEnabled(), true);
  assert.equal(posts.length, 0);
  await page.getByRole("button", { name: "Submit", exact: true }).click();
  await page.locator("#issued-token").waitFor();
  assert.equal(posts.length, 1);
});

test("a repeated requestSubmit in the same task is cancelled", { timeout: 10000 }, async (t) => {
  const { page } = await fixture(t);
  await page.locator("[name=nickname]").fill("programmatic");
  const prevented = await page.evaluate(() => {
    const events = [];
    window.addEventListener("submit", (event) => events.push(event));
    const form = document.querySelector("form");
    const button = form.querySelector("button");
    form.requestSubmit(button);
    form.requestSubmit(button);
    return events.map((event) => event.defaultPrevented);
  });
  assert.deepEqual(prevented, [false, true]);
  await page.locator("#issued-token").waitFor();
});

test("an HTTP error response permits a corrected retry", { timeout: 10000 }, async (t) => {
  const { page, posts } = await fixture(t, { failOnce: true });
  await page.locator("[name=nickname]").fill("first");
  await page.getByRole("button", { name: "Submit", exact: true }).click();
  await page.getByRole("alert").waitFor();
  assert.equal(await page.getByRole("button", { name: "Submit", exact: true }).isEnabled(), true);
  await page.locator("[name=nickname]").fill("retry");
  await page.getByRole("button", { name: "Submit", exact: true }).click();
  await page.locator("#issued-token").waitFor();
  assert.equal(posts.length, 2);
  assert.equal(posts[1].get("nickname"), "retry");
});

test("pageshow restores only the controls disabled by submission", { timeout: 10000 }, async (t) => {
  const { page } = await fixture(t);
  const observation = await observePending(page, true);
  await page.locator("[name=nickname]").fill("restore");
  const button = await page.getByRole("button", { name: "Submit", exact: true }).boundingBox();
  await page.mouse.click(button.x + button.width / 2, button.y + button.height / 2);
  assert.deepEqual(await observation.state, {
    pending: { busy: "true", buttons: [true, true, true, false] },
    restored: { busy: "false", buttons: [false, false, true, false] },
  });
  await page.locator("#issued-token").waitFor();
  await page.goBack({ waitUntil: "commit" });
  assert.equal(await page.getByRole("button", { name: "Submit", exact: true }).isEnabled(), true);
});
