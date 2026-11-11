const {test}=require('node:test');
const { useCatalog } = require('./i18n_fixture.cjs');
const assert=require('node:assert/strict');
const {chromium}=require('playwright');
const {readFileSync}=require('node:fs');
const {join}=require('node:path');

for(const [zone,expected] of [['Asia/Seoul','2026-10-14 14:12:34'],['America/Los_Angeles','2026-10-13 22:12:34']]) {
 test(`audit times render locally with whole seconds in ${zone}`,async()=>{
  const browser=await chromium.launch();
  try {
   const page=await browser.newPage({timezoneId:zone});
   const errors=[];page.on('pageerror',error=>errors.push(error.message));
   const machine='2026-10-14T05:12:34.123456Z';
   await page.setContent(`<time data-local-time datetime="${machine}">${machine}</time><time id="invalid" data-local-time datetime="invalid">invalid</time><a href="/projects/sessions?project=demo#session-registration:fixture">세션 관리 · 해제</a>`);
   await useCatalog(page,'ko');
   await page.addScriptTag({content:readFileSync(join(__dirname,'panel.js'),'utf8')});
   assert.equal(await page.locator('time').first().textContent(),expected);
   assert.equal(await page.locator('time').first().getAttribute('datetime'),machine);
   assert.equal(await page.locator('#invalid').textContent(),'시각 확인 불가');
   assert.deepEqual(errors,[]);
  } finally {await browser.close();}
 });
}
