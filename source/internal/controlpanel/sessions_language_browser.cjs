// Full Chrome check of the translated session pages (i18n MR-4): the sessions page and the session audit in the
// account's language, a live update that keeps the page's language, no horizontal overflow and no serious/critical
// axe findings, for en/ko × 1280/390 × light/dark. Evidence holds no credentials.
const {chromium} = require('playwright');
const axe = require('axe-core');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {catalog, msg} = require('./i18n_fixture.cjs');

const text = (lang, key) => catalog(lang)[key];

async function axeFindings(page) {
 await page.evaluate(axe.source);
 return page.evaluate(async () => {
  const result = await axe.run(document, {resultTypes: ['violations']});
  return result.violations.filter(v => ['serious', 'critical'].includes(v.impact)).map(v => ({id: v.id, impact: v.impact, count: v.nodes.length}));
 });
}

async function check(page, name, evidence, stage) {
 assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), name + ' overflows horizontally');
 await page.evaluate(() => document.fonts.ready);
 const findings = await axeFindings(page);
 assert.equal(findings.length, 0, name + ' has serious/critical axe violations');
 await page.screenshot({path: path.join(evidence, `${stage}-${name}.png`), fullPage: true});
 return findings;
}

(async () => {
 const config = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
 const evidence = process.env.ASB_SESSIONS_LANGUAGE_EVIDENCE;
 assert(evidence && process.env.ASB_FULL_CHROME);
 fs.mkdirSync(evidence, {recursive: true});
 const browser = await chromium.launch({executablePath: process.env.ASB_FULL_CHROME, args: ['--keep-alive-for-test']});
 const rows = [];
 let stage = 'start';
 try {
  // One sign-in per language (the sign-in rate limit); every viewport reuses that session.
  const sessions = {};
  for (const [language, user] of [['en', 'owner'], ['ko', 'developer']]) {
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
   const context = await browser.newContext({viewport: {width, height: 900}, colorScheme: theme, storageState: sessions[language]});
   await context.addInitScript(value => localStorage.setItem('asb-theme', value), theme);
   const page = await context.newPage();
   await page.goto(config.base + '/projects/sessions?project=demo');
   assert.equal(await page.locator('html').getAttribute('lang'), language);
   assert.equal(await page.locator('html').getAttribute('data-theme'), theme);
   assert.equal(await page.locator('#sessions-title').textContent(), text(language, 'sessions.title'));
   const badge = page.locator('#session-table [data-connection-state]').first();
   assert.equal((await badge.textContent()).trim(), text(language, 'session.connection.ready'));
   // The first poll replaces the badge and tooltip from live JSON; both stay in the page's language.
   const ok = new RegExp('^' + msg(language, 'js.sessions.status.ok', {time: '\u0000'}).replace(/[.*+?^${}()|[\]\\]/g, '\\$&').replace('\u0000', '\\d\\d:\\d\\d:\\d\\d') + '$');
   await page.waitForFunction(pattern => new RegExp(pattern).test(document.querySelector('[data-session-live-status]').textContent), ok.source, {timeout: 15000});
   assert.equal((await badge.textContent()).trim(), text(language, 'session.connection.ready'));
   const line = page.locator('#session-table [data-session-tooltip] [data-activity-line]').first();
   assert.equal(await line.textContent(), msg(language, 'js.sessions.tooltip.activity', {label: msg(language, 'js.sessions.activity.working')}));
   assert.equal(await page.locator('#session-table [data-activity-state]').first().getAttribute('aria-label'), msg(language, 'js.sessions.activity_aria', {label: msg(language, 'js.sessions.activity.working')}));
   const sessionsFindings = await check(page, 'sessions', evidence, stage);
   await page.goto(config.base + '/projects/audit?project=demo');
   assert.equal(await page.locator('html').getAttribute('lang'), language);
   assert.equal(await page.locator('#direct-audit-title').textContent(), text(language, 'title.audit'));
   assert.equal(await page.locator('#direct-audit-table caption').textContent(), text(language, 'audit.caption'));
   assert.equal((await page.locator('#direct-audit-table [data-audit-kind="registration"] td').nth(1).textContent()).trim(), text(language, 'audit.event.registration'));
   const auditFindings = await check(page, 'audit', evidence, stage);
   rows.push({language, width, theme, sessions: sessionsFindings, audit: auditFindings});
   await context.close();
  }
 } catch (error) {
  fs.writeFileSync(path.join(evidence, 'failure.json'), JSON.stringify({stage, kind: error.name, reason: 'sessions language browser/axe assertion failed'}));
  process.exitCode = 1;
 } finally {
  fs.writeFileSync(path.join(evidence, 'results.json'), JSON.stringify({axe: axe.version, rows}, null, 2));
  await browser.close();
 }
})();
