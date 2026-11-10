const fs=require('node:fs');const path=require('node:path');const assert=require('node:assert/strict');
const {chromium}=require('playwright');const config=JSON.parse(fs.readFileSync(process.argv[2],'utf8'));
const out=process.env.ASB_ROUND2_EVIDENCE;if(!out)throw new Error('evidence directory required');fs.mkdirSync(out,{recursive:true});
(async()=>{
 const browser=await chromium.launch({executablePath:'/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',headless:true});const results=[];
 try{
  for(const theme of ['light','dark']){
   const context=await browser.newContext({viewport:{width:1280,height:900},colorScheme:theme});
   await context.addInitScript(t=>localStorage.setItem('asb-theme',t),theme);
   await context.addCookies([{name:config.CookieName,value:config.Cookie,url:config.Base}]);
   const page=await context.newPage();const errors=[];page.on('pageerror',()=>errors.push('pageerror'));
   const response=await page.goto(config.Base+'/projects/audit?project=demo');assert.equal(response.status(),200);
   await page.evaluate(()=>document.fonts.ready);
   const body=await page.locator('body').innerText();assert.ok(body.includes('표시할 세션 감사 이력이 없습니다.'));assert.ok(!body.includes('previous-owner-private-session'));
   const metrics=await page.evaluate(()=>({theme:document.documentElement.dataset.theme,width:document.documentElement.scrollWidth,viewport:innerWidth}));
   assert.equal(metrics.theme,theme);assert.equal(metrics.width,1280);assert.equal(errors.length,0);
   const png=path.join(out,`recreated-audit-1280-${theme}.png`);await page.screenshot({path:png,fullPage:true});results.push({theme,status:200,oldAuditHidden:true,emptyShown:true,png,metrics});await context.close();
  }
  fs.writeFileSync(path.join(out,'manifest.json'),JSON.stringify({browser:browser.version(),passed:results.length,results},null,2));
 }catch(e){fs.writeFileSync(path.join(out,'failure.json'),JSON.stringify({completed:results.length,kind:e.name}));process.exitCode=1}
 finally{await browser.close()}
})();
