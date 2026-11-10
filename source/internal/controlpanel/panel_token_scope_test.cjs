const { test } = require("node:test");
const assert = require("node:assert/strict");
const { chromium } = require("playwright");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");

test("auto scope controls submit finite limits only when available and selected", async () => {
 const browser = await chromium.launch();
 try {
 const page = await browser.newPage();
 await page.setContent(`<form><fieldset id="scopes"><label><input type="checkbox" name="scope" value="session:register:auto" data-auto-scope>Auto</label><fieldset data-auto-limits disabled><input name="auto_cap" value="8"><input name="auto_days" value="30"></fieldset></fieldset></form>`);
 await page.addScriptTag({ content: readFileSync(join(__dirname,"panel.js"),"utf8") });
 const fields=()=>page.evaluate(()=>Array.from(new FormData(document.querySelector("form")).keys()));
 assert.deepEqual(await fields(),[]);
 await page.locator("[data-auto-scope]").check();
 assert.deepEqual(await fields(),["scope","auto_cap","auto_days"]);
 await page.locator("[data-auto-scope]").uncheck();
 assert.deepEqual(await fields(),[]);
 await page.evaluate(()=>{document.querySelector("#scopes").disabled=true;document.querySelector("[data-auto-scope]").checked=true;window.dispatchEvent(new Event("pageshow"));});
 assert.equal(await page.locator("[data-auto-limits]").evaluate(e=>e.disabled),true);
 assert.deepEqual(await fields(),[]);
 } finally { await browser.close(); }
});

test("zero limits submit exactly one scalar, with validation and auto-scope gating", async () => {
 const browser = await chromium.launch();
 try {
  const page = await browser.newPage();
  await page.setContent(`<form><input type="checkbox" name="scope" value="session:register:auto" data-auto-scope checked><fieldset data-auto-limits><input type="number" name="auto_cap" min="0" max="100000" value="8" required><input type="number" name="auto_days" min="0" max="36500" value="30" required></fieldset></form>`);
  await page.addScriptTag({content:readFileSync(join(__dirname,"panel.js"),"utf8")});
  const fields=()=>page.evaluate(()=>Object.fromEntries(["auto_cap","auto_days"].map(k=>[k,new FormData(document.querySelector("form")).getAll(k)])));
  assert.deepEqual(await fields(),{auto_cap:["8"],auto_days:["30"]});
  await page.locator('[name="auto_cap"]').fill('0');
  await page.locator('[name="auto_days"]').fill('0');
  await page.evaluate(()=>window.dispatchEvent(new Event('pageshow')));
  assert.deepEqual(await fields(),{auto_cap:["0"],auto_days:["0"]});
  assert.equal(await page.locator('form').evaluate(e=>e.checkValidity()),true);
  for(const key of ['auto_cap','auto_days']) {
   for(const value of ['-1','']) {
    await page.locator(`[name="${key}"]`).fill(value);
    assert.equal(await page.locator('form').evaluate(e=>e.checkValidity()),false);
   }
   await page.locator(`[name="${key}"]`).fill('0');
  }
  await page.locator('[data-auto-scope]').uncheck();
  assert.deepEqual(await fields(),{auto_cap:[],auto_days:[]});
 } finally {await browser.close();}
});
