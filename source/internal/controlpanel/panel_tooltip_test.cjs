const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const { chromium } = require('playwright');
let browser, server, origin;
const badge = i => `<span data-tooltip class="asb-tooltip"><button type="button" class="asb-tooltip__trigger" aria-describedby="tip-${i}"><span class="badge">대기 ${i}</span></button><span id="tip-${i}" role="tooltip" class="asb-tooltip__content" popover="manual"><code>missing</code><br>수신 연결 실패<br>상태 응답 대기</span></span>`;
before(async () => {
  server = http.createServer((req, res) => {
    res.setHeader('Content-Security-Policy', "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'");
    if (req.url === '/') {
      res.setHeader('Content-Type', 'text/html; charset=utf-8');
      res.end(`<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/tokens.css"><link rel="stylesheet" href="/focus.css"><link rel="stylesheet" href="/panel.css"><link rel="stylesheet" href="/tooltip.css"><script type="module" src="/tooltip.js"></script></head><body class="asb"><button id="outside">밖</button><div class="table-scroll"><table><tbody>${Array.from({length:30}, (_, i) => `<tr><td>${badge(i)}</td></tr>`).join('')}</tbody></table></div></body></html>`);
    } else {
      const name = req.url.slice(1);
      const manifest = JSON.parse(fs.readFileSync(path.join(__dirname, 'asset-manifest.json')));
      const file = manifest.assets.find(a => a.path === `/assets/${name}`)?.file || name;
      if (!['focus.css','tokens.css','panel.css','tooltip.css','tooltip.js'].includes(name)) { res.writeHead(404).end(); return; }
      res.setHeader('Content-Type', name.endsWith('.css') ? 'text/css' : 'text/javascript');
      res.end(fs.readFileSync(path.join(__dirname, file)));
    }
  });
  await new Promise(r => server.listen(0, '127.0.0.1', r));
  origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM_EXECUTABLE });
});
after(async () => { await browser?.close(); await new Promise(r => server.close(r)); });
async function fixture(t, options = {}) {
  const context = await browser.newContext({ viewport: {width:390,height:700}, ...options });
  const page = await context.newPage(); const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  await page.addInitScript(() => { window.violations=[]; document.addEventListener('securitypolicyviolation', e => window.violations.push(e.violatedDirective)); });
  t.after(async () => { try { assert.deepEqual(errors, []); assert.deepEqual(await page.evaluate(() => window.violations), []); } finally { await context.close(); } });
  await page.goto(origin); await page.waitForFunction(() => document.querySelector('.asb-tooltip__trigger').getAttribute('aria-describedby'));
  return page;
}
async function opened(page, n) { await page.waitForFunction(n => document.getElementById(`tip-${n}`).matches(':popover-open'), n); }
async function closed(page, n) { await page.waitForFunction(n => !document.getElementById(`tip-${n}`).matches(':popover-open'), n); }
test('hover and focus open; mouseleave, blur and Escape close; description is accessible', async t => {
  const page = await fixture(t);
  const trigger = page.locator('.asb-tooltip__trigger').first();
  await trigger.hover(); await opened(page,0);
  assert.equal(await trigger.getAttribute('title'),null);
  assert.match(await page.locator('#tip-0').innerText(), /missing.*\n수신 연결 실패/);
  await page.mouse.move(350,10); await closed(page,0);
  await trigger.focus(); await opened(page,0);
  await page.keyboard.press('Escape'); await closed(page,0);
  await page.locator('#outside').focus(); await trigger.focus(); await opened(page,0);
  await page.locator('#outside').focus(); await closed(page,0);
});
test('touch toggles and another tooltip dismisses the previous one', async t => {
  const page = await fixture(t,{hasTouch:true}); const triggers=page.locator('.asb-tooltip__trigger');
  await triggers.nth(0).tap(); await opened(page,0);
  await triggers.nth(0).tap(); await closed(page,0);
  await triggers.nth(0).tap(); await opened(page,0);
  await triggers.nth(5).tap(); await opened(page,5); await closed(page,0);
  await page.locator('#outside').tap(); await closed(page,5);
});
test('top-layer tooltip stays inside viewport after scrolling, without inline styles', async t => {
  for (const width of [390,1440]) {
    const page = await fixture(t,{viewport:{width,height:700}});
    await page.locator('.asb-tooltip__trigger').last().scrollIntoViewIfNeeded();
    await page.locator('.asb-tooltip__trigger').last().focus(); await opened(page,29);
    const b = await page.locator('#tip-29').boundingBox();
    assert.ok(b.x >= 0 && b.y >= 0 && b.x+b.width <= width && b.y+b.height <=700, JSON.stringify(b));
    assert.equal(await page.locator('[data-tooltip] [style]').count(),0);
  }
});
