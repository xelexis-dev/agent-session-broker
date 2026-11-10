const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const {chromium}=require('playwright');
const rows=JSON.parse(fs.readFileSync(process.argv[2],'utf8'));
const out=process.env.ASB_ATTACHMENT_UI_EVIDENCE;
const draftBase=process.env.ASB_ATTACHMENT_DRAFT_URL;
if(!out)throw Error('evidence directory required');
fs.mkdirSync(out,{recursive:true});
(async()=>{
 const browser=await chromium.launch({executablePath:'/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',headless:true});
 const results=[];let stage='setup';
 try{
  for(const row of rows)for(const theme of ['light','dark'])for(const width of [1280,390]){
   const stem=`${row.State}-${theme}-${width}`;
   const context=await browser.newContext({viewport:{width,height:1000},colorScheme:theme,timezoneId:'Asia/Seoul',reducedMotion:'reduce'});
   await context.addInitScript(theme=>localStorage.setItem('asb-theme',theme),theme);
   await context.addCookies([{name:row.CookieName,value:row.Cookie,url:row.Base}]);
   const actual=await context.newPage(),draft=draftBase?await context.newPage():null;
   stage=stem+':load';
   await actual.goto(row.Base+'/admin/settings');
   const extended=row.State.startsWith('off-');
   if(draft)await draft.goto(`${draftBase}?state=${extended?row.State.slice(4):row.State}&theme=${theme}`);
   if(extended&&draft)await draft.evaluate(()=>{
    const badge=document.getElementById('attachment-badge');badge.textContent='OFF';badge.classList.remove('enabled');
    const toggle=document.getElementById('attachment-toggle');toggle.textContent='켜기';toggle.setAttribute('aria-label','메시지 파일 첨부 켜기');
    document.getElementById('attachment-form').elements.namedItem('attachments').value='true';
    document.getElementById('attachment-history').textContent='설정 변경 이력이 없습니다.';
    document.getElementById('attachment-status-detail').textContent='첨부가 꺼져 있어 서버의 첨부 파일은 지워졌습니다. 메시지 전달은 계속됩니다.';
   });
   await actual.evaluate(()=>document.fonts.ready);if(draft)await draft.evaluate(()=>document.fonts.ready);
   const metrics=async page=>page.evaluate(()=>{
    const card=document.querySelector('#attachment-settings'),r=card.getBoundingClientRect();
    return {overflow:document.documentElement.scrollWidth>innerWidth,cardOverflow:card.scrollWidth>card.clientWidth,x:r.x,y:r.y,width:r.width,height:r.height,text:card.innerText.replace(/\s+/g,' ').trim()};
   });
   const a=await metrics(actual),d=draft?await metrics(draft):null;
   const actualPNG=await actual.screenshot({path:path.join(out,stem+'-actual.png'),fullPage:true});
   if(draft){const draftPNG=await draft.screenshot({path:path.join(out,stem+'-draft.png'),fullPage:true});
   const compare=await context.newPage();
   await compare.setContent('<canvas></canvas>');
   await compare.evaluate(async data=>{
    const images=await Promise.all(data.map(src=>new Promise((resolve,reject)=>{const image=new Image();image.onload=()=>resolve(image);image.onerror=reject;image.src=src;})));
    const canvas=document.querySelector('canvas');canvas.width=images[0].width+images[1].width;canvas.height=Math.max(images[0].height,images[1].height);
    const c=canvas.getContext('2d');c.drawImage(images[0],0,0);c.drawImage(images[1],images[0].width,0);
    document.body.style.margin='0';
   },[draftPNG,actualPNG].map(p=>'data:image/png;base64,'+p.toString('base64')));
   await compare.locator('canvas').screenshot({path:path.join(out,stem+'-side-by-side.png')});
   }
   stage=stem+':compare';
   results.push({state:row.State,theme,width,actual:a,draft:d});
   fs.writeFileSync(path.join(out,'results.json'),JSON.stringify(results,null,2));
   assert.equal(a.overflow,false,'product document overflow');assert.equal(a.cardOverflow,false,'product card overflow');
   if(draft){assert.equal(d.overflow,false,'draft document overflow');
   assert.equal(a.text,d.text,'visible attachment copy differs');
   for(const key of ['x','y','width','height'])assert.ok(Math.abs(a[key]-d[key])<=1,`card ${key} differs: ${a[key]} vs ${d[key]}`);
   }
   const status=actual.locator('#attachment-status-title');
   const statusTitles={off:'파일 첨부가 꺼져 있습니다.',on:'새 첨부를 받을 수 있습니다.',long:'새 첨부를 받을 수 있습니다.',low:'남은 공간이 1 GiB 미만이라 새 첨부를 받지 않습니다.',unknown:'서버 디스크: 남은 공간을 확인할 수 없어 새 첨부를 받지 않습니다.'};
   assert.equal(await status.isVisible(),true,'attachment status title is visible');
   assert.equal(await status.innerText(),statusTitles[row.State.replace(/^off-/, '')],'attachment status copy');
   const expectedOn=row.State!=='off'&&!extended;
   assert.equal(await actual.locator('#attachment-badge').innerText(),expectedOn?'ON':'OFF');
   assert.equal(await actual.locator('#attachment-form [name=attachments]').inputValue(),String(!expectedOn));
   if(extended){assert.ok(a.text.includes('첨부가 꺼져 있어 서버의 첨부 파일은 지워졌습니다. 메시지 전달은 계속됩니다.'));assert.ok(!a.text.includes('이미 보관한 첨부의 다운로드와 메시지 전달은 계속됩니다.'));}
   stage=stem+':dialog';
   const enabled=row.State!=='off'&&!row.State.startsWith('off-');
   await actual.locator('#attachment-toggle').click();
   assert.equal(await actual.locator('#confirm-action-title').innerText(),`메시지 파일 첨부 ${enabled?'끄기':'켜기'}`);
   assert.equal(await actual.locator('#confirm-action-description').innerText(),enabled?'새 첨부를 막고 서버에 남은 첨부 파일을 모두 지웁니다. 지운 파일은 되돌릴 수 없습니다. 메시지 파일 첨부를 끄시겠습니까?':'세션이 보낸 파일이 이 서버를 거쳐 받는 PC에 저장됩니다. 메시지 파일 첨부를 켜시겠습니까?');
   assert.equal(await actual.locator('[data-confirm-submit]').innerText(),enabled?'끄고 서버 파일 삭제':'첨부 켜기');
   assert.equal(await actual.locator('#confirm-action [autofocus]').evaluate(el=>el===document.activeElement),true);
   await actual.keyboard.press('Escape');
   await actual.locator('#confirm-action').waitFor({state:'hidden'});
   assert.equal(await actual.locator('#attachment-toggle').evaluate(el=>el===document.activeElement),true);
   assert.equal((await metrics(actual)).text,a.text,'cancel changed displayed policy/history');
   await context.close();
  }
  stage='existing-confirmations';
  const row=rows.find(r=>r.State==='on');const context=await browser.newContext();
  await context.addCookies([{name:row.CookieName,value:row.Cookie,url:row.Base}]);
  const cases=[
   ['scope-issuance','제한 권한·자동 등록 발급 켜기',{enabled:'true'}],
   ['http-policy','HTTP 연결 허용',{allowHTTP:'true'}],
   ['session-suspend','세션 일시 중지',{}],['token-revoke','토큰 취소',{}],['project-token-revoke','프로젝트 토큰 취소',{}],['session-grant-release','닉네임 예약 해제',{}],['session-detach','세션 연결 해제',{}],['member-remove','멤버 제외',{}],['member-role','역할 강등',{role:'viewer'}]
  ];
  let extra='<div hidden id="regression-fixtures">';
  for(const [action,,fields]of cases){extra+=`<form data-confirm-action="${action}" data-confirm-target="synthetic" data-current-role="owner">`+Object.entries(fields).map(([name,value])=>`<input name="${name}" value="${value}">`).join('')+'<button>submit</button></form>';}
  extra+='</div>';
  await context.route('**/admin/settings',async route=>{const response=await route.fetch();await route.fulfill({response,body:(await response.text()).replace('</body>',extra+'</body>')});});
  const page=await context.newPage();await page.goto(row.Base+'/admin/settings');
  await page.evaluate(()=>document.addEventListener('submit',e=>e.preventDefault()));
  for(const [action,title]of cases){
   await page.locator(`#regression-fixtures form[data-confirm-action="${action}"]`).evaluate(f=>f.requestSubmit(f.querySelector('button[type=submit], button:not([type])')));
   assert.equal(await page.locator('#confirm-action-title').innerText(),title);
   assert.equal(await page.locator('[data-confirm-submit]').innerText(),title,'existing confirm button label changed');
   await page.keyboard.press('Escape');await page.locator('#confirm-action').waitFor({state:'hidden'});
  }
  const announcement=page.locator('form[data-confirm-action="broker-announcement"]');
  await announcement.locator('textarea[name="body"]').fill('synthetic announcement');
  await announcement.evaluate(f=>f.requestSubmit(f.querySelector('button[type=submit], button:not([type])')));
  assert.equal(await page.locator('#confirm-action-title').innerText(),'공지 접수');
  assert.equal(await page.locator('[data-confirm-submit]').innerText(),'공지 접수');
  await page.keyboard.press('Escape');await page.locator('#confirm-action').waitFor({state:'hidden'});
  await context.close();
  fs.writeFileSync(path.join(out,'summary.json'),JSON.stringify({states:28,existingConfirmationCases:cases.length+1,status:'passed'},null,2));
 }catch(e){fs.writeFileSync(path.join(out,'failure.json'),JSON.stringify({stage,kind:e.name,reason:String(e.message).split('\n')[0].replace(/https?:\/\/\S+/g,'[URL]')}));process.exitCode=1;}
 finally{await browser.close();}
})();
