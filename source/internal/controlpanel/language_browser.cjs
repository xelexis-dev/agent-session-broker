const {chromium} = require('playwright');
const axe = require('axe-core');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {catalog} = require('./i18n_fixture.cjs');

(async () => {
 const config=JSON.parse(fs.readFileSync(process.argv[2],'utf8'));
 const evidence=process.env.ASB_LANGUAGE_EVIDENCE;
 assert(evidence && process.env.ASB_FULL_CHROME);
 fs.mkdirSync(evidence,{recursive:true});
 const browser=await chromium.launch({executablePath:process.env.ASB_FULL_CHROME,args:['--keep-alive-for-test']});
 const rows=[];let stage='start';
 try {
  for(const width of [1280,390]) for(const theme of ['light','dark']) for(const language of ['en','ko']) {
   stage=`${language}-${width}-${theme}`;
   const context=await browser.newContext({viewport:{width,height:900},colorScheme:theme});
   await context.addInitScript(value=>localStorage.setItem('asb-theme',value),theme);
   const page=await context.newPage();
   await page.goto(config.base+'/login');
   await page.locator('input[name=username]').fill('admin');
   await page.locator('input[name=password]').fill(config.password);
   await Promise.all([page.waitForNavigation(),page.locator('form[action="/login"] button').click()]);
   await page.goto(config.base+'/profile');
   let form=page.locator('form[action="/profile/language"]');
   await form.locator('.select__trigger').click();
   await form.locator('[role=option]').filter({hasText:language==='ko'?'한국어':'English'}).click();
   assert.equal(await form.locator('select').inputValue(),language);
   await Promise.all([page.waitForNavigation(),form.locator('button.primary').click()]);
   assert.equal(new URL(page.url()).pathname,'/profile');
   form=page.locator('form[action="/profile/language"]');
   assert.equal(await form.locator('select').inputValue(),language);
   const cookie=(await context.cookies()).find(value=>value.name==='asb_lang');
   assert(cookie && cookie.value===language && cookie.httpOnly && cookie.sameSite==='Lax' && !cookie.secure);
   assert.equal(await page.locator('html').getAttribute('data-theme'),theme);
   assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
   await page.evaluate(()=>document.fonts.ready);
   await page.evaluate(axe.source);
   const violations=await page.evaluate(async()=>{
    const result=await axe.run(document,{resultTypes:['violations']});
    return result.violations.filter(v=>['serious','critical'].includes(v.impact)).map(v=>({id:v.id,impact:v.impact,count:v.nodes.length}));
   });
   assert.equal(violations.length,0,'profile has serious/critical axe violations');
   await page.locator('#profile-language-title').scrollIntoViewIfNeeded();
   await page.screenshot({path:path.join(evidence,stage+'.png'),fullPage:false});
   rows.push({language,width,theme,violations});
   // Each changed page uses the saved account language, including a scoped-token editor.
   const text=catalog(language);
   const surfaces=[['projects','/','#project-list-title','projects.projects'],
    ['settings','/projects/settings?project=demo','#project-token-title','tokens.project_token'],
    ['scope-editor','/projects/settings?project=demo&edit='+config.editor,'#scope-editor-title','tokens.edit_token_scopes'],
    ['api-tokens','/profile/api-tokens','#api-token-list-title','tokens.issued_tokens'],
    ['api-token-new','/profile/api-tokens/new','#api-new-title','tokens.token_details']];
   for(const [name,url,selector,key] of surfaces) {
    stage=`${name}-${language}-${width}-${theme}`;
    await page.goto(config.base+url);
    assert.equal(await page.locator('html').getAttribute('lang'),language);
    assert.equal((await page.locator(selector).innerText()).trim(),text[key]);
    if(name==='settings') {
     assert.equal((await page.locator('[data-scope-help="session:manage"]').innerText()).trim(),text['tokens.scope_manage_help']);
    }
    assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'page overflow');
    await page.evaluate(axe.source);
    const findings=await page.evaluate(async()=>{
     const result=await axe.run(document,{resultTypes:['violations']});
     return result.violations.filter(v=>['serious','critical'].includes(v.impact)).map(v=>({id:v.id,impact:v.impact,count:v.nodes.length}));
    });
    assert.equal(findings.length,0,'translated page has serious/critical axe violations');
    await page.screenshot({path:path.join(evidence,stage+'.png'),fullPage:false});
    rows.push({surface:name,language,width,theme,violations:findings});
    if(name==='projects'||name==='settings') {
     const id=name==='projects'?'create-project':'add-project-member';
     await page.locator(`[data-dialog-open="${id}"]`).click();
     assert(await page.locator('#'+id).isVisible());
     await page.locator('#'+id+' [data-dialog-close]').first().click();
    }
   }
   await context.close();
  }
  // The native select/form must still work when custom-select JS is unavailable.
  stage='no-js';
  const context=await browser.newContext({javaScriptEnabled:false});
  const page=await context.newPage();await page.goto(config.base+'/login');
  await page.locator('input[name=username]').fill('admin');await page.locator('input[name=password]').fill(config.password);
  await Promise.all([page.waitForNavigation(),page.locator('form[action="/login"] button').click()]);
  await page.goto(config.base+'/profile');const form=page.locator('form[action="/profile/language"]');await form.locator('select').selectOption('en');
  await Promise.all([page.waitForNavigation(),form.locator('button.primary').click()]);
  assert.equal(await page.locator('form[action="/profile/language"] select').inputValue(),'en');await context.close();
 } catch(error) {
  fs.writeFileSync(path.join(evidence,'failure.json'),JSON.stringify({stage,kind:error.name,reason:'language browser/axe assertion failed'}));
  process.exitCode=1;
 } finally {
  fs.writeFileSync(path.join(evidence,'results.json'),JSON.stringify({axe:axe.version,rows},null,2));
  await browser.close();
 }
})();
