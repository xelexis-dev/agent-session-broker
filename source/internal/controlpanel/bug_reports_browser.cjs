// Auth stays in an ephemeral file; evidence contains synthetic UI data only.
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const {chromium}=require('playwright');
const config=JSON.parse(fs.readFileSync(process.argv[2],'utf8')),out=process.env.ASB_BUG_REPORT_EVIDENCE;
if(!out)throw new Error('ASB_BUG_REPORT_EVIDENCE required');fs.mkdirSync(out,{recursive:true});
(async()=>{
 const browser=await chromium.launch({executablePath:'/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',headless:true});const results=[];
 try {
  for(const width of [1280,390])for(const theme of ['light','dark'])for(const state of ['list','empty','open','resolved','ignored','closed','conflict','previous','long']){
   const context=await browser.newContext({viewport:{width,height:900},colorScheme:theme,timezoneId:'Asia/Seoul'});
   await context.addInitScript(t=>localStorage.setItem('asb-theme',t),theme);
   const auth=state==='resolved'?config.After[`${width}-${theme}`]:config;
   await context.addCookies([{name:auth.CookieName,value:auth.Cookie,url:auth.Base}]);
   const page=await context.newPage(),errors=[];page.on('pageerror',()=>errors.push('pageerror'));
   let route='/admin/bug-reports?status=all';
   if(state==='empty')route='/admin/bug-reports?status=resolved&severity=low';
   if(!['list','empty'].includes(state))route='/admin/bug-reports/detail?id='+config.IDs[state==='conflict'?'resolved':state]+(state==='previous'?'&bucket=history':'');
   if(state==='resolved')route='/admin/bug-reports/detail?id='+auth.ReportID;
   let response=await page.goto(auth.Base+route);assert.equal(response.status(),200);
   if(state==='resolved'){
    await page.locator('#bug-note').fill('수정 배포 확인');
    await Promise.all([page.waitForResponse(r=>r.request().method()==='POST'),page.getByRole('button',{name:'해결',exact:true}).click()]);
    assert.equal(await page.locator('.notice-info').innerText(),'상태를 해결로 바꿨습니다. 목록의 현재 필터에는 표시되지 않습니다.');
   }
   if(state==='conflict'){
    await page.locator('#bug-note').fill('중복 리포트로 보여 무시하려고 했습니다. 입력한 메모 유지');
    await page.locator('.bug-status-form [name=expectedVersion]').evaluate(e=>e.value='stale-version');
    [response]=await Promise.all([page.waitForResponse(r=>r.request().method()==='POST'),page.getByRole('button',{name:'무시로 변경',exact:true}).click()]);
    assert.equal(response.status(),409);assert.match(await page.locator('#bug-note').inputValue(),/입력한 메모 유지/);assert.equal(await page.getByRole('link',{name:'다시 불러오기'}).count(),1);
    assert.equal(await page.locator('.notice.error').innerText(),'다른 곳에서 상태가 바뀌었습니다. 최신 내용을 확인하세요.');
    assert.equal(await page.locator('.notice.error a').count(),0);
    assert.equal(await page.locator('.bug-conflict a.button').count(),1);
    assert.equal(await page.locator('.notice-info').count(),0);
   }
   await page.evaluate(()=>document.fonts.ready);
   const metrics=await page.evaluate(()=>{
    const rect=e=>{const r=e.getBoundingClientRect(),s=getComputedStyle(e);return {x:r.x,y:r.y,width:r.width,height:r.height,font:s.font,lineHeight:s.lineHeight,color:s.color,background:s.backgroundColor,border:s.borderColor,padding:s.padding,gap:s.gap}};
    return {theme:document.documentElement.dataset.theme,width:document.documentElement.scrollWidth,viewport:innerWidth,cards:[...document.querySelectorAll('.bug-detail-stack .card')].map(rect),textarea:document.querySelector('#bug-note')?rect(document.querySelector('#bug-note')):null,buttons:[...document.querySelectorAll('.bug-status-actions button')].map(rect)};
   });
   assert.equal(metrics.theme,theme);assert.equal(metrics.width,width);assert.equal(errors.length,0);
   if(!['list','empty'].includes(state)){
    assert.equal(metrics.cards.length,6);
    assert.deepEqual(await page.locator('.bug-detail-stack .card h2').allTextContents(),['처리 상태','상태 기록','보고 내용','보고자','client 진단','리포트 삭제']);
    if(state==='previous')assert.equal(await page.locator('.bug-status-form').count(),0);
    else{if(state!=='conflict')assert.equal(await page.locator('#bug-note').inputValue(),'');assert.ok(metrics.textarea.height>=120);assert.ok(metrics.buttons.every(b=>b.height>=44));}
    if(state==='resolved')assert.deepEqual(await page.locator('.bug-status-actions button').allTextContents(),['다시 열기','무시로 변경']);
    if(state==='long')assert.match(await page.locator('.bug-history-card').innerText(),/이전 변경 기록 일부 생략/);
   }
   if(state==='list'){
    assert.equal(await page.locator('.bug-report-table tbody tr').count(),5);
    assert.equal(await page.locator('.bug-filters button.primary').count(),1);
    assert.deepEqual(await page.locator('.bug-filters select[name=status] option').allTextContents(),['열림','해결','무시','기존 닫힘','전체']);
    if(width===1280)assert.ok(await page.locator('.bug-col-status').evaluate(e=>e.getBoundingClientRect().width)>=104);
   }
   const png=path.join(out,`${state}-${width}-${theme}.png`);await page.screenshot({path:png,fullPage:true});results.push({state,width,theme,png,metrics});
   if(state==='resolved'){await page.reload();assert.equal(await page.locator('.notice-info').count(),0);assert.equal(await page.locator('#bug-note').inputValue(),'');}
   await context.close();
  }
  // Native form submission remains functional with JavaScript disabled.
  const context=await browser.newContext({javaScriptEnabled:false});await context.addCookies([{name:config.CookieName,value:config.Cookie,url:config.Base}]);const p=await context.newPage();await p.goto(config.Base+'/admin/bug-reports/detail?id='+config.IDs.open);await p.locator('#bug-note').fill('No JS');await Promise.all([p.waitForURL(u=>u.pathname==='/admin/bug-reports/detail'),p.getByRole('button',{name:'해결',exact:true}).click()]);assert.equal(await p.getByRole('button',{name:'무시로 변경',exact:true}).count(),1);await context.close();
  fs.writeFileSync(path.join(out,'manifest.json'),JSON.stringify({browser:browser.version(),passed:results.length,noJS:true,results},null,2));
 }catch(e){fs.writeFileSync(path.join(out,'failure.json'),JSON.stringify({completed:results.length,kind:e.name,message:String(e.message).replace(/https?:\/\/\S+/g,'[url]')},null,2));process.exitCode=1;}finally{await browser.close();}
})();
