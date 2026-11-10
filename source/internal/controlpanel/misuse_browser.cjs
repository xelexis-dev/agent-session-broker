const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const {chromium} = require('playwright');
const config = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const out = process.env.ASB_MISUSE_EVIDENCE;
if (!out) throw new Error('ASB_MISUSE_EVIDENCE is required');
fs.mkdirSync(out, {recursive:true});
(async () => {
 const browser = await chromium.launch({executablePath:'/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',headless:true});
 const results=[];
 try {
  for (const theme of ['light','dark']) {
   const context=await browser.newContext({viewport:{width:1280,height:900},colorScheme:theme});
   await context.addInitScript(t=>localStorage.setItem('asb-theme',t),theme);
   await context.addCookies([{name:config.CookieName,value:config.Cookie,url:config.Base}]);
   const page=await context.newPage();const errors=[];page.on('pageerror',()=>errors.push('pageerror'));
   const response=await page.goto(config.Base+'/projects/settings?project=demo');assert.equal(response.status(),200);
   const form=page.locator('form[action="/tokens/project"]');
   await form.locator('input[name="label"]').fill('중복 제출 확인');
   const data=await form.evaluate(f=>new URLSearchParams(new FormData(f)).toString());
   const first=await context.request.post(config.Base+'/tokens/project',{data,headers:{'content-type':'application/x-www-form-urlencoded'},maxRedirects:0});
   assert.equal(first.status(),303);
   const [replay]=await Promise.all([page.waitForNavigation(),form.evaluate(f=>f.requestSubmit())]);
   assert.equal(replay.status(),409);
   await page.evaluate(()=>document.fonts.ready);
   assert.equal(await page.locator('.notice.error').innerText(),'발급 양식이 만료되었거나 이미 사용되었습니다. 내용을 확인한 뒤 다시 발급해 주세요.');
   assert.equal(await page.locator('form[action="/tokens/project"] input[name="label"]').inputValue(),'중복 제출 확인');
   assert.equal(await page.locator('#issued-token').count(),0);
   const fresh=await page.locator('input[name="issuance_token"]').inputValue();assert.notEqual(fresh,new URLSearchParams(data).get('issuance_token'));
   const metrics=await page.evaluate(()=>{
    const e=document.querySelector('.notice.error');const r=e.getBoundingClientRect();const s=getComputedStyle(e);
    return {theme:document.documentElement.dataset.theme,scrollWidth:document.documentElement.scrollWidth,viewport:innerWidth,notice:{x:r.x,y:r.y,width:r.width,height:r.height,color:s.color,background:s.backgroundColor,font:s.font}};
   });
   assert.equal(metrics.theme,theme);assert.equal(metrics.scrollWidth,1280);assert.equal(errors.length,0);
   const png=path.join(out,`duplicate-token-1280-${theme}.png`);await page.screenshot({path:png,fullPage:true});
   results.push({theme,status:409,labelRetained:true,newForm:true,tokenHidden:true,png,metrics});await context.close();
  }
  fs.writeFileSync(path.join(out,'manifest.json'),JSON.stringify({browser:browser.version(),passed:results.length,results},null,2));
 } catch(e) {
  // Do not serialize request data, assertion operands or URLs.
  fs.writeFileSync(path.join(out,'failure.json'),JSON.stringify({completed:results.length,kind:e.name},null,2));process.exitCode=1;
 } finally {await browser.close()}
})();
