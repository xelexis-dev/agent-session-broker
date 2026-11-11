const {chromium}=require('playwright');
const {catalog}=require('./i18n_fixture.cjs');
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const {Base}=JSON.parse(fs.readFileSync(process.argv[2],'utf8'));
const out=process.env.ASB_PROFILE_SELECT_EVIDENCE,kit=process.env.ASB_PROFILE_SELECT_DRAFT;
assert(out&&process.env.ASB_FULL_CHROME);fs.mkdirSync(out,{recursive:true});
const cases=[...['default','empty','many'].map(state=>['profile','/profile',state]),['api-tokens','/profile/api-tokens','empty'],...['default','err-cap','err-http'].map(state=>['api-token-new','/profile/api-tokens/new',state]),['bug-reports','/admin/bug-reports','default'],['api-token-new','/profile/api-tokens/new','open-select']];
(async()=>{
 const browser=await chromium.launch({executablePath:process.env.ASB_FULL_CHROME,headless:true});const rows=[];let stage='start';
 try{
 for(const [file,route,state]of cases)for(const role of file==='bug-reports'?['admin']:['admin','user'])for(const theme of ['light','dark'])for(const width of [1280,390]){
  const stem=`${file}-${state}-${role}-${theme}-${width}`;stage=stem;
  const context=await browser.newContext({viewport:{width,height:1000},colorScheme:theme,reducedMotion:'reduce'});
  const actual=await context.newPage(),draft=kit?await context.newPage():null;let errors=0;actual.on('pageerror',()=>errors++);
  await actual.addInitScript(t=>localStorage.setItem('asb-theme',t),theme);
  await actual.goto(`${Base}${route}?state=${state}&role=${role}`);
  if(draft)await draft.goto(`${kit}/out/${file==='bug-reports'?'kimi':'opus'}/${file}.html?state=${state==='open-select'?'default':state}&role=${role}&theme=${theme}`);
  const text=catalog((await actual.locator('html').getAttribute('lang'))||'en');
  await actual.evaluate(()=>document.fonts.ready);if(draft)await draft.evaluate(()=>document.fonts.ready);
  if(state==='open-select'){await actual.getByRole('combobox',{name:text['tokens.validity'],exact:true}).click();if(draft)await draft.getByRole('combobox',{name:'유효 기간',exact:true}).click();}
  const selector=file==='bug-reports'?'.bug-list':'.profile-content';
  const metric=page=>page.locator(selector).evaluate(e=>{const r=e.getBoundingClientRect();return {x:r.x,y:r.y,width:r.width,height:r.height,overflow:document.documentElement.scrollWidth>innerWidth};});
  const a=await metric(actual),d=draft?await metric(draft):null;
  const imgs=[];for(const [page,label]of [[draft,'draft'],[actual,'actual']].filter(([page])=>page))imgs.push(await page.screenshot({path:path.join(out,stem+'-'+label+'.png'),fullPage:true}));
  if(draft){const compare=await context.newPage();await compare.setContent('<canvas></canvas>');
  await compare.evaluate(async urls=>{const ims=await Promise.all(urls.map(src=>new Promise(resolve=>{const i=new Image();i.onload=()=>resolve(i);i.src=src})));const c=document.querySelector('canvas');c.width=ims[0].width+ims[1].width;c.height=Math.max(...ims.map(i=>i.height));const x=c.getContext('2d');x.drawImage(ims[0],0,0);x.drawImage(ims[1],ims[0].width,0);document.body.style.margin='0'},imgs.map(b=>'data:image/png;base64,'+b.toString('base64')));
  await compare.locator('canvas').screenshot({path:path.join(out,stem+'-side-by-side.png')});
  }
  const protruding=await actual.locator('.profile-content .card, .profile-forms, .bug-list').evaluateAll(cards=>cards.flatMap(card=>[...card.querySelectorAll('input:not([type=hidden]),select,button,.api-scope,.profile-project')].filter(e=>e.getClientRects().length&&e.getBoundingClientRect().width>card.getBoundingClientRect().width+1).map(e=>e.tagName)));
  rows.push({file,state,role,theme,width,actual:a,draft:d,protruding});fs.writeFileSync(path.join(out,'results.json'),JSON.stringify(rows,null,2));
  assert.equal(a.overflow,false,stem+' horizontal overflow');assert.deepEqual(protruding,[]);assert.equal(errors,0);
  if(file!=='bug-reports')assert.equal(await actual.locator('.profile-account .profile-project-list, .profile-account .profile-projects').count(),0);
  if(file==='profile')assert.equal(await actual.locator('.profile-project').count(),state==='empty'?0:state==='many'?12:role==='user'?2:5);
  if(file==='api-token-new'){
   assert.equal(await actual.locator('[name=current_password]').count(),0);
   assert.equal(await actual.locator('[name=scope][value=admin]').count(),role==='admin'?1:0);
   if(state==='default'){
    const trigger=actual.getByRole('combobox',{name:text['tokens.validity'],exact:true});assert.equal(await trigger.getAttribute('aria-describedby'),'api-expiry-help');await trigger.focus();await actual.keyboard.press('End');await actual.keyboard.press('Enter');assert.equal(await actual.locator('[name=expires_days]').inputValue(),'90');
   }
  }
  await context.close();
 }
 fs.writeFileSync(path.join(out,'summary.json'),JSON.stringify({pairs:rows.length,status:'passed',note:'Geometry differences are retained for review.'},null,2));
 }catch(e){fs.writeFileSync(path.join(out,'failure.json'),JSON.stringify({stage,kind:e.name,reason:String(e.message).split('\n')[0].replace(/asb_[psu]_[^\s]+/g,'[redacted]')}));process.exitCode=1}finally{await browser.close()}
})();
