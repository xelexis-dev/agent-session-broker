const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const {chromium}=require('playwright');
const {Base}=JSON.parse(fs.readFileSync(process.argv[2],'utf8'));
const out=process.env.ASB_ADMIN_API_UI_EVIDENCE,draftBase=process.env.ASB_ADMIN_API_DRAFT_URL;
if(!out)throw Error('evidence directory required');
fs.mkdirSync(out,{recursive:true});
const cases=[
 ['profile','profile','default'],['api-tokens','api-tokens','empty'],['api-tokens','api-tokens','tokens'],['api-tokens','api-tokens','issued'],['api-tokens','api-tokens','off'],
 ...['default','err-cap','err-rate','err-http'].map(s=>['api-token-new','api-token-new',s]),
 ['settings','admin-settings','off'],['settings','admin-settings','on'],['user-detail','user-detail','tokens'],['user-detail','user-detail','empty']
];
(async()=>{
 const browser=await chromium.launch({executablePath:process.env.ASB_FULL_CHROME||'/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',headless:true});
 const results=[];let stage='setup';
 try{
 for(const [file,view,state]of cases)for(const role of view.startsWith('admin')||view==='user-detail'?['admin']:['admin','user'])for(const theme of ['light','dark'])for(const width of [1280,390]){
  const stem=`${file}-${state}-${role}-${theme}-${width}`;stage=stem+':load';
  const context=await browser.newContext({viewport:{width,height:1000},colorScheme:theme,timezoneId:'Asia/Seoul',reducedMotion:'reduce'});
  await context.addInitScript(theme=>localStorage.setItem('asb-theme',theme),theme);
  const actual=await context.newPage(),draft=draftBase?await context.newPage():null;
  await actual.goto(`${Base}/?view=${view}&state=${state}&role=${role}`);
  if(draft)await draft.goto(`${draftBase}/${file}.html?state=${state}&role=${role}&theme=${theme}`);
  await actual.evaluate(()=>document.fonts.ready);if(draft)await draft.evaluate(()=>document.fonts.ready);
  const selector=view==='admin-settings'?'#admin-api':view==='user-detail'?'#api-tokens':'.profile-content';
  const metrics=async page=>page.locator(selector).evaluate(el=>{const r=el.getBoundingClientRect();return {x:r.x,y:r.y,width:r.width,height:r.height,overflow:document.documentElement.scrollWidth>innerWidth,text:el.innerText.replace(/\s+/g,' ').trim()};});
  const a=await metrics(actual),d=draft?await metrics(draft):null;
  // Tokens are synthetic and intentionally never serialized into the evidence JSON.
  const scrub=s=>s.replace(/asb_[psu]_[^\s]+/g,'[redacted]');a.text=scrub(a.text);if(draft)d.text=scrub(d.text);
  const ap=await actual.screenshot({path:path.join(out,stem+'-actual.png'),fullPage:true});
  if(draft){const dp=await draft.screenshot({path:path.join(out,stem+'-draft.png'),fullPage:true});
  const compare=await context.newPage();await compare.setContent('<canvas></canvas>');
  await compare.evaluate(async data=>{const images=await Promise.all(data.map(src=>new Promise(resolve=>{const im=new Image();im.onload=()=>resolve(im);im.src=src;})));const c=document.querySelector('canvas');c.width=images[0].width+images[1].width;c.height=Math.max(...images.map(i=>i.height));const ctx=c.getContext('2d');ctx.drawImage(images[0],0,0);ctx.drawImage(images[1],images[0].width,0);document.body.style.margin='0';},[dp,ap].map(b=>'data:image/png;base64,'+b.toString('base64')));
  await compare.locator('canvas').screenshot({path:path.join(out,stem+'-side-by-side.png')});
  }
  const differences=[];if(draft){for(const key of ['x','y','width','height'])if(Math.abs(a[key]-d[key])>1)differences.push(key);
  if(a.text!==d.text)differences.push('text');if(a.overflow)differences.push('overflow');
  for(const shell of ['.breadcrumb','.page-heading'])if((await actual.locator(shell).innerText()).replace(/\s+/g,' ').trim()!==(await draft.locator(shell).innerText()).replace(/\s+/g,' ').trim())differences.push(shell);
  }
  assert.equal(a.overflow,false);assert.ok(a.width>0&&a.height>0);
  results.push({file,state,role,theme,width,actual:a,draft:d,differences,approvedShellDifference:role==='user'});
  fs.writeFileSync(path.join(out,'results.json'),JSON.stringify(results,null,2));
  stage=stem+':behavior';
  if(role==='user')assert.equal(await actual.locator('.sidebar a[href="/admin/settings"]').count(),0);
  if(view==='api-token-new'){
   assert.equal(await actual.locator('[name=current_password]').count(),0);
   assert.equal(await actual.locator('[name=scope][value=admin]').count(),role==='admin'?1:0);
   assert.equal(await actual.locator('[name=expires_days]').inputValue(),'30');
   assert.equal(await actual.locator('[name=scope][value=read]').isChecked(),true);assert.equal(await actual.locator('[name=scope][value=write]').isChecked(),true);
   assert.equal(await actual.locator('[name=scope][value=approve]').isChecked(),false);
   const disabled=['err-cap','err-rate','err-http'].includes(state);
   assert.equal(await actual.locator('#api-token-issue').evaluate(f=>[...f.elements].filter(e=>e.matches('input:not([type=hidden]),select,button')).every(e=>e.disabled)),disabled);
  }
  const forms=actual.locator('form[data-api-confirm]');
  for(let i=0;i<await forms.count();i++){
   const form=forms.nth(i),button=form.locator('button');const expected=await form.evaluate(f=>({...f.dataset}));
   await button.click();assert.equal(await actual.locator('#confirm-action-title').innerText(),expected.confirmTitle);assert.equal(await actual.locator('#confirm-action-description').innerText(),expected.confirmDescription);assert.equal(await actual.locator('[data-confirm-submit]').innerText(),expected.confirmButton);
   assert.equal(await actual.locator('#confirm-action [autofocus]').evaluate(e=>e===document.activeElement),true);
   await actual.keyboard.press('Escape');await actual.locator('#confirm-action').waitFor({state:'hidden'});assert.equal(await button.evaluate(e=>e===document.activeElement),true);
  }
  if(state==='issued'){
   // Reload to verify receipt focus before interacting with the copy fallback.
   await actual.reload();assert.equal(await actual.locator('#api-issued-title').evaluate(e=>e===document.activeElement),true);
   await actual.evaluate(()=>Object.defineProperty(navigator,'clipboard',{configurable:true,value:{writeText:async()=>{throw Error('synthetic clipboard denial');}}}));
   await actual.locator('[data-copy-token]').click();assert.equal(await actual.locator('[data-copy-status]').innerText(),'토큰을 선택했습니다. 복사하세요.');
   assert.equal(await actual.evaluate(()=>window.getSelection().toString()===document.getElementById('issued-token').textContent),true);
   await actual.evaluate(()=>Object.defineProperty(navigator,'clipboard',{configurable:true,value:{writeText:async()=>{}}}));
   await actual.locator('[data-copy-token]').click();assert.equal(await actual.locator('[data-copy-status]').innerText(),'토큰을 복사했습니다.');
  }
  await context.close();
 }
 const differences=results.filter(r=>r.differences.length);
 fs.writeFileSync(path.join(out,'summary.json'),JSON.stringify({pairs:results.length,materialDifferencePairs:differences.length,status:differences.length?'comparison-needs-review':'passed'},null,2));
 assert.equal(differences.length,0,'selected design component differences require review');
 }catch(e){fs.writeFileSync(path.join(out,'failure.json'),JSON.stringify({stage,kind:e.name,reason:String(e.message).split('\n')[0].replace(/asb_[psu]_[^\s]+/g,'[redacted]').replace(/https?:\/\/\S+/g,'[URL]')}));process.exitCode=1;}
 finally{await browser.close();}
})();
