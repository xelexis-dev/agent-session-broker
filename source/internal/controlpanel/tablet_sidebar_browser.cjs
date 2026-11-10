'use strict';
const assert=require('node:assert/strict');
const fs=require('node:fs');
const {chromium}=require('playwright');

async function checkShell(page,width){
 const sidebar=page.locator('.sidebar');
 if(width<=600){
  assert.equal(await sidebar.isVisible(),false,'drawer initially closed');
  await page.locator('[data-mobile-menu-toggle]').click();
  await sidebar.waitFor({state:'visible'});
 }
 const geometry=await page.evaluate(()=>{
  const box=e=>{const r=e.getBoundingClientRect();return{x:r.x,y:r.y,right:r.right,bottom:r.bottom,width:r.width,height:r.height}};
  const s=document.querySelector('.sidebar');const v=s.querySelector('.sidebar__version');const a=s.querySelector('.avatar');
  return{sidebar:box(s),position:getComputedStyle(s).position,main:box(document.querySelector('main')),workspace:box(document.querySelector('.workspace')),scroll:document.documentElement.scrollWidth,viewport:innerWidth,version:getComputedStyle(v).display,avatar:getComputedStyle(a).display};
 });
 const s=geometry.sidebar;
 if(width>600){
  assert.equal(s.width,width>1100?232:72,'sidebar width');assert.equal(s.x,0);assert.equal(s.y,0);assert.equal(geometry.position,'sticky');
  assert.ok(geometry.main.x>=s.right,'main overlaps rail');
  if(width<=1100)assert.equal(geometry.version,'none','version must not spill down the icon rail');
 }else{
  assert.equal(geometry.position,'fixed');assert.equal(s.right,width);assert.ok(s.width<=Math.min(300,width*.86)+1);assert.equal(geometry.workspace.x,0,'drawer must overlay rather than displace main');
 }
 assert.notEqual(geometry.avatar,'none','account avatar hidden by legacy rules');
 assert.ok(geometry.scroll<=width+1,'horizontal page scroll');
 const links=sidebar.locator('.nav__item');
 for(let i=0;i<await links.count();i++){
  const link=links.nth(i);await link.scrollIntoViewIfNeeded();const b=await link.boundingBox();assert.ok(b&&b.width>0&&b.height>0,'nav item invisible');
  assert.ok(b.x>=s.x&&b.x+b.width<=s.right+1,'nav item outside sidebar');await link.click({trial:true});
 }
 const toggle=sidebar.locator('[data-user-menu-toggle]');await toggle.scrollIntoViewIfNeeded();await toggle.click();
 const menu=page.locator('.user-menu-items');assert.equal(await menu.isVisible(),true);
 const m=await menu.boundingBox();const viewport=page.viewportSize();assert.ok(m.x>=0&&m.y>=0&&m.x+m.width<=viewport.width+1&&m.y+m.height<=viewport.height+1,'account menu outside viewport');
 await menu.getByRole('menuitem',{name:'내 프로필',exact:true}).click();await page.waitForURL(u=>u.pathname==='/profile');
 if(width<=600){await page.locator('[data-mobile-menu-toggle]').click();}
 // A real nav click must navigate, beyond merely being laid out on screen.
 await page.locator('.sidebar .nav__item').first().click();await page.waitForURL(u=>u.pathname==='/');
}

(async()=>{
 let browser;
 try{
  const cfg=JSON.parse(fs.readFileSync(process.argv[2]));assert.equal(new URL(cfg.Base).origin,'http://127.0.0.1:18897');
  browser=await chromium.launch({executablePath:process.env.ASB_CHROME_PATH||'/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',headless:true});
  let count=0;
  for(const theme of ['light','dark']){
   const context=await browser.newContext({viewport:{width:1280,height:960},colorScheme:theme,reducedMotion:'reduce'});const page=await context.newPage();
   await page.goto(cfg.Base+'/login');await page.locator('[name=username]').fill('admin');await page.locator('[name=password]').fill(fs.readFileSync(cfg.PasswordFile,'utf8'));
   await Promise.all([page.waitForURL(u=>u.pathname==='/'),page.locator('form[action="/login"]').locator('button[type=submit],button:not([type])').click()]);
   for(const width of [1280,1150,1100,1024,834,768,760,700,620,601,600,390]){
    await page.setViewportSize({width,height:960});
    for(const url of ['/', '/projects/sessions?project=tablet-demo','/projects/settings?project=tablet-demo','/admin/users','/admin/settings','/profile','/profile/api-tokens','/auth/code']){
     await page.goto(cfg.Base+url);await page.evaluate(()=>document.fonts.ready);await checkShell(page,width);count++;
    }
   }
   await context.close();
  }
  console.log(JSON.stringify({tabletShellChecks:count,passed:true}));
 }catch(e){console.log(JSON.stringify({tabletShellFailure:e.name,assertion:e.code==='ERR_ASSERTION'?e.message:'browser operation failed'}));process.exitCode=1;}
 finally{if(browser)await browser.close();}
})();
