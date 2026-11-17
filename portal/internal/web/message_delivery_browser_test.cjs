"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
const baseURL = process.argv[2];

(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless:true, ...(engine===chromium ? {channel:"chromium"} : {})});
    try {
      const page = await browser.newPage({ignoreHTTPSErrors:true});
      if (process.env.PI_MESSAGE_ONLY === "1") {
        const scope = "pi-workflow-v1:" + "a".repeat(64), slug = "2026-11-14-transcript-fixture";
        const base = `/api/sessions/${slug}`, conversation = `/codex/conversations/${slug}`;
        const sends = [], errors = [], unexpected = [];
        const file = {clientId: "11111111-1111-4111-8111-111111111111", id: "22222222-2222-4222-8222-222222222222", name: "generic.txt", size: 4, state: "ready"};
        const draftKey = `workspace-portal.upload-draft.${slug}.pi-thread`;
        let status = "active", nextRead = null, nextSend = null, rejectMessage = false, reads = 0;
        const hold = () => {let release; const promise = new Promise(resolve => {release = resolve;}); return {promise, release, started: false, finished: false};};
        page.on("pageerror", error => errors.push(error.message));
        await page.addInitScript(({draftKey, file}) => {
          if (!sessionStorage.getItem("pi-fixture-seeded")) {localStorage.setItem(draftKey, JSON.stringify([file])); sessionStorage.setItem("pi-fixture-seeded", "1");}
          window.fixtureEvents = [];
          window.EventSource = class {constructor() {window.fixtureEvents.push(this); queueMicrotask(() => this.onopen?.());} addEventListener() {} close() {}};
        }, {draftKey, file});
        const refresh = () => page.evaluate(() => {for (const source of window.fixtureEvents) source.onmessage?.({data: "{}"});});
        await page.route("**/*", async route => {
          const request = route.request(), path = new URL(request.url()).pathname;
          const fulfill = json => route.fulfill({json, headers: {"X-Workspace-Conversation-Scope": scope}});
          const operation = path.startsWith(conversation + "/") ? path.slice(conversation.length + 1) : null;
          const sessionRead = path.startsWith(base + "/") ? path.slice(base.length + 1) : null;
          if (operation || ["details", "pi-questions", "pi-plan"].includes(sessionRead) || path.startsWith("/uploads/")) assert.equal(request.headers()["x-workspace-conversation-scope"], scope);
          if (["thread", "thread/page"].includes(operation)) {
            const observed = status;
            reads++;
            const wait = nextRead; nextRead = null;
            if (wait) {wait.started = true; await wait.promise;}
            await fulfill({threadId: "pi-thread", status: observed, latestTurnId: "turn-1", model: "model-1", reasoningEffort: "high", entries: [], hasOlder: false}).catch(() => {});
            if (wait) wait.finished = true;
            return;
          }
          if (operation === "message") {
            const body = request.postDataJSON(); sends.push(body);
            const wait = nextSend; nextSend = null;
            if (wait) {wait.started = true; await wait.promise;}
            if (rejectMessage) return route.fulfill({status: 503, headers: {"X-Workspace-Conversation-Scope": scope},
              json: {code: "message_not_sent", notSent: true, ...(rejectMessage === "missing" ? {} : {retryable: false}), error: "The message was not sent."}});
            status = "active";
            return route.fulfill({status: 202, headers: {"X-Workspace-Conversation-Scope": scope}, json: {clientUserMessageId: body.clientUserMessageId, turnId: "accepted"}});
          }
          if (operation === "queue/reconcile") {assert.equal(request.postData(), "{}"); return fulfill({ok: true});}
          if (request.method() !== "GET") {unexpected.push(`${request.method()} ${path}`); return route.fulfill({status: 405, json: {error: "unexpected fixture mutation"}});}
          if (sessionRead === "details") return fulfill({repositoriesHTML: "", artifactsHTML: "", sessionBackend: "pi", conversationScope: scope, conversationInteractive: true, conversationHeld: false, readyMembers: []});
          if (operation === "activity") return fulfill({currentState: status === "idle" ? "idle" : "working", workingMs: 0, waitingMs: 0, coverageComplete: true});
          if (operation === "retry/activity") return fulfill({serverNowMs: Date.now(), activity: null});
          if (["pending", "queue"].includes(operation)) return fulfill([]);
          if (sessionRead === "pi-questions") return fulfill({revision: 0, questions: []});
          if (sessionRead === "pi-plan") return fulfill({revision: 0, proposals: [], acceptances: []});
          if (sessionRead === "operation") return fulfill({state: "idle", options: {}});
          if (path === `/uploads/s-${slug}`) return fulfill({files: [file], limits: {chunkBytes: 4194304, fileBytes: 1073741824, promptBytes: 2147483648, files: 50}});
          if (operation === "models" || ["/api/models", "/codex/models"].includes(path)) return fulfill([{id: "model-1", model: "model-1", supportedReasoningEfforts: [{reasoningEffort: "high"}], defaultReasoningEffort: "high"}]);
          if (operation === "collaboration-modes" || path === "/api/collaboration-modes") return fulfill([{mode: "default", name: "Default"}]);
          if (sessionRead === "retry-policy") return fulfill({revision: "fixture", capacity: false, cybersecurity: false});
          if (sessionRead === "auto-archive") return fulfill({enabled: false, hold: true, blockers: []});
          if (sessionRead === "progress") return fulfill({available: false});
          if (sessionRead === "attention") return fulfill({version: 1, identity: await page.locator("body").getAttribute("data-session-observation"), rootThread: "", enabled: false, revision: 0, event: null, fresh: true});
          if (sessionRead === "coordination") return fulfill({workspaces: [], reservationHolders: [], jobs: []});
          if (sessionRead === "communication") return fulfill({candidates: [], grants: [], messages: []});
          if (path === "/api/browser-notifications") return fulfill({version: 1, deliveryEpoch: 1, publicKey: "", registered: false, available: false});
          if (path === "/api/codex-limits") return fulfill({windows: [], canReset: false});
          if (path === "/api/codex-providers") return fulfill({providers: [], inherited: true});
          if (path.startsWith("/static/") || path.startsWith("/codex/assets/") || path === "/fixture/pi-transcript") return route.continue();
          if (path === "/favicon.ico") return route.fulfill({status: 204});
          unexpected.push(path); return route.fulfill({status: 404, json: {error: "unaccounted fixture read"}});
        });
        await page.goto(baseURL + "/fixture/pi-transcript", {waitUntil: "domcontentloaded"});
        const input = page.locator('#message-form textarea[name="message"]'), send = page.locator("#message-send");
        const receipt = text => page.locator(".message-receipt").filter({has: page.locator("span", {hasText: text})});
        await expect(send).toBeEnabled(); await expect(send).toHaveText("Queue message");
        await input.fill("first Pi FIFO"); await send.click();
        await input.fill("second Pi FIFO"); await send.click();
        assert.equal(await input.inputValue(), "", JSON.stringify({errors, unexpected, notice: await page.locator("#message-submission-status").textContent()}));
        await expect(receipt("first Pi FIFO")).toContainText("Waiting to send");
        assert.equal(sends.length, 0);
        const storedOrder = await page.evaluate(() => {
          const key = Object.keys(sessionStorage).find(key => key.startsWith("workspace-portal.message-dispatch."));
          return JSON.parse(sessionStorage.getItem(key)).entries;
        });
        assert.equal(storedOrder.length, 2);
        assert.deepEqual(storedOrder[0].attachmentIds, [file.id]);
        assert.deepEqual(storedOrder[1].attachmentIds, []);
        status = "idle";
        await page.reload();
        await expect(receipt("first Pi FIFO")).toContainText("Waiting to send");
        await expect(send).toHaveText("Send");
        assert.equal(sends.length, 0, "restored FIFO remains paused at idle");
        const beforeSendRead = nextRead = hold(), firstSend = nextSend = hold();
        await refresh(); await expect.poll(() => beforeSendRead.started).toBe(true);
        await receipt("first Pi FIFO").getByRole("button", {name: "Resume sending", exact: true}).click();
        await expect.poll(() => sends.length).toBe(1);
        assert.equal(sends[0].clientUserMessageId, storedOrder[0].id);
        assert.deepEqual(sends[0].attachmentIds, [file.id]);
        beforeSendRead.release(); await expect.poll(() => beforeSendRead.finished).toBe(true);
        await expect(send).toHaveText("Queue message");
        const inFlightRead = nextRead = hold();
        await refresh(); await expect.poll(() => inFlightRead.started).toBe(true);
        firstSend.release(); await expect(receipt("first Pi FIFO")).toContainText("Sent");
        const settledReads = reads;
        inFlightRead.release();
        await expect.poll(() => reads).toBeGreaterThan(settledReads);
        await expect(receipt("second Pi FIFO")).toContainText("Waiting to send");
        await expect(send).toHaveText("Queue message");
        assert.equal(sends.length, 1, "settlement cannot dispatch second FIFO using first idle");
        status = "idle"; await refresh();
        await expect.poll(() => sends.length).toBe(2);
        assert.equal(sends[1].clientUserMessageId, storedOrder[1].id);
        status = "idle"; rejectMessage = true;
        // A synthetic Ready draft exercises the ordinary file capture, without live uploads.
        await page.evaluate(({draftKey, file}) => localStorage.setItem(draftKey, JSON.stringify([file])), {draftKey, file});
        await page.reload();
        await expect(send).toHaveText("Send");
        await input.fill("Pi terminal refusal"); await send.click();
        await expect(receipt("Pi terminal refusal")).toContainText("Pi did not accept this message");
        const terminalID = sends.at(-1).clientUserMessageId;
        const terminalKey = `workspace-portal.send-attempt.${slug}.pi.${encodeURIComponent(scope)}.pi-thread.${terminalID}`;
        const terminalRecord = await page.evaluate(key => JSON.parse(sessionStorage.getItem(key)), terminalKey);
        assert.deepEqual(sends.at(-1).attachmentIds, [file.id]);
        await expect(receipt("Pi terminal refusal").getByRole("button", {name: "Retry original", exact: true})).toHaveCount(0);
        await page.reload();
        await expect(receipt("Pi terminal refusal")).toContainText("Pi did not accept this message");
        assert.equal(sends.length, 3, "terminal reload has no automatic retry");
        await page.evaluate(({draftKey, file}) => localStorage.setItem(draftKey, JSON.stringify([file])), {draftKey, file});
        await page.reload(); await expect(send).toHaveText("Send");
        await input.fill("Pi terminal refusal"); await send.click();
        assert.equal(sends.length, 3, "ordinary matching draft cannot replace terminal identity");
        rejectMessage = false;
        await receipt("Pi terminal refusal").getByRole("button", {name: "Send as new message", exact: true}).click();
        await expect.poll(() => sends.length).toBe(4);
        assert.notEqual(sends[3].clientUserMessageId, terminalID); assert.equal(sends[3].message, "Pi terminal refusal");
        assert.deepEqual(sends[3].attachmentIds, [file.id]);
        status = "idle"; rejectMessage = "missing"; await refresh();
        await expect(send).toHaveText("Send");
        await input.fill("Pi missing refusal metadata"); await send.click();
        await expect(receipt("Pi missing refusal metadata")).toContainText("Not sent.");
        await expect(receipt("Pi missing refusal metadata").getByRole("button", {name: "Retry original", exact: true})).toBeVisible();
        await expect(receipt("Pi missing refusal metadata").getByRole("button", {name: "Send as new message", exact: true})).toHaveCount(0);
        const historicalID = sends.at(-1).clientUserMessageId;
        await page.evaluate(({slug, scope, historicalID}) => {
          const key = `workspace-portal.send-attempt.${slug}.pi.${encodeURIComponent(scope)}.pi-thread.${historicalID}`;
          const saved = JSON.parse(sessionStorage.getItem(key));
          sessionStorage.setItem(key, JSON.stringify({...saved, retryable: false}));
          // Retained receipts can predate the FIFO. Keep the original identity.
          const order = Object.keys(sessionStorage).find(key => key.startsWith("workspace-portal.message-dispatch."));
          sessionStorage.setItem(order, JSON.stringify({schema: 1, entries: []}));
        }, {slug, scope, historicalID});
        await page.reload(); await expect(send).toHaveText("Send");
        await expect(receipt("Pi missing refusal metadata").getByRole("button", {name: "Retry original", exact: true})).toBeEnabled();
        assert.equal(sends.length, 5, "historical unmarked false never automatically reconciles");
        rejectMessage = true;
        await receipt("Pi missing refusal metadata").getByRole("button", {name: "Retry original", exact: true}).click();
        await expect(receipt("Pi missing refusal metadata")).toContainText("Pi did not accept this message");
        assert.equal(sends.length, 6); assert.equal(sends[5].clientUserMessageId, historicalID);
        // Restore the explicit-false legacy scoped receipt, without a FIFO record.
        status = "idle";
        await page.evaluate(({terminalKey, terminalRecord}) => {
          sessionStorage.setItem(terminalKey, JSON.stringify({...terminalRecord, state: "unsent", retryable: false}));
        }, {terminalKey, terminalRecord});
        await page.reload();
        await expect(receipt("Pi terminal refusal").filter({has: page.getByRole("button", {name: "Send as new message", exact: true})})).toHaveCount(1);
        assert.equal(sends.length, 6, "marked reload never automatically retries");
        // A response from the old page cannot authorize a changed scope.
        const oldPageRead = nextRead = hold(); await refresh(); await expect.poll(() => oldPageRead.started).toBe(true);
        await page.evaluate(() => {document.body.dataset.conversationScope = "pi-workflow-v1:" + "b".repeat(64);});
        oldPageRead.release(); await expect(send).toHaveText("Queue message");
        assert.equal(sends.length, 6, "stale-scope idle cannot dispatch");
        assert.deepEqual(errors, []);
        assert.deepEqual(unexpected, []);
        continue;
      }
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
      await expect(receipt("Reworded successful message")).toContainText("Waiting to send");
      await expect(banner).toBeVisible();
      assert.equal(sends.length,1,"reworded intent waits behind the unknown head");
      await expect(receipt("Unconfirmed original")).toContainText("Outcome unknown.");

      await input.fill("Keep this newer draft");
      page.once("dialog", dialog=>{
        assert(dialog.message().includes("original message may still arrive"));
        void dialog.accept();
      });
      await receipt("Unconfirmed original").getByRole("button",{name:"Send another copy…",exact:true}).click();
      await expect.poll(()=>sends.length).toBe(4);
      assert.equal(sends[1].clientUserMessageId,originalID);
      assert.equal(sends[1].retry,true);
      assert.notEqual(sends[2].clientUserMessageId,originalID);
      assert.equal(sends[2].message,"Unconfirmed original");
      await expect(banner).toBeHidden();
      await expect(input).toHaveValue("Keep this newer draft");
      const replacementID=sends[2].clientUserMessageId;
      assert.equal(sends[3].message,"Reworded successful message");
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
