"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
const baseURL = process.argv[2];

(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless:true, ...(engine===chromium ? {channel:"chromium"} : {})});
    try {
      const page = await browser.newPage({ignoreHTTPSErrors:true});
      const sends=[], errors=[];
      let releaseAssets;
      await page.route("**/codex/assets/conversation.js*", async route=>{
        await new Promise(resolve=>{releaseAssets=resolve;});
        return route.continue();
      });
      let originalID,releaseSafe;
      const safeIDs=[];
      page.on("pageerror", error=>errors.push(error.message));
      await page.addInitScript(()=>{
        window.EventSource=class {constructor() {queueMicrotask(()=>this.onopen?.());} addEventListener() {} close() {}};
      });
      await page.route("**/api/sessions/example/**", async route=>{
        const operation=new URL(route.request().url()).pathname.split("/").at(-1);
        if (["thread","page"].includes(operation)) return route.fulfill({json:{threadId:"thread-1",status:"idle",latestTurnId:"turn-1",model:"model-1",reasoningEffort:"low",collaborationMode:"default",entries:[],hasOlder:false}});
        if (operation==="message") {
          const body=route.request().postDataJSON(); sends.push(body);
          if (body.message==="Retry known unsent") {
            safeIDs.push(body.clientUserMessageId);
            if (safeIDs.length===1) return route.fulfill({status:503,json:{code:"message_not_sent",notSent:true,error:"The message was not sent."}});
            await new Promise(resolve=>{releaseSafe=resolve;});
            return route.fulfill({status:202,json:{clientUserMessageId:body.clientUserMessageId,turnId:"accepted"}}).catch(()=>{});
          }
          if (!originalID) originalID=body.clientUserMessageId;
          if (body.clientUserMessageId===originalID) return route.fulfill({status:409,json:{error:"Message delivery could not be confirmed.",code:"message_delivery_unknown",notSent:false}});
          return route.fulfill({status:202,json:{clientUserMessageId:body.clientUserMessageId,turnId:"accepted"}});
        }
        if (["pending","queue"].includes(operation)) return route.fulfill({json:[]});
        if (operation==="reconcile") return route.fulfill({json:{ok:true}});
        if (operation==="details") return route.fulfill({json:{repositoriesHTML:"",teamHTML:""}});
        if (operation==="activity") return route.fulfill({json:{currentState:"idle",workingMs:0,waitingMs:0,coverageComplete:true}});
        return route.continue();
      });
      await page.goto(baseURL+"/example/#codex",{waitUntil:"domcontentloaded"});
      await expect.poll(()=>Boolean(releaseAssets)).toBe(true);
      await expect(page.locator("#message-send")).toBeDisabled();
      releaseAssets();
      await page.unroute("**/codex/assets/conversation.js*");
      await expect(page.locator("#message-send")).toBeEnabled();
      const input=page.locator('#message-form textarea[name="message"]');
      const banner=page.locator("#message-submission-status");
      const receipt=text=>page.locator(".message-receipt").filter({has:page.locator("span",{hasText:text})});
      await input.fill("Unconfirmed original");
      await page.locator("#message-send").click();
      await expect(banner).toBeVisible();
      await expect(receipt("Unconfirmed original")).toContainText("Outcome unknown.");
      await expect(banner).not.toContainText("service is unavailable");
      await input.fill("Reworded successful message");
      await page.locator("#message-send").click();
      await expect(banner).toBeHidden();
      await expect(receipt("Unconfirmed original")).toContainText("Outcome unknown.");

      await input.fill("Keep this newer draft");
      page.once("dialog", dialog=>{
        assert(dialog.message().includes("original message may still arrive"));
        void dialog.accept();
      });
      await receipt("Unconfirmed original").getByRole("button",{name:"Send another copy…",exact:true}).click();
      await expect.poll(()=>sends.length).toBe(4);
      assert.equal(sends[2].clientUserMessageId,originalID);
      assert.equal(sends[2].retry,true);
      assert.notEqual(sends[3].clientUserMessageId,originalID);
      assert.equal(sends[3].message,"Unconfirmed original");
      await expect(banner).toBeHidden();
      await expect(input).toHaveValue("Keep this newer draft");
      const replacementID=sends[3].clientUserMessageId;
      assert.equal(await page.evaluate(id=>JSON.parse(sessionStorage.getItem(`workspace-portal.send-attempt.example.thread-1.${id}`)).replacementId,originalID),replacementID);
      await page.reload();
      await expect(page.getByRole("button",{name:"Send another copy…",exact:true})).toHaveCount(0);
      await expect(page.locator("#message-receipts")).toContainText("Another copy requested");
      assert.equal(sends.length,4,"reload must not send another copy");

      await input.fill("Retry known unsent");
      await page.locator("#message-send").click();
      await expect(receipt("Retry known unsent")).toContainText("Not sent.");
      await input.fill("Retry known unsent");
      await page.locator("#message-send").click();
      await expect.poll(()=>Boolean(releaseSafe)).toBe(true);
      assert.equal(safeIDs[0],safeIDs[1]);
      await expect(receipt("Retry known unsent")).toContainText("Sending…");
      await page.reload();
      await expect(receipt("Retry known unsent")).toContainText("Outcome unknown.");
      releaseSafe();
      assert.deepEqual(errors,[]);
    } finally { await browser.close(); }
  }
})().catch(error=>{console.error(error);process.exitCode=1;});
