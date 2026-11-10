// Authentication is confined to the ephemeral fixture file; screenshots use synthetic UI data.
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const {chromium}=require('playwright');
const config=JSON.parse(fs.readFileSync(process.argv[2],'utf8')),out=process.env.ASB_BUG_DELETE_EVIDENCE;
if(!out)throw Error('ASB_BUG_DELETE_EVIDENCE required');fs.mkdirSync(out,{recursive:true});
(async()=>{
 const browser=await chromium.launch({executablePath:'/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',headless:true});const results=[];
 try{
  for(const width of [1280,390])for(const theme of ['light','dark'])for(const state of ['success','not-confirmed','card','conflict','deleted','requested']){
   const auth=config[`${width}-${theme}-${state}`],context=await browser.newContext({viewport:{width,height:900},colorScheme:theme,timezoneId:'Asia/Seoul'});
   await context.addInitScript(t=>localStorage.setItem('asb-theme',t),theme);
   await context.addCookies([{name:auth.CookieName,value:auth.Cookie,url:auth.Base}]);
   const page=await context.newPage(),errors=[];page.on('pageerror',()=>errors.push('pageerror'));
   const notices=state==='deleted'||state==='requested';
   let response=await page.goto(auth.Base+'/admin/bug-reports/detail?id='+auth.ReportID+'&status=all');
   assert.equal(response.status(),notices?(state==='deleted'?410:404):200);
   if(!notices){
    const checkbox=page.locator('.bug-delete-card input[name=confirm]');
    assert.equal(await checkbox.isChecked(),false);assert.equal(await checkbox.getAttribute('required'),'');
    assert.equal(await page.locator('.bug-delete-card form[data-confirm-action]').count(),0);
    if(state==='conflict'){
     await page.locator('.bug-delete-card [name=expectedVersion]').evaluate(e=>e.value='stale-version');await checkbox.check();
     [response]=await Promise.all([page.waitForResponse(r=>r.request().method()==='POST'),page.locator('.bug-delete-card button').click()]);
     assert.equal(response.status(),409);assert.equal(await checkbox.isChecked(),false);
     assert.equal(await page.getByRole('link',{name:'다시 불러오기'}).count(),0);
     assert.equal(await page.locator('.notice.error').innerText(),'다른 곳에서 리포트가 바뀌었습니다. 최신 내용을 확인한 뒤 다시 삭제하세요.');
    }else if(state==='success'||state==='not-confirmed'){
     await checkbox.check();await Promise.all([page.waitForURL(u=>u.pathname==='/admin/bug-reports'),page.locator('.bug-delete-card button').click()]);
     assert.equal(await page.locator('.bug-report-table tbody tr').count(),4);
     const notice=state==='success'?page.locator('.notice-info'):page.locator('.notice.error');
     assert.equal(await notice.innerText(),state==='success'?`리포트 ${auth.ReportID}를 삭제했습니다.`:'리포트는 삭제했지만 삭제 기록을 완료로 저장하지 못했습니다.');
     assert.equal(await page.locator('.bug-report-table tbody').innerText().then(t=>t.includes(auth.ReportID)),false);
    }
   }else{
    assert.equal(await page.locator('h1').innerText(),'버그 리포트');assert.equal(await page.locator('.bug-report-meta .mono').innerText(),auth.ReportID);
    assert.equal(await page.getByRole('link',{name:'← 목록'}).count(),1);
    assert.equal(await page.locator('.bug-delete-card').count(),0);
    const text=await page.locator(state==='deleted'?'.notice-info':'.notice.error').innerText();
    assert.ok(text.endsWith(state==='deleted'?'에 관리자(admin)가 삭제했습니다.':'에 관리자(admin)가 삭제를 요청했지만 완료는 확인되지 않았습니다.'));
   }
   await page.evaluate(()=>document.fonts.ready);
   const metrics=await page.evaluate(()=>{
    const rect=e=>{const r=e.getBoundingClientRect(),s=getComputedStyle(e);return {x:r.x,y:r.y,width:r.width,height:r.height,color:s.color,background:s.backgroundColor,border:s.borderColor,padding:s.padding,font:s.font,lineHeight:s.lineHeight,gap:s.gap}};
    return {width:document.documentElement.scrollWidth,viewport:innerWidth,theme:document.documentElement.dataset.theme,card:document.querySelector('.bug-delete-card')?rect(document.querySelector('.bug-delete-card')):null,button:document.querySelector('.bug-delete-card button')?rect(document.querySelector('.bug-delete-card button')):null,notice:document.querySelector('.notice')?rect(document.querySelector('.notice')):null};
   });
   assert.equal(metrics.width,width);assert.equal(metrics.theme,theme);assert.equal(errors.length,0);
   if(state==='card'||state==='conflict'){
    assert.equal(await page.locator('.bug-detail-stack > .card').last().locator('h2').innerText(),'리포트 삭제');
    assert.equal(await page.locator('.bug-delete-card button.danger').count(),1);assert.equal(metrics.button.height,44);
   }
   const png=path.join(out,`${state}-${width}-${theme}.png`);await page.screenshot({path:png,fullPage:true});results.push({state,width,theme,png,metrics});
   if(state==='deleted'&&width===390){await page.goto(auth.Base+'/admin/bug-reports/detail?id='+auth.LongID);await page.evaluate(()=>document.fonts.ready);assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth),width);await page.screenshot({path:path.join(out,`long-id-${width}-${theme}.png`),fullPage:true});}
   if(state==='success'||state==='not-confirmed'){await page.reload();assert.equal(await page.locator('.notice-info,.notice.error').count(),0);}
   await context.close();
  }
  const auth=config['1280-light-card'],context=await browser.newContext({javaScriptEnabled:false});await context.addCookies([{name:auth.CookieName,value:auth.Cookie,url:auth.Base}]);
  const page=await context.newPage();await page.goto(auth.Base+'/admin/bug-reports/detail?id='+auth.ReportID);await page.locator('.bug-delete-card input[name=confirm]').check();await Promise.all([page.waitForURL(u=>u.pathname==='/admin/bug-reports'),page.locator('.bug-delete-card button').click()]);assert.match(await page.locator('.notice-info').innerText(),/삭제했습니다/);await context.close();
  fs.writeFileSync(path.join(out,'manifest.json'),JSON.stringify({browser:browser.version(),passed:results.length,noJS:true,longID:true,results},null,2));
 }catch(e){fs.writeFileSync(path.join(out,'failure.json'),JSON.stringify({completed:results.length,kind:e.name,message:String(e.message).replace(/https?:\/\/\S+/g,'[url]')},null,2));process.exitCode=1;}finally{await browser.close();}
})();
