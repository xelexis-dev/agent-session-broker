const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const {chromium} = require('playwright');
const config = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const out = process.env.ASB_SETTINGS_EVIDENCE;
if (!out) throw new Error('evidence directory required');
fs.mkdirSync(out, {recursive: true});
(async () => {
  const browser = await chromium.launch({executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', headless: true});
  const stem = `${config.Name}-conflict-1280-${config.Theme}`;
  let stage = 'setup';
  try {
    const contexts = [];
    for (const value of [config.CookieA, config.CookieB]) {
      const context = await browser.newContext({viewport: {width: 1280, height: 900}, colorScheme: config.Theme});
      await context.addInitScript(theme => localStorage.setItem('asb-theme', theme), config.Theme);
      await context.addCookies([{name: config.CookieName, value, url: config.Base}]);
      contexts.push(context);
    }
    const a = await contexts[0].newPage(), b = await contexts[1].newPage();
    const errors = [];
    b.on('pageerror', () => errors.push('pageerror'));
    await a.goto(config.Base + config.Page);
    await b.goto(config.Base + config.Page);
    const selector = `form[action="${config.Action}"]`;
    const original = await b.locator(`${selector} input[name="settings_version"]`).inputValue();
    const av = await a.locator(`${selector} input[name="settings_version"]`).inputValue();
    const csrf = await a.locator(`${selector} input[name="csrf"]`).inputValue();
    stage = 'first-save';
    const saved = await contexts[0].request.post(config.Base + config.Action, {form: {...config.First, csrf, settings_version: av}, maxRedirects: 0});
    assert.equal(saved.status(), 303);
    stage = 'stale-submit';
    const response = b.waitForResponse(r => r.request().method() === 'POST' && new URL(r.url()).pathname === config.Action);
    await Promise.all([b.waitForNavigation({waitUntil: "load"}), b.locator(selector).evaluate(form => form.submit())]);
    assert.equal((await response).status(), 409);
    await b.evaluate(() => document.fonts.ready);
    stage = 'conflict-view';
    assert.equal(await b.locator('.notice.error[role="alert"]').innerText(), config.Message);
    const fresh = await b.locator(`${selector} input[name="settings_version"]`).inputValue();
    assert.match(fresh, /^[a-f0-9]{64}$/);
    assert.notEqual(fresh, original);
    // Compare editable controls against a separate fresh GET, excluding credentials.
    const current = await contexts[1].newPage();
    await current.goto(config.Base + config.Page);
    const values = form => [...form.elements].filter(e => e.name && !['csrf', 'password', 'password_confirm'].includes(e.name)).map(e => ({name:e.name, value:e.value, checked:e.type === 'checkbox' ? e.checked : null}));
    assert.deepEqual(await b.locator(selector).evaluate(values), await current.locator(selector).evaluate(values));
    const metrics = await b.evaluate(() => ({theme: document.documentElement.dataset.theme, width: document.documentElement.scrollWidth, viewport: innerWidth}));
    assert.equal(metrics.theme, config.Theme);
    assert.equal(metrics.width, 1280);
    assert.equal(errors.length, 0);
    const png = path.join(out, stem + '.png');
    await b.screenshot({path: png, fullPage: true});
    fs.writeFileSync(path.join(out, stem + '.json'), JSON.stringify({browser: browser.version(), state: config.Name, theme: config.Theme, status: 409, currentValues: true, freshVersion: true, metrics, png}, null, 2));
  } catch (e) {
    fs.writeFileSync(path.join(out, stem + '-failure.json'), JSON.stringify({stage, kind: e.name, reason: String(e.message).split("\n")[0].replace(/https?:\/\/\S+/g, "[URL]")}));
    process.exitCode = 1;
  } finally { await browser.close(); }
})();
