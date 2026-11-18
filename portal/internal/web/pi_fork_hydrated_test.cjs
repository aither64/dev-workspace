"use strict";
const assert = require("node:assert/strict");
const {chromium,firefox} = require("playwright");
const input=JSON.parse(process.argv[2]);
(async()=>{
 for(const [name,engine] of [["chromium",chromium],["firefox",firefox]]){
  const browser=await engine.launch({headless:true});
  try {
   const page=await browser.newPage();const requests=[];
   page.on("request",request=>{if(request.method()==="POST")requests.push({url:request.url(),body:request.postDataJSON(),scope:request.headers()["x-workspace-conversation-scope"]});});
   await page.goto(input.origin+input.path,{waitUntil:"domcontentloaded"});
   assert.equal(await page.locator("body").getAttribute("data-thread-id"),input.nativeId);
   assert.equal(await page.locator("body").getAttribute("data-root-thread-id"),"");
   await page.locator("#fork-open").click();
   await page.locator("#fork-dialog").waitFor({state:"visible"});
   assert.equal(await page.locator('#fork-form [name="model"],#fork-form [name="effort"],#fork-form [name="provider"]').count(),0);
   await page.locator('#fork-form [name="name"]').fill("fork");
   await Promise.all([page.waitForURL(input.origin+input.destination),page.locator('#fork-form button[type="submit"]').click()]);
   assert.equal(requests.length,1);assert.equal(new URL(requests[0].url).searchParams.get("member"),"reviewer0");
   assert.equal(requests[0].scope,input.scope);
   assert.deepEqual(requests[0].body,{name:"fork",creationDate:"2026-11-18",browserNotificationsEnabled:true});
   console.log(name+" selected-member Fork hydration PASS");
  }finally{await browser.close();}
 }
})().catch(error=>{console.error(error);process.exitCode=1;});
