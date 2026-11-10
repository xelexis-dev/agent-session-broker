// Run with node --test; Playwright must be available on NODE_PATH.
const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const fs = require('node:fs');
const path = require('node:path');
let browser;
before(async () => { browser = await chromium.launch({ headless: true }); });
after(async () => { await browser?.close(); });

async function fixture(t, kind = 'token-revoke') {
  const page = await browser.newPage();
  t.after(() => page.close());
  await page.setContent(`<form method="post" data-confirm-action="${kind}" data-confirm-target="example" data-current-role="developer">
    <input name="required" required value="valid">
    <select name="role"><option value="developer">Developer</option><option value="viewer">Viewer</option><option value="owner">Owner</option></select>
    <button name="operation" value="original">Apply</button></form>
    <dialog id="confirm-action" aria-labelledby="confirm-action-title" aria-describedby="confirm-action-description">
      <h2 id="confirm-action-title"></h2><p id="confirm-action-description"></p>
      <button type="button" data-dialog-close autofocus>취소</button><button type="button" data-confirm-submit>확인</button>
    </dialog>`);
  await page.addScriptTag({ content: fs.readFileSync(path.join(__dirname, 'panel.js'), 'utf8') });
  // Emulate a global submission guard: canceled confirmation must never lock it.
  await page.evaluate(() => {
    window.submitted = [];
    window.addEventListener('submit', event => {
      if (event.defaultPrevented) return;
      event.preventDefault();
      window.submitted.push(event.submitter?.value);
    });
  });
  return page;
}

test('cancel and Escape restore the initiator without submitting or locking the form', async t => {
  const page = await fixture(t);
  await page.getByRole('button', { name: 'Apply' }).click();
  assert.equal(await page.locator('dialog').isVisible(), true);
  assert.match(await page.locator('#confirm-action-description').textContent(), /example/);
  assert.match(await page.locator('#confirm-action-description').textContent(), /되돌릴 수 없/);
  assert.equal(await page.getByRole('button', { name: '취소', exact: true }).evaluate(el => el === document.activeElement), true);
  await page.keyboard.press('Escape');
  assert.equal(await page.getByRole('button', { name: 'Apply' }).evaluate(el => el === document.activeElement), true);
  await page.keyboard.press('Enter');
  assert.equal(await page.locator('dialog').isVisible(), true);
  await page.getByRole('button', { name: '취소', exact: true }).click();
  assert.deepEqual(await page.evaluate(() => window.submitted), []);
  await page.getByRole('button', { name: 'Apply' }).click();
  await page.locator('[data-confirm-submit]').click();
  assert.deepEqual(await page.evaluate(() => window.submitted), ['original']);
});

test('immediate resubmission reopens confirmation and survives the previous close event', async t => {
  const page = await fixture(t);
  await page.getByRole('button', { name: 'Apply' }).click();
  const settled = await page.evaluate(() => new Promise(resolve => {
    const dialog = document.querySelector('dialog');
    const form = document.querySelector('form');
    const submitter = form.querySelector('button');
    let reopened;
    dialog.addEventListener('close', () => resolve({
      reopened,
      open: dialog.open,
      cancelFocused: document.activeElement === dialog.querySelector('[data-dialog-close]'),
      busy: form.getAttribute('aria-busy'),
      submitted: window.submitted,
    }), { once: true });
    dialog.close();
    form.dataset.confirmTarget = 'reopened target';
    form.requestSubmit(submitter);
    reopened = dialog.open;
  }));
  assert.deepEqual(settled, { reopened: true, open: true, cancelFocused: true, busy: null, submitted: [] });
  assert.match(await page.locator('#confirm-action-description').textContent(), /reopened target/);
  await page.locator('[data-confirm-submit]').click();
  assert.deepEqual(await page.evaluate(() => window.submitted), ['original']);
});

test('confirmation repeats native validation before submitting', async t => {
  const page = await fixture(t);
  await page.getByRole('button', { name: 'Apply' }).click();
  await page.locator('[name="required"]').evaluate(el => { el.value = ''; });
  await page.locator('[data-confirm-submit]').click();
  assert.deepEqual(await page.evaluate(() => window.submitted), []);
  await page.locator('[name="required"]').fill('valid again');
  await page.getByRole('button', { name: 'Apply' }).click();
  assert.equal(await page.locator('dialog').isVisible(), true);
});

test('detach and member removal show distinct target effects before keyboard confirmation', async t => {
  for (const [kind, effect] of [['session-detach', /등록·닉네임 예약·사용한 토큰을 제거/], ['member-remove', /프로젝트 토큰을 모두 취소/]]) {
    const page = await fixture(t, kind);
    await page.getByRole('button', { name: 'Apply' }).click();
    assert.match(await page.locator('#confirm-action-description').textContent(), effect);
    assert.match(await page.locator('#confirm-action-description').textContent(), /example/);
    assert.deepEqual(await page.evaluate(() => window.submitted), []);
    await page.keyboard.press('Tab');
    assert.equal(await page.locator('[data-confirm-submit]').evaluate(el => el === document.activeElement), true);
    await page.keyboard.press('Enter');
    assert.deepEqual(await page.evaluate(() => window.submitted), ['original']);
  }
});

test('role no-op is disabled, promotion submits, downgrade explains token revocation', async t => {
  const page = await fixture(t, 'member-role');
  assert.equal(await page.getByRole('button', { name: 'Apply' }).isDisabled(), true);
  await page.locator('[name="role"]').selectOption('owner');
  await page.getByRole('button', { name: 'Apply' }).click();
  assert.deepEqual(await page.evaluate(() => window.submitted), ['original']);
  assert.equal(await page.locator('dialog').isVisible(), false);
  await page.locator('[name="role"]').selectOption('viewer');
  await page.getByRole('button', { name: 'Apply' }).click();
  assert.equal(await page.locator('dialog').isVisible(), true);
  assert.match(await page.locator('#confirm-action-description').textContent(), /example/);
  assert.match(await page.locator('#confirm-action-description').textContent(), /프로젝트 토큰을 모두 취소/);
  await page.locator('[data-confirm-submit]').click();
  assert.deepEqual(await page.evaluate(() => window.submitted), ['original', 'original']);
});

test('unregistered grant release explains its exact scope and confirms once', async t => {
  const page = await fixture(t, 'session-grant-release');
  await page.getByRole('button', { name: 'Apply' }).click();
  assert.equal(await page.locator('#confirm-action-title').textContent(), '닉네임 예약 해제');
  assert.match(await page.locator('#confirm-action-description').textContent(), /example/);
  assert.equal(await page.locator('#confirm-action-description').textContent(), "example 세션 토큰을 취소하고 닉네임 예약을 해제합니다. 되돌릴 수 없습니다.");
  await page.getByRole('button', { name: '취소', exact: true }).click();
  assert.deepEqual(await page.evaluate(() => window.submitted), []);
  await page.getByRole('button', { name: 'Apply' }).click();
  await page.locator('[data-confirm-submit]').click();
  assert.deepEqual(await page.evaluate(() => window.submitted), ['original']);
});

 test('announcement confirmation names scope and body before submitting once', async t => {
 const page = await fixture(t, 'broker-announcement');
 await page.evaluate(() => {
 const form=document.querySelector('form');
 const select=document.createElement('select');select.name='project';select.innerHTML='<option value="">전체 프로젝트</option><option value="demo">demo</option>';form.append(select);
 const body=document.createElement('textarea');body.name='body';body.value='공지 <example>';form.append(body);
 });
 await page.getByRole('button', {name:'Apply',exact:true}).click();
 await page.locator('#confirm-action').waitFor({state:'visible'});
 const text=await page.locator('#confirm-action-description').textContent();
 assert.match(text,/전체 프로젝트/);assert.match(text,/공지 <example>/);assert.match(text,/수신 준비된 세션에 broker-admin으로 보냅니다. 답신은 받을 수 없습니다./);
 await page.locator('[data-confirm-submit]').click();assert.equal(await page.evaluate(()=>window.submitted.length),1);
 });

test('scope issuance enable explains approval-free registration before confirmation; OFF submits directly', async t => {
  const page = await fixture(t, 'scope-issuance');
  await page.evaluate(() => {
    const input=document.createElement('input');input.name='enabled';input.value='true';
    document.querySelector('form').append(input);
  });
  await page.locator('form button').click();
  assert.equal(await page.locator('#confirm-action').evaluate(d=>d.open),true);
  const text=await page.locator('#confirm-action-description').textContent();
  assert.equal(text, '제한 권한·자동 등록 토큰 발급을 켭니다. 사용자가 이 대화의 연결을 요청하면 승인 없이 등록합니다. 새 대화가 시작되는 것만으로 등록하지 않습니다. 켜시겠습니까?');
  assert.deepEqual(await page.evaluate(()=>window.submitted),[]);
  await page.locator('[data-dialog-close]').click();
  assert.deepEqual(await page.evaluate(()=>window.submitted),[]);
  await page.locator('form button').click();
  await page.locator('[data-confirm-submit]').click();
  assert.deepEqual(await page.evaluate(()=>window.submitted),['original']);
  await page.evaluate(()=>{document.querySelector('[name=enabled]').value='false';window.submitted=[];});
  await page.locator('form button').click();
  assert.deepEqual(await page.evaluate(()=>window.submitted),['original']);
});
