const { test, before, after } = require("node:test");
const assert = require("node:assert/strict");
const { chromium } = require("playwright");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const { createServer } = require("node:http");
let browser;
before(async () => { browser = await chromium.launch(); });
after(async () => { await browser?.close(); });

async function fixture(t) {
 const script = readFileSync(join(__dirname,"panel.js"));
 const server = createServer(async (req,res) => {
  if (req.url === "/panel.js") { res.writeHead(200,{"Content-Type":"text/javascript"}).end(script); return; }
  if (req.method === "POST") {
   let body = ""; for await (const chunk of req) body += chunk;
   res.writeHead(200,{"Content-Type":"application/json"}).end(JSON.stringify(Object.fromEntries(new URLSearchParams(body)))); return;
  }
  res.writeHead(200,{"Content-Type":"text/html"}).end(`<!doctype html><script src="/panel.js" defer></script><form action="/auth/code" method="post"><input name="code" required><input name="nickname" required><button>Approve</button></form><form action="/profile/save"><input name="nickname" value="  leave alone  "></form><button id="outside">outside</button>`);
 });
 await new Promise(resolve=>server.listen(0,"127.0.0.1",resolve));
 const page = await browser.newPage();
 t.after(async()=>{await page.close();server.closeAllConnections();await new Promise(resolve=>server.close(resolve));});
 await page.goto(`http://127.0.0.1:${server.address().port}/`);
 return page;
}

test("approval blur trims edges only and leaves other forms untouched",async t=>{
 const page=await fixture(t);
 await page.locator('form[action="/auth/code"] input[name="code"]').fill("  WDJB-MJHT  ");
 await page.locator('form[action="/auth/code"] input[name="nickname"]').fill("  agent one  ");
 await page.locator("#outside").focus();
 assert.equal(await page.locator('input[name="code"]').inputValue(),"WDJB-MJHT");
 assert.equal(await page.locator('form[action="/auth/code"] input[name="nickname"]').inputValue(),"agent one");
 assert.equal(await page.locator('form[action="/profile/save"] input').inputValue(),"  leave alone  ");
});

test("approval submit posts clean code and nickname without a prior blur",async t=>{
 const page=await fixture(t);
 await Promise.all([page.waitForURL("**/auth/code"),page.evaluate(()=>{
  const form=document.querySelector('form[action="/auth/code"]');
  form.elements.code.value="  WDJB-MJHT  ";form.elements.nickname.value="  agent-one  ";form.requestSubmit();
 })]);
 assert.deepEqual(JSON.parse(await page.locator("body").innerText()),{code:"WDJB-MJHT",nickname:"agent-one"});
});
