const {test,before,after}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const http=require('node:http');
const {chromium}=require('playwright');
let browser,server,origin;
const current=()=>new Date().toISOString();
function fixtureHTML(){
 const now=current(),expires=new Date(Date.now()+15000).toISOString();
 return `<!doctype html><html><head><script src="/panel.js" defer></script>${fs.existsSync(path.join(__dirname,'session-live.js'))?'<script src="/session-live.js" defer></script>':''}<script type="module" src="/tooltip.js"></script></head><body><button id="outside">outside</button><input type="search" data-table-filter="session-table"><p data-session-live-status role="status"></p><table id="session-table" data-session-live="/projects/sessions/live?project=demo" data-live-server-time="${now}"><tbody><tr data-session-row="registration:fixture" data-row-shape="same"><td><span data-session-dot class="state-dot state-dot--ready"></span><strong>fixture-session</strong><small data-runtime-info aria-label="모델 / effort">모델·effort 알 수 없음</small></td><td class="cell-status"><span data-tooltip><button type="button" class="asb-tooltip__trigger" aria-describedby="tip"><span data-connection-state class="badge state-badge state-badge--ready"><span class="badge__dot"></span>수신 준비</span><span role="img" aria-label="활동: working" data-activity-state="working" data-activity-expires="${expires}"></span></button><span id="tip" data-session-tooltip role="tooltip" popover="manual">alive</span></span></td><td><form method="post" action="/sessions/detach" data-confirm-action="session-detach" data-confirm-target="fixture-session"><input name="csrf" value="fixture-csrf"><button id="detach">해제</button></form></td></tr></tbody></table><dialog id="confirm-action"><h2 id="confirm-action-title"></h2><p id="confirm-action-description"></p><button type="button" data-dialog-close autofocus>취소</button><button type="button" data-confirm-submit>확인</button></dialog></body></html>`;
}
before(async()=>{
 server=http.createServer((req,res)=>{
  res.setHeader('Content-Security-Policy',"default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'");
  if(req.url==='/'){res.setHeader('Content-Type','text/html; charset=utf-8');res.end(fixtureHTML());return;}
  const name=req.url.slice(1);
  if(!['panel.js','session-live.js','tooltip.js'].includes(name)){res.writeHead(404).end();return;}
  res.setHeader('Content-Type','text/javascript');res.end(fs.readFileSync(path.join(__dirname,name)));
 });
 await new Promise(r=>server.listen(0,'127.0.0.1',r));origin=`http://127.0.0.1:${server.address().port}`;
 browser=await chromium.launch({executablePath:process.env.PW_CHROMIUM_EXECUTABLE});
});
after(async()=>{await browser?.close();await new Promise(r=>server.close(r));});
function payload(state='idle',extra={}){
 return {project:'demo',serverTime:current(),summary:{Registered:1,Ready:1,Unreachable:0,Suspended:0,Reserved:0},rows:[{key:'registration:fixture',shape:'same',displayState:'ready',connectionLabel:'수신 준비',status:'alive',reason:'',probePending:false,canManage:true,activity:{state,label:state==='working'?'턴 진행':state==='idle'?'대기':'활동 확인 불가',source:'Claude 세션 상태',reason:'',observedAt:current(),receivedAt:current(),expiresAt:new Date(Date.now()+15000).toISOString()},...extra}]};
}
async function fixture(t,handler,options={}){
 const context=await browser.newContext({timezoneId:options.timezoneId||'Asia/Seoul'});t.after(()=>context.close());const page=await context.newPage();const errors=[];
 page.on('pageerror',e=>errors.push(e.message));t.after(()=>assert.deepEqual(errors,[]));
 await page.clock.install(options.time?{time:new Date(options.time)}:{});
 if(options.initialTime)await page.route(origin+'/',route=>route.fulfill({contentType:'text/html; charset=utf-8',body:fixtureHTML().replace(/data-live-server-time="[^"]*"/,`data-live-server-time="${options.time}"`).replace('>alive</span>','>alive<time data-activity-time="receivedAt" datetime="'+options.initialTime+'">UTC fallback</time></span>')}));
 await page.route('**/projects/sessions/live?*',handler);await page.goto(origin);return page;
}
async function advance(page,milliseconds=5000){await page.clock.runFor(milliseconds);await page.waitForTimeout(25);}

test('activity automatically changes working to idle to unknown',async t=>{
 let calls=0;
 const page=await fixture(t,route=>route.fulfill({json:payload(++calls===1?'idle':'unknown')}));
 assert.equal(await page.locator('[data-activity-state]').getAttribute('data-activity-state'),'working');
 await advance(page);
 await page.waitForFunction(()=>document.querySelector('[data-activity-state]').dataset.activityState==='idle',null,{timeout:1000});
 await advance(page);
 await page.waitForFunction(()=>document.querySelector('[data-activity-state]').dataset.activityState==='unknown',null,{timeout:1000});
 assert.ok(calls>=2);
});

test('deferred activity never gets a fresh TTL when a dialog closes',async t=>{
 const page=await fixture(t,route=>route.fulfill({json:payload('idle')}));
 await page.locator('#detach').click();
 await advance(page,5000);
 assert.equal(await page.locator('[data-activity-state]').getAttribute('data-activity-state'),'working');
 // Pause further fetches while keeping the first accepted response pending.
 await page.route('**/projects/sessions/live?*',route=>route.fulfill({status:503,json:{error:'unavailable'}}));
 await advance(page,16000);
 await page.locator('[data-dialog-close]').click();await advance(page,1000);
 assert.equal(await page.locator('[data-activity-state]').getAttribute('data-activity-state'),'unknown');
});

test('polling preserves search, row/form identity, CSRF and focused tooltip',async t=>{
 const page=await fixture(t,route=>route.fulfill({json:payload()}));
 await page.evaluate(()=>{window.savedRow=document.querySelector('tr');window.savedForm=document.querySelector('form');window.savedTip=document.querySelector('#tip');});
 const input=page.locator('[data-table-filter]');await input.fill('대기');await advance(page);
 assert.equal(await input.inputValue(),'대기');assert.equal(await input.evaluate(e=>e===document.activeElement),true);
 assert.equal(await page.evaluate(()=>savedRow===document.querySelector('tr')&&savedForm===document.querySelector('form')&&savedTip===document.querySelector('#tip')),true);
 assert.equal(await page.locator('input[name=csrf]').inputValue(),'fixture-csrf');
 await input.fill('');await page.locator('.asb-tooltip__trigger').focus();
 await advance(page);assert.equal(await page.locator('#tip').evaluate(e=>e.matches(':popover-open')),true);
});

test('hidden tabs stop requests and resume immediately',async t=>{
 let calls=0;const page=await fixture(t,route=>{calls++;return route.fulfill({json:payload()});});
 await advance(page);assert.equal(calls,1);
 await page.evaluate(()=>{Object.defineProperty(document,'hidden',{configurable:true,get:()=>true});document.dispatchEvent(new Event('visibilitychange'));});
 await advance(page,30000);assert.equal(calls,1);
 await page.evaluate(()=>{Object.defineProperty(document,'hidden',{configurable:true,get:()=>false});document.dispatchEvent(new Event('visibilitychange'));});
 await advance(page,1);assert.equal(calls,2);
});

for(const status of [401,403,404])test(`permission status ${status} stops polling and disables management`,async t=>{
 let calls=0;const page=await fixture(t,route=>{calls++;return route.fulfill({status,json:{error:'forbidden'}});});
 await advance(page);await advance(page,30000);assert.equal(calls,1);
 assert.equal(await page.locator('#detach').isDisabled(),true);
 assert.match(await page.locator('[data-session-live-status]').textContent(),/권한/);
});

test('failures expire activity and back off without replacing rows',async t=>{
 let calls=0;const page=await fixture(t,route=>{calls++;return route.fulfill({status:503,json:{error:'unavailable'}});});
 await advance(page);assert.equal(calls,1);await advance(page);assert.equal(calls,2);
 await advance(page,10000);assert.equal(calls,3);
 assert.equal(await page.locator('[data-activity-state]').getAttribute('data-activity-state'),'unknown');
 assert.equal(await page.locator('strong').textContent(),'fixture-session');
 await advance(page,10000);assert.equal(calls,3);
});

test('changed rows retain forms and require explicit reload',async t=>{
 const page=await fixture(t,route=>route.fulfill({json:payload('idle',{shape:'changed',canManage:false})}));
 await advance(page);assert.equal(await page.locator('#detach').isDisabled(),true);
 assert.match(await page.locator('[data-session-live-status]').textContent(),/새로고침 필요/);
 assert.equal(await page.locator('input[name=csrf]').inputValue(),'fixture-csrf');
});

test('an open tooltip stays intact until closed, including expiry',async t=>{
 const page=await fixture(t,route=>route.fulfill({status:503,json:{error:'unavailable'}}));
 await page.locator('.asb-tooltip__trigger').focus();
 const original=await page.locator('#tip').textContent();await advance(page,17000);
 assert.equal(await page.locator('[data-activity-state]').getAttribute('data-activity-state'),'unknown');
 assert.equal(await page.locator('#tip').textContent(),original);
 await page.locator('#outside').focus();await advance(page,1000);
 assert.match(await page.locator('#tip').textContent(),/활동 관측 만료/);
});

test('POST busy state defers updates and pagehide cancels a pending request',async t=>{
 let calls=0;let held;
 const page=await fixture(t,route=>{calls++;if(calls===1)return route.fulfill({json:payload()});held=route;});
 await page.locator('form').evaluate(e=>e.setAttribute('aria-busy','true'));await advance(page);
 assert.equal(await page.locator('[data-activity-state]').getAttribute('data-activity-state'),'working');
 await page.locator('form').evaluate(e=>e.removeAttribute('aria-busy'));await advance(page,1000);
 assert.equal(await page.locator('[data-activity-state]').getAttribute('data-activity-state'),'idle');
 await advance(page,4000);assert.equal(calls,2);await advance(page,5000);assert.equal(calls,2);
 await page.evaluate(()=>window.dispatchEvent(new PageTransitionEvent('pagehide')));await advance(page,30000);assert.equal(calls,2);
 if(held)await held.fulfill({json:payload('working')}).catch(()=>{});
 assert.equal(await page.locator('[data-activity-state]').getAttribute('data-activity-state'),'idle');
 await page.evaluate(()=>window.dispatchEvent(new PageTransitionEvent('pageshow',{persisted:true})));await advance(page,1);
 assert.equal(calls,3);
});

test('HTML and malformed or foreign JSON never overwrite the status',async t=>{
 let calls=0;const page=await fixture(t,route=>{
  calls++;if(calls===1)return route.fulfill({contentType:'text/html',body:'<h1>Login</h1>'});
  if(calls===2)return route.fulfill({json:{...payload(),project:'other'}});
  return route.fulfill({json:{rows:'bad'}});
 });
 await advance(page);await advance(page);await advance(page,10000);
 assert.equal(calls,3);assert.equal(await page.locator('h1').count(),0);
 assert.equal(await page.locator('[data-activity-state]').getAttribute('data-activity-state'),'unknown');
 assert.match(await page.locator('[data-session-live-status]').textContent(),/갱신 실패/);
});

test('a successful retry restores status after a temporary failure',async t=>{
 let calls=0;const page=await fixture(t,route=>++calls===1?route.fulfill({status:503,json:{error:'unavailable'}}):route.fulfill({json:payload('idle')}));
 await advance(page);assert.match(await page.locator('[data-session-live-status]').textContent(),/갱신 실패/);
 await advance(page);assert.equal(await page.locator('[data-activity-state]').getAttribute('data-activity-state'),'idle');assert.match(await page.locator('[data-session-live-status]').textContent(),/마지막 성공/);
});

test('new or missing registrations only request a manual reload',async t=>{
 let calls=0;const page=await fixture(t,route=>{const data=payload();data.rows=++calls===1?[...data.rows,{...data.rows[0],key:'registration:new'}]:[];return route.fulfill({json:data});});
 await advance(page);assert.equal(await page.locator('[data-session-row]').count(),1);assert.match(await page.locator('[data-session-live-status]').textContent(),/새로고침 필요/);
 await advance(page);assert.equal(await page.locator('[data-session-row]').count(),1);assert.equal(await page.locator('#detach').isDisabled(),true);
});

test('activity uses a text-free accessible indicator and wording only in the tooltip',async t=>{
 const page=await fixture(t,route=>route.fulfill({json:payload('idle')}));
 await advance(page);
 const indicator=page.locator('[data-activity-state]');
 assert.equal(await indicator.textContent(),'');
 assert.equal(await indicator.getAttribute('role'),'img');
 assert.equal(await indicator.getAttribute('aria-label'),'활동: idle');
 assert.match(await page.locator('#tip').textContent(),/활동: 대기/);
 await advance(page,16000);
 assert.equal(await indicator.textContent(),'');
});

test('tooltip times are local and fraction-free with original datetime and freshness',async t=>{
 const observed='2026-10-10T07:19:28.316438Z',now='2026-10-10T07:19:31.316438Z',expires='2026-10-10T07:19:43.316438Z';
 const data=payload('working');data.serverTime=now;data.rows[0].activity={...data.rows[0].activity,observedAt:observed,receivedAt:observed,expiresAt:expires};
 const page=await fixture(t,route=>route.fulfill({json:data}),{time:now});await advance(page);
 await page.locator('.asb-tooltip__trigger').focus();const text=await page.locator('#tip').innerText();
 assert.doesNotMatch(text,/\d{4}-\d\d-\d\dT|\.\d{3,}Z/);
 assert.equal(await page.locator(`#tip time[datetime="${observed}"]`).count(),2);
 assert.match(text,/16:19:28/);assert.doesNotMatch(text,/2026-10-10/);
 assert.match(text,/3초 전/);assert.match(text,/만료까지 12초/);
});

for (const [timezoneId,expected] of [['Asia/Seoul','16:19:28'],['UTC','07:19:28'],['America/Los_Angeles','00:19:28']]) test(`initial and polled times share the local formatter in ${timezoneId}`,async t=>{
 const now='2026-10-10T07:19:31.316438Z', observed='2026-10-10T07:19:28.316438Z';
 const data=payload();data.serverTime=now;data.rows[0].activity={state:'unknown',receivedAt:observed,changedAt:'2026-10-09T06:19:28.316438Z',observedAt:'invalid',expiresAt:'2026-10-10T07:19:30Z'};
 const page=await fixture(t,route=>route.fulfill({json:data}),{time:now,timezoneId,initialTime:observed});
 assert.equal(await page.locator('#tip time').textContent(),`${expected} · 3초 전`);
 await advance(page);
 assert.equal(await page.locator('#tip time[data-activity-time=receivedAt]').textContent(),`${expected} · 3초 전`);
 assert.match(await page.locator('#tip time[data-activity-time=changedAt]').textContent(),/2026-10-0[89] .* · 1일 전/);
 assert.equal(await page.locator('#tip time[data-activity-time=observedAt]').count(),0);
 assert.match(await page.locator('#tip time[data-activity-time=expiresAt]').textContent(),/만료됨/);
});

test('open tooltip updates relative time without replacing time or popup nodes',async t=>{
 const now='2026-10-10T07:19:31Z',observed='2026-10-10T07:19:28Z';
 const page=await fixture(t,route=>route.fulfill({status:503}),{time:now,initialTime:observed});
 await page.locator('.asb-tooltip__trigger').focus();
 await page.evaluate(()=>{window.savedTime=document.querySelector('#tip time');window.savedPopup=document.querySelector('#tip');});
 await advance(page,3000);
 assert.match(await page.locator('#tip time').textContent(),/6초 전/);
 assert.equal(await page.evaluate(()=>savedTime===document.querySelector('#tip time')&&savedPopup===document.querySelector('#tip')&&savedPopup.matches(':popover-open')),true);
});


test('model and effort update without replacing a session row and expire locally',async t=>{
 const info={model:'gpt-6.1-sol',effort:'medium',expiresAt:new Date(Date.now()+15000).toISOString()};
 const page=await fixture(t,route=>route.fulfill({json:payload('working',{runtimeInfo:info})}));
 await page.evaluate(()=>window.originalRow=document.querySelector('tr'));
 await advance(page);
 await page.waitForFunction(()=>document.querySelector('[data-runtime-info]').textContent==='gpt-6.1-sol / medium');
 assert.equal(await page.locator('[data-runtime-info]').getAttribute('aria-label'),'모델 / effort');
 assert.equal(await page.evaluate(()=>originalRow===document.querySelector('tr')),true);
 await page.route('**/projects/sessions/live?*',route=>route.fulfill({status:503,json:{error:'unavailable'}}));
 await advance(page,16000);
 assert.equal(await page.locator('[data-runtime-info]').textContent(),'모델·effort 알 수 없음');
});

test('unsafe model metadata is rendered as unknown with no HTML injection',async t=>{
 const page=await fixture(t,route=>route.fulfill({json:payload('idle',{runtimeInfo:{model:'<img src=x>',effort:'high',expiresAt:new Date(Date.now()+15000).toISOString()}})}));
 await advance(page);
 assert.equal(await page.locator('[data-runtime-info]').textContent(),'모델·effort 알 수 없음');
 assert.equal(await page.locator('[data-runtime-info] img').count(),0);
});
