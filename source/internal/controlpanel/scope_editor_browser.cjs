const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const {chromium} = require('playwright');
const cfg = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const out = process.env.ASB_SCOPE_PANEL_EVIDENCE;
if (!out) throw new Error('evidence directory required');
fs.mkdirSync(out, {recursive:true});
let stage = 'launch';
(async () => {
 const browser = await chromium.launch({executablePath:'/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',headless:true});
 const manifest = {browser:browser.version(),sourceSHA:process.env.ASB_SCOPE_PANEL_SOURCE_SHA,sourceDigest:cfg.SourceDigest,captures:[],checks:[]};
 const contexts = [];
 const baseline=['memory:read','message:send','session:read','session:register','session:rename'];
 const general=['session:read','session:rename','session:manage','message:send','message:receive','memory:read','memory:write'];
 async function context(width,theme,actor='Owner',js=true) {
  const ctx = await browser.newContext({viewport:{width,height:900},colorScheme:theme,javaScriptEnabled:js,reducedMotion:'reduce'});
  if (js) await ctx.addInitScript(theme => localStorage.setItem('asb-theme',theme),theme);
  await ctx.addCookies([{name:cfg.CookieName,value:cfg[actor+'Cookie'],url:cfg.Base}]);
  ctx.setDefaultTimeout(10000);ctx.setDefaultNavigationTimeout(15000);contexts.push(ctx);return ctx;
 }
 const editor = p => p.locator('#scope-editor');
 const checkbox=(p,code)=>editor(p).locator(`input[type=checkbox][value="${code}"]`);
 async function ready(p,modal=true) {
  await p.evaluate(()=>document.fonts.ready);
  if(modal)await p.waitForFunction(()=>document.querySelector('#scope-editor')?.matches(':modal'));
 }
 async function open(p,id=cfg.OwnerID,project='demo',modal=true) {
  await p.goto(`${cfg.Base}/projects/settings?project=${project}&edit=${id}`);await ready(p,modal);
 }
 async function draft(p) {
  await checkbox(p,'session:rename').uncheck();await checkbox(p,'message:receive').check();await checkbox(p,'memory:write').check();
 }
 async function review(p) {
  await editor(p).locator('[data-scope-next]').click();
  await p.waitForFunction(()=>document.querySelector('#scope-editor')?.dataset.step==='review');
 }
 async function back(p) {
  await editor(p).locator('button[value="back"]').click();
  await p.waitForFunction(()=>document.querySelector('#scope-editor')?.dataset.step==='select');
 }
 async function formData(p) {
  return await editor(p).locator('form').evaluate(form=>Array.from(new FormData(form).entries()));
 }
 async function post(p,fields,intent='apply') {
  const body=new URLSearchParams(fields);body.set('intent',intent);
  return await p.context().request.post(cfg.Base+'/tokens/scopes',{data:body.toString(),headers:{'Content-Type':'application/x-www-form-urlencoded'},maxRedirects:0});
 }
 async function setScopes(p,id,scopes,project='demo') {
  await open(p,id,project);
  const fields=(await formData(p)).filter(([key])=>key!=='scope');
  for(const code of scopes)fields.push(['scope',code]);
  const response=await post(p,fields);
  assert.equal(response.status(),303,'fixture reset through guarded apply');
 }
 async function settle(p) { await p.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)))); }
 async function assertErrorVisible(p) {
  await settle(p);
  assert.equal(await editor(p).evaluate(e=>{const notice=e.querySelector('.notice.error[role=alert]');const r=e.getBoundingClientRect(),n=notice.getBoundingClientRect();return document.activeElement===notice&&n.top>=r.top&&n.bottom<=r.bottom;}),true,'entire focused error notice must remain inside drawer after scheduled frames');
 }
 async function capture(p,name,width,theme,options={}) {
  await settle(p);
  await p.evaluate(async options=>{await document.fonts.ready;const e=document.querySelector('#scope-editor');if(e&&options.bottom)e.scrollTop=e.scrollHeight;if(e&&options.top)e.scrollTop=0;},options);
  await settle(p);
  if(options.bottom)assert.equal(await editor(p).evaluate(e=>{const action=[...e.querySelectorAll('.form-actions')].find(n=>n.getClientRects().length);const a=action.getBoundingClientRect(),r=e.getBoundingClientRect();return a.top>=r.top&&a.bottom<=r.bottom;}),true,'final actions reachable inside drawer');
  const file=`${name}-${width}-${theme}.png`;
  const geometry=await p.evaluate(()=>{
   function lines(e) {
    const groups=new Map();const walker=document.createTreeWalker(e,NodeFilter.SHOW_TEXT);
    while(walker.nextNode()){const text=walker.currentNode;if(!text.parentElement.getClientRects().length)continue;
     for(let i=0;i<text.length;i++){const r=document.createRange();r.setStart(text,i);r.setEnd(text,i+1);const box=r.getBoundingClientRect();if(!box.width)continue;const y=Math.round(box.y*2)/2;groups.set(y,(groups.get(y)||'')+text.data[i]);}}
    return [...groups].sort((a,b)=>a[0]-b[0]).map(([y,text])=>({y,text:text.trim()}));
   }
   const selectors=['.scope-editor','.scope-editor .section-heading','.scope-editor-label','.scope-editor-info','.scope-editor-fixed','.scope-editor-warning','.scope-editor-group h3','.scope-editor-grid','.scope-editor-grid .check','.scope-editor-summary','.scope-editor-summary p','.scope-editor .form-actions:not([hidden])','.scope-editor-change','.scope-editor .notice','#project-token-title'];
   const nodes=selectors.flatMap(selector=>[...document.querySelectorAll(selector)].filter(e=>e.getClientRects().length).map(e=>{
    const r=e.getBoundingClientRect(),s=getComputedStyle(e);
    return {selector,text:e.matches('p,h3,label')?e.innerText:undefined,box:{x:r.x,y:r.y,width:r.width,height:r.height},font:s.font,fontSize:s.fontSize,fontWeight:s.fontWeight,lineHeight:s.lineHeight,padding:s.padding,color:s.color,background:s.backgroundColor,border:s.border,lines:lines(e)};
   }));
   return {nodes,fonts:document.fonts.status,theme:document.documentElement.dataset.theme,scrollY:scrollY,drawerScroll:document.querySelector('#scope-editor')?.scrollTop};
  });
  assert.equal(geometry.fonts,'loaded');assert.equal(geometry.theme,theme);
  await p.screenshot({path:path.join(out,file),fullPage:name==='list'});
  manifest.captures.push({file,state:name,width,height:900,theme,...geometry});
 }
 try {
  for(const width of [1280,390])for(const theme of ['light','dark']) {
   stage=`matrix-${width}-${theme}`;
   const ctx=await context(width,theme),p=await ctx.newPage();
   await setScopes(p,cfg.OwnerID,baseline);
   await p.goto(cfg.Base+'/projects/settings?project=demo');await ready(p,false);
   if(await p.locator('.toast').count())await p.locator('.toast button').first().click({timeout:1000}).catch(()=>{});
   await capture(p,'list',width,theme);
   const link=p.locator(`[data-scope-edit="${cfg.OwnerID}"]`);
   assert.match(await link.getAttribute('aria-label'),/dev-agent/);await link.click();await ready(p);
   assert.equal(Math.round((await editor(p).boundingBox()).width),Math.min(width,620));
   assert.equal(await p.locator(':focus').getAttribute('id'),'scope-editor-title');
   assert.equal(await editor(p).locator('[data-scope-next]').isDisabled(),true);
   assert.equal(await editor(p).locator('input[name=auto_cap],input[name=auto_days],input[name=owner]').count(),0);
   assert.match(await editor(p).innerText(),/현재 승인된 세션 자격 4개 · 연결된 세션 약 3개/);
   await draft(p);await capture(p,'editor-owner',width,theme,{top:true});
   if(width===1280&&theme==='light'){
     const row=await p.locator(`[data-token-row="${cfg.OwnerID}"]`).boundingBox();assert.ok(row.y>=64&&row.y+row.height<=900);
     await capture(p,'selected-row',width,theme);
   }
   await capture(p,'editor-owner-bottom',width,theme,{bottom:true});
   await review(p);await capture(p,'confirm',width,theme);await capture(p,'confirm-bottom',width,theme,{bottom:true});
   assert.equal(await editor(p).locator('[data-scope-apply]').getAttribute('class'),'danger');
   assert.deepEqual(await editor(p).locator('.is-added code').allTextContents(),['memory:write','message:receive']);
   assert.deepEqual(await editor(p).locator('.is-removed code').allTextContents(),['session:rename']);
   await back(p);assert.equal(await checkbox(p,'memory:write').isChecked(),true);
   await p.keyboard.press('Escape');await p.waitForFunction(id=>document.activeElement?.getAttribute('data-scope-edit')===id,cfg.OwnerID);
   const devctx=await context(width,theme,'Developer'),dev=await devctx.newPage();await open(dev,cfg.DeveloperID);
   assert.equal(await checkbox(dev,'session:manage').isDisabled(),true);
   await checkbox(dev,'session:rename').uncheck();await checkbox(dev,'session:rename').check();assert.equal(await checkbox(dev,'session:rename').isDisabled(),false);
   await checkbox(dev,'session:rename').uncheck();await capture(dev,'editor-developer',width,theme,{top:true});await capture(dev,'editor-developer-bottom',width,theme,{bottom:true});
   await open(p);await draft(p);const stale=await formData(p);
   const changed=stale.filter(([key])=>key!=='scope');for(const code of baseline.filter(x=>x!=='session:rename'))changed.push(['scope',code]);
   assert.equal((await post(p,changed)).status(),303);
   await editor(p).locator('[data-scope-next]').click();await editor(p).locator('[role=alert]').waitFor();
   assert.equal(await editor(p).getAttribute('data-step'),'select');assert.equal(await checkbox(p,'message:receive').isChecked(),false);assert.equal(await checkbox(p,'session:rename').isChecked(),false);
   assert.equal(await p.locator(':focus').getAttribute('role'),'alert');
   assert.match(await editor(p).locator('[data-scope-noop]').innerText(),/변경 없음 · 최신 권한을 다시 선택해 주세요/);
   assert.equal(await editor(p).locator('[data-scope-impact]').isVisible(),false);
   await assertErrorVisible(p);await capture(p,'conflict',width,theme);await capture(p,'conflict-bottom',width,theme,{bottom:true});
  }
  manifest.checks.push('20 fixed-theme captures: List, Editor owner, Editor developer, Confirm, Conflict ×1280/390 ×Light/Dark');
  stage='interaction';const ctx=await context(1280,'light'),p=await ctx.newPage();await setScopes(p,cfg.OwnerID,baseline);await open(p);
  for(let i=0;i<20;i++){await p.keyboard.press('Tab');assert.equal(await p.locator(':focus').evaluate(e=>!!e.closest('#scope-editor')),true);}
  await p.mouse.click(20,400);assert.equal(await editor(p).isVisible(),true);
  await checkbox(p,'message:receive').check();await review(p);assert.equal(await editor(p).locator('[data-scope-apply]').getAttribute('class'),'primary');
  await back(p);assert.equal(await checkbox(p,'message:receive').isChecked(),true);await review(p);
  let applyRequests=0;p.on('request',r=>{if(r.url().endsWith('/tokens/scopes')&&r.postData()?.includes('intent=apply'))applyRequests++;});
  await editor(p).locator('[data-scope-apply]').evaluate(button=>{button.click();button.click();});await p.waitForURL(/\/projects\/settings\?project=demo$/);
  assert.equal(applyRequests,1);assert.match(await p.locator('.toast').innerText(),/토큰 권한을 변경했습니다/);
  await open(p);await checkbox(p,'message:receive').uncheck();await capture(p,'receive-removal-select',1280,'light',{bottom:true});assert.equal(await editor(p).locator('[data-scope-receive-removed]').isVisible(),true);await review(p);assert.equal(await editor(p).locator('[data-scope-receive-removed]').isVisible(),true);await capture(p,'receive-removal-review',1280,'light',{bottom:true});
  await back(p);await editor(p).locator('[data-scope-close]').first().click();await p.waitForFunction(()=>!document.querySelector('#scope-editor').open);
  await open(p,cfg.AutoID);await capture(p,'auto-metadata',1280,'light');assert.match(await editor(p).innerText(),/자동 등록 한도 10개 · 수명 7일/);assert.equal(await editor(p).locator('input[value="session:register:auto"]').count(),0);
  await open(p,cfg.AllID,'scope-edges');for(const code of general)await checkbox(p,code).check();await review(p);assert.equal(await editor(p).locator('.scope-editor-change code').count(),general.length);
  for(const width of [320,390,640]){await p.setViewportSize({width,height:900});await open(p,cfg.LongID,'scope-edges');assert.equal(await editor(p).evaluate(e=>e.scrollWidth<=e.clientWidth),true);}
  // 200% browser zoom has the same CSS viewport as half the desktop width.
  await p.setViewportSize({width:640,height:450});await open(p,cfg.LongID,'scope-edges');for(const code of general)await checkbox(p,code).check();await review(p);assert.equal(await editor(p).evaluate(e=>e.scrollWidth<=e.clientWidth),true);
  manifest.checks.push('keyboard trap/heading, outside click, Esc and focus return, Back draft, add-only/mixed review, double Apply, receive help, immutable auto metadata, eight changes, long labels at320/390 and200% equivalent viewport');
  stage='off-state';
  const adminctx=await context(1280,'light','Admin'),admin=await adminctx.newPage();
  async function issuance(enabled) {
    await admin.goto(cfg.Base+'/admin/settings');
    const fields=await admin.locator('form[action="/admin/scope-issuance"]').evaluate(form=>Array.from(new FormData(form).entries()));
    const body=new URLSearchParams(fields);body.set('enabled',String(enabled));
    const response=await adminctx.request.post(cfg.Base+'/admin/scope-issuance',{data:body.toString(),headers:{'Content-Type':'application/x-www-form-urlencoded'},maxRedirects:0});
    assert.equal(response.status(),303);
  }
  await issuance(false);await open(p);assert.match(await editor(p).innerText(),/현재 권한을 줄이는 것만/);
  assert.equal(await checkbox(p,'session:manage').isDisabled(),true);assert.equal(await checkbox(p,'session:read').isDisabled(),false);
  await checkbox(p,'session:read').uncheck();await checkbox(p,'session:read').check();
  await issuance(true);
  manifest.checks.push('issuance-off Owner editor: held scope can uncheck/recheck, absent scope disabled');
  stage='no-js';const plain=await context(1280,'light','Owner',false),q=await plain.newPage();await open(q,cfg.OwnerID,'demo',false);
  const noop=await formData(q);assert.equal((await post(q,noop,'review')).status(),200);
  await checkbox(q,'session:rename').uncheck();await editor(q).locator('[data-scope-next]').click();assert.equal(await editor(q).getAttribute('data-step'),'review');
  await editor(q).locator('button[value=back]').click();assert.equal(await checkbox(q,'session:rename').isChecked(),false);
  await editor(q).locator('[data-scope-next]').click();await editor(q).locator('[data-scope-apply]').click();assert.match(q.url(),/\/projects\/settings\?project=demo$/);
  await open(q,cfg.OwnerID,'demo',false);
  const staleNoJS=await formData(q);await setScopes(p,cfg.OwnerID,baseline);
  const conflictResponse=q.waitForResponse(r=>r.url().endsWith('/tokens/scopes'));
  await editor(q).locator('[data-scope-next]').click();assert.equal((await conflictResponse).status(),409);
  assert.equal(await editor(q).getAttribute('data-step'),'select');assert.equal(await q.locator(':focus').getAttribute('role'),'alert');
  await editor(q).locator('[data-scope-close]').first().click();assert.equal(await editor(q).count(),0);
  const viewerctx=await context(1280,'light','Viewer'),v=await viewerctx.newPage();const forbidden=await v.goto(cfg.Base+'/projects/settings?project=demo&edit='+cfg.OwnerID);assert.equal(forbidden.status(),403);assert.doesNotMatch(await v.locator('body').innerText(),/dev-agent|session:rename/);
  manifest.checks.push('no-JS GET/Review/Back/Apply/no-op/Close; Viewer denial without target data');
  for(const theme of ['light','dark']) {
   stage='empty-intersection-'+theme;const errorctx=await context(390,theme),errorPage=await errorctx.newPage();
   await open(errorPage,cfg.HistoryID,'scope-edges');await checkbox(errorPage,'message:receive').uncheck();
   await editor(errorPage).evaluate(e=>{e.scrollTop=e.scrollHeight;});
   await editor(errorPage).locator('[data-scope-next]').click();await editor(errorPage).locator('[role=alert]').waitFor();
   assert.match(await editor(errorPage).locator('[role=alert]').innerText(),/이전 자격 기록과 함께 저장할 수 없는 변경/);
   await assertErrorVisible(errorPage);await capture(errorPage,'empty-intersection',390,theme);
  }
  assert.equal(manifest.captures.filter(x=>['list','editor-owner','editor-developer','confirm','conflict'].includes(x.state)).length,20);
  fs.writeFileSync(path.join(out,'manifest.json'),JSON.stringify(manifest,null,2));
  fs.rmSync(path.join(out,'failure.json'),{force:true});
  console.log('scope editor full Chrome PASS; '+manifest.captures.length+' captures; evidence '+out);
 } catch(error) {
  console.error('scope editor stage failed: '+stage);
  fs.writeFileSync(path.join(out,'failure.json'),JSON.stringify({stage,message:String(error.message).replace(/asb_[psu]_[\w-]+/g,'[redacted]')},null,2));
  process.exitCode=1;
 } finally {for(const ctx of contexts)await ctx.close();await browser.close();}
})();
