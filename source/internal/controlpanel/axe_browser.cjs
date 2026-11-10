const {chromium} = require('playwright');
const axe = require('axe-core');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

(async () => {
  const config = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
  const out = process.env.ASB_AXE_EVIDENCE;
  assert(out && process.env.ASB_FULL_CHROME);
  assert.equal(axe.version, '4.11.0');
  fs.mkdirSync(out, {recursive: true});
  const browser = await chromium.launch({executablePath: process.env.ASB_FULL_CHROME});
  const rows = [];
  let stage = 'guard';
  try {
    const probe = await browser.newPage();
    await probe.setContent('<html lang="en"><title>Probe</title><main><input></main></html>');
    await probe.evaluate(axe.source);
    assert(await probe.evaluate(async () => (await axe.run()).violations.some(v => v.id === 'label' && v.impact === 'critical')));
    await probe.close();
    for (const theme of ['light', 'dark']) {
      const context = await browser.newContext({viewport: {width: 1440, height: 1100}, colorScheme: theme});
      await context.addInitScript(t => localStorage.setItem('asb-theme', t), theme);
      const page = await context.newPage();
      const settings = '/projects/settings?project=demo';
      const screens = {
        setup: ['/setup', 'form[action="/setup"]'],
        login: ['/login', 'form[action="/login"]'],
        projects: ['/', '#project-list-title'],
        tokens: [settings, '#project-token-title'],
        members: [settings, '#add-project-member:modal'],
        'scope-editor': [settings + '&edit=' + config.parent, '#scope-editor:modal'],
        users: ['/admin/users', '#user-list-title'],
        'user-detail': ['/admin/users/detail?id=owner', 'form[action="/admin/users/update"]'],
        devices: ['/auth/code?code=' + encodeURIComponent(config.device), 'form[action="/auth/code"]:has(input[name="action"][value="approve"])'],
        sessions: ['/projects/sessions?project=demo', '#sessions-title'],
        'instance-settings': ['/admin/settings', '#instance-policies-title'],
        announcements: ['/admin/settings', '#announcement-title'],
        'select-open': ['/admin/settings', '.select__popup:popover-open [role="listbox"]'],
        'confirm-open': ['/admin/settings', '#confirm-action:modal #confirm-action-description'],
      };
      async function scan(screen, ready) {
        stage = screen + '-' + theme;
        if (ready) await ready();
        const [route, marker] = screens[screen];
        const expected = new URL(route, screen === 'setup' ? config.setup : config.base);
        const actual = new URL(page.url());
        // Compare without logging query values, which can include approval codes.
        assert(actual.origin === expected.origin && actual.pathname === expected.pathname && actual.search === expected.search, 'unexpected screen URL');
        assert.equal(await page.locator(marker).count(), 1, 'unique screen marker required');
        await page.locator(marker).waitFor({state: 'visible'});
        if (screen === 'user-detail') {
          assert(await page.locator(marker + ' input[name="id"]').inputValue() === 'owner', 'user-detail fixture target required');
        }
        await page.evaluate(() => document.fonts.ready);
        assert.equal(await page.locator('html').getAttribute('data-theme'), theme);
        await page.evaluate(axe.source);
        // Keep selectors/counts only: axe HTML and URLs can contain fixture credentials.
        const result = await page.evaluate(async () => {
          const r = await axe.run(document, {resultTypes: ['violations', 'incomplete']});
          const summarize = v => ({id: v.id, impact: v.impact, nodes: v.nodes.map(n => n.target)});
          return {violations: r.violations.map(summarize), incomplete: r.incomplete.map(summarize), contrastChecked: r.passes.some(v => v.id === 'color-contrast')};
        });
        const counts = Object.fromEntries(['critical', 'serious', 'moderate', 'minor'].map(impact => [impact, result.violations.filter(v => v.impact === impact).reduce((n, v) => n + v.nodes.length, 0)]));
        rows.push({screen, theme, counts, ...result});
        assert(result.contrastChecked, 'color contrast must run on every screen');
        await page.screenshot({path: path.join(out, stage + '.png'), fullPage: true});
      }
      await page.goto(config.setup + '/setup'); await scan('setup');
      await page.goto(config.base + '/login'); await scan('login');
      await page.locator('[name=username]').fill('admin');
      await page.locator('[name=password]').fill(config.password);
      await Promise.all([page.waitForURL(config.base + '/'), page.locator('form[action="/login"] button').click()]);
      await scan('projects');
      await page.goto(config.base + '/projects/settings?project=demo'); await scan('tokens');
      await scan('members', () => page.locator('[data-dialog-open="add-project-member"]').click());
      await page.keyboard.press('Escape');
      await page.goto(config.base + '/projects/settings?project=demo&edit=' + config.parent);
      await scan('scope-editor', () => page.waitForFunction(() => document.querySelector('#scope-editor')?.matches(':modal')));
      await page.goto(config.base + '/admin/users'); await scan('users');
      await page.goto(config.base + '/admin/users/detail?id=owner'); await scan('user-detail');
      await page.goto(config.base + '/auth/code?code=' + encodeURIComponent(config.device)); await scan('devices');
      await page.goto(config.base + '/projects/sessions?project=demo'); await scan('sessions');
      await page.goto(config.base + '/admin/settings'); await scan('instance-settings');
      await scan('announcements', () => page.locator('form[action="/admin/announcement"]').scrollIntoViewIfNeeded());
      await scan('select-open', async () => {
        await page.locator('.select__trigger').first().click();
        await page.locator('.select__popup:popover-open').waitFor();
      });
      await page.keyboard.press('Escape');
      await scan('confirm-open', async () => {
        await page.locator('form[data-confirm-action="http-policy"] button').click();
        await page.locator('#confirm-action:modal').waitFor();
      });
      await context.close();
    }
    assert.equal(rows.length, 28);
    assert.equal(new Set(rows.map(row => row.screen + '/' + row.theme)).size, 28);
    stage = 'violations';
    assert(rows.every(r => r.counts.critical === 0 && r.counts.serious === 0), 'serious/critical axe violations');
  } catch (e) {
    fs.writeFileSync(path.join(out, 'failure.json'), JSON.stringify({stage, kind: e.name, reason: 'axe check or screen readiness failed; inspect sanitized results'}));
    process.exitCode = 1;
  } finally {
    fs.writeFileSync(path.join(out, 'axe-results.json'), JSON.stringify({axeVersion: axe.version, browser: browser.version(), rows}, null, 2));
    await browser.close();
  }
})();
