// Full Chrome check of the translated shell (i18n MR-2): login with and without the asb_lang cookie, the signed-in
// shell in the account's language, the JSON message block under the real CSP, the mobile sidebar, no horizontal
// overflow and no serious/critical axe findings, for en/ko × 1280/390 × light/dark. Evidence holds no credentials.
const {chromium} = require('playwright');
const axe = require('axe-core');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {msg} = require('./i18n_fixture.cjs');

const shell = {
 en: {signIn: 'Sign in', nav: 'Main navigation', projects: 'Projects', menu: 'Open menu'},
 ko: {signIn: '로그인', nav: '주 메뉴', projects: '프로젝트 목록', menu: '메뉴 열기'},
};

async function axeFindings(page) {
 await page.evaluate(axe.source);
 return page.evaluate(async () => {
  const result = await axe.run(document, {resultTypes: ['violations']});
  return result.violations.filter(v => ['serious', 'critical'].includes(v.impact)).map(v => ({id: v.id, impact: v.impact, count: v.nodes.length}));
 });
}

(async () => {
 const config = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
 const evidence = process.env.ASB_SHELL_LANGUAGE_EVIDENCE;
 assert(evidence && process.env.ASB_FULL_CHROME);
 fs.mkdirSync(evidence, {recursive: true});
 const browser = await chromium.launch({executablePath: process.env.ASB_FULL_CHROME, args: ['--keep-alive-for-test']});
 const rows = [];
 let stage = 'start';
 try {
  // One sign-in per language (the sign-in rate limit); every viewport reuses that session.
  const sessions = {};
  for (const [language, user] of [['en', 'admin'], ['ko', 'owner']]) {
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
   stage = `${language}-${width}-${theme}`;
   const words = shell[language];
   // Before sign-in: the cookie copied from the last account decides, English without it.
   const guest = await browser.newContext({viewport: {width, height: 900}, colorScheme: theme});
   await guest.addInitScript(value => localStorage.setItem('asb-theme', value), theme);
   if (language === 'ko') await guest.addCookies([{name: 'asb_lang', value: 'ko', url: config.base}]);
   const login = await guest.newPage();
   await login.goto(config.base + '/login');
   assert.equal(await login.locator('html').getAttribute('lang'), language);
   assert.equal((await login.locator('h1').textContent()).trim(), words.signIn);
   assert(await login.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'login overflows horizontally');
   await login.evaluate(() => document.fonts.ready);
   const loginFindings = await axeFindings(login);
   assert.equal(loginFindings.length, 0, 'login has serious/critical axe violations');
   await login.screenshot({path: path.join(evidence, stage + '-login.png')});
   await guest.close();
   // Signed in: the account language drives the shell and the script messages.
   const context = await browser.newContext({viewport: {width, height: 900}, colorScheme: theme, storageState: sessions[language]});
   await context.addInitScript(value => localStorage.setItem('asb-theme', value), theme);
   const page = await context.newPage();
   await page.goto(config.base + '/');
   assert.equal(await page.locator('html').getAttribute('lang'), language);
   assert.equal(await page.locator('html').getAttribute('data-theme'), theme);
   assert.equal(await page.locator('nav.nav').getAttribute('aria-label'), words.nav);
   await page.locator('[data-table-filter="project-table"]').fill('demo');
   await page.waitForFunction(text => document.querySelector('[data-table-filter-status]')?.textContent === text, msg(language, 'js.filter.count', {n: 1}));
   assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'shell overflows horizontally');
   if (width === 390) {
    const toggle = page.getByRole('button', {name: words.menu});
    await toggle.click();
    await page.locator('#panel-sidebar').getByText(words.projects, {exact: true}).first().waitFor({state: 'visible'});
   } else {
    assert(await page.locator('#panel-sidebar').getByText(words.projects, {exact: true}).first().isVisible());
   }
   await page.evaluate(() => document.fonts.ready);
   const findings = await axeFindings(page);
   assert.equal(findings.length, 0, 'shell has serious/critical axe violations');
   await page.screenshot({path: path.join(evidence, stage + '-shell.png')});
   rows.push({language, width, theme, login: loginFindings, shell: findings});
   await context.close();
  }
 } catch (error) {
  fs.writeFileSync(path.join(evidence, 'failure.json'), JSON.stringify({stage, kind: error.name, reason: 'shell language browser/axe assertion failed'}));
  process.exitCode = 1;
 } finally {
  fs.writeFileSync(path.join(evidence, 'results.json'), JSON.stringify({axe: axe.version, rows}, null, 2));
  await browser.close();
 }
})();
