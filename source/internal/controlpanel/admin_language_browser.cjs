// Full Chrome check of the translated admin pages and session approval (i18n MR-5): each page in the account's
// language, no horizontal overflow and no serious/critical axe findings, for en/ko × 1280/390 × light/dark.
// Evidence holds no credentials.
const {chromium} = require('playwright');
const axe = require('axe-core');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {catalog} = require('./i18n_fixture.cjs');

function text(lang, key, n) {
 const value = catalog(lang)[key];
 const form = typeof value === 'object' ? (n === 1 ? value.one : value.other) : value;
 return n === undefined ? form : form.replace('{n}', String(n));
}

async function axeFindings(page) {
 await page.evaluate(axe.source);
 return page.evaluate(async () => {
  const result = await axe.run(document, {resultTypes: ['violations']});
  return result.violations.filter(v => ['serious', 'critical'].includes(v.impact)).map(v => ({id: v.id, impact: v.impact, count: v.nodes.length}));
 });
}

(async () => {
 const config = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
 const evidence = process.env.ASB_ADMIN_LANGUAGE_EVIDENCE;
 assert(evidence && process.env.ASB_FULL_CHROME);
 fs.mkdirSync(evidence, {recursive: true});
 const pages = [
  ['users', '/admin/users', '#user-list-title', 'admin.users.registered'],
  ['user-detail', '/admin/users/detail?id=owner', '#user-identity-title', 'admin.users.account'],
  ['settings', '/admin/settings', '#admin-api-title', 'admin.api.title'],
  ['bug-reports', '/admin/bug-reports', 'section.bug-list h2', 'bug.total', 1],
  ['bug-report', '/admin/bug-reports/detail?id=' + encodeURIComponent(config.report), '#bug-status-title', 'bug.handling'],
  ['approval', '/auth/code', '#authcode-title', 'device.check_title'],
 ];
 const browser = await chromium.launch({executablePath: process.env.ASB_FULL_CHROME, args: ['--keep-alive-for-test']});
 const rows = [];
 let stage = 'start';
 try {
  // One sign-in per language (the sign-in rate limit); every viewport reuses that session.
  const sessions = {};
  for (const [language, user] of [['en', 'admin'], ['ko', 'admin-ko']]) {
   stage = 'sign-in-' + language;
   const context = await browser.newContext();
   const page = await context.newPage();
   await page.goto(config.base + '/login');
   await page.locator('input[name=username]').fill(user);
   await page.locator('input[name=password]').fill(config.password);
   await Promise.all([page.waitForNavigation(), page.locator('form[action="/login"] button').click()]);
   sessions[language] = await context.storageState();
   await context.close();
  }
  for (const width of [1280, 390]) for (const theme of ['light', 'dark']) for (const language of ['en', 'ko']) {
   const context = await browser.newContext({viewport: {width, height: 900}, colorScheme: theme, storageState: sessions[language]});
   await context.addInitScript(value => localStorage.setItem('asb-theme', value), theme);
   const page = await context.newPage();
   for (const [name, route, selector, key, n] of pages) {
    stage = `${language}-${width}-${theme}-${name}`;
    await page.goto(config.base + route);
    assert.equal(await page.locator('html').getAttribute('lang'), language);
    assert.equal(await page.locator('html').getAttribute('data-theme'), theme);
    assert.equal((await page.locator(selector).first().textContent()).trim(), text(language, key, n));
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), name + ' overflows horizontally');
    await page.evaluate(() => document.fonts.ready);
    const findings = await axeFindings(page);
    assert.equal(findings.length, 0, name + ' has serious/critical axe violations');
    await page.screenshot({path: path.join(evidence, stage + '.png'), fullPage: true});
    rows.push({language, width, theme, page: name, findings});
   }
   await context.close();
  }
 } catch (error) {
  fs.writeFileSync(path.join(evidence, 'failure.json'), JSON.stringify({stage, kind: error.name, reason: 'admin language browser/axe assertion failed'}));
  process.exitCode = 1;
 } finally {
  fs.writeFileSync(path.join(evidence, 'results.json'), JSON.stringify({axe: axe.version, rows}, null, 2));
  await browser.close();
 }
})();
