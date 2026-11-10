// Synthetic authenticated fixtures only. Never print the private input.
const fs=require('node:fs');
const path=require('node:path');
const assert=require('node:assert/strict');
const {chromium}=require('playwright');
const config=JSON.parse(fs.readFileSync(process.argv[2],'utf8'));
const out=process.env.ASB_HISTORY_EVIDENCE;
if(!out) throw new Error('ASB_HISTORY_EVIDENCE is required');
fs.mkdirSync(out,{recursive:true});
const notices={unknown:'프로젝트 세대를 확인할 수 없는 기록입니다. 현재 프로젝트의 기록과 구분해 표시합니다.',previous:'현재 프로젝트와 다른 세대의 기록입니다.',mixed:'현재 세대에 속하지 않거나 세대를 확인할 수 없는 기록입니다.'};
(async()=>{
 const browser=await chromium.launch({executablePath:'/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',headless:true});
 const results=[];
 try{
  for(const theme of ['light','dark'])for(const state of ['unknown','previous','mixed']) {
   const auth=config[state];
   const context=await browser.newContext({viewport:{width:1280,height:900},colorScheme:theme,timezoneId:'Asia/Seoul'});
   await context.addInitScript(t=>localStorage.setItem('asb-theme',t),theme);
   await context.addCookies([{name:auth.CookieName,value:auth.Cookie,url:auth.Base}]);
   const page=await context.newPage();const errors=[];page.on('pageerror',()=>errors.push('pageerror'));
   const routes=[['audit','/projects/audit?project=demo&bucket=history'],['reports','/admin/bug-reports?bucket=history']];
   if(state!=='mixed')routes.push(['detail',`/admin/bug-reports/detail?bucket=history&id=bug-${state}-001`]);
   for(const [view,route] of routes){
    const response=await page.goto(auth.Base+route);assert.equal(response.status(),200);
    await page.evaluate(()=>document.fonts.ready);
    assert.equal(await page.locator('.notice[role="status"]').innerText(),notices[state]);
    if(view==='detail'){
     const back=page.getByRole('link',{name:'← 목록',exact:true});
     const target=new URL(await back.getAttribute('href'),auth.Base);
     assert.equal(target.pathname,'/admin/bug-reports');assert.equal(target.searchParams.get('bucket'),'history');
     assert.equal(await page.getByText('이전 세대 리포트는 읽기 전용입니다.',{exact:true}).count(),1);
    }else assert.equal(await page.locator('a[aria-current="page"]').filter({hasText:'이전 기록'}).count(),1);
    assert.equal(await page.locator('form[action^="/admin/bug-reports/status"]').count(),0);
    assert.equal(await page.locator('a[href^="/projects/settings?project="],a[href^="/projects/sessions?project="]').count(),0);
    if(view==='reports')assert.equal(await page.locator('input[name="bucket"]').inputValue(),'history');
    const metrics=await page.evaluate(()=>{
     const notice=document.querySelector('.notice');const r=notice.getBoundingClientRect();const s=getComputedStyle(notice);
     return {theme:document.documentElement.dataset.theme,width:document.documentElement.scrollWidth,viewport:innerWidth,notice:{x:r.x,y:r.y,width:r.width,height:r.height,color:s.color,background:s.backgroundColor},contentWidth:notice.scrollWidth,boxWidth:notice.clientWidth};
    });
    assert.equal(metrics.theme,theme);assert.equal(metrics.width,1280);assert.ok(metrics.contentWidth<=metrics.boxWidth);assert.equal(errors.length,0);
    const png=path.join(out,`${view}-${state}-1280-${theme}.png`);await page.screenshot({path:png,fullPage:true});
    results.push({view,state,theme,png,metrics});
   }
   await context.close();
  }
  fs.writeFileSync(path.join(out,'manifest.json'),JSON.stringify({browser:browser.version(),passed:results.length,results},null,2));
 }catch(e){
  fs.writeFileSync(path.join(out,'failure.json'),JSON.stringify({completed:results.length,kind:e.name,message:String(e.message).replace(/https?:\/\/\S+/g,'[url]')},null,2));process.exitCode=1;
 }finally{await browser.close()}
})();
