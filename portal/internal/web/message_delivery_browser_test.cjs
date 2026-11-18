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
        let model = "model-1", effort = "high", settingsFailure = false, modelReads = 0;
        const settingsWrites = [];
        let status = "idle", nextRead = null, nextSend = null, rejectMessage = false, failReads = false, reads = 0, inputDelivery = false;
        const nativeInputs = new Map(), enquiries = [], consumed = new Set();
        const hold = () => {let release; const promise = new Promise(resolve => {release = resolve;}); return {promise, release, started: false, finished: false};};
        page.on("pageerror", error => errors.push(error.message));
        await page.addInitScript(({draftKey, file}) => {
          if (!sessionStorage.getItem("pi-fixture-seeded")) {localStorage.setItem(draftKey, JSON.stringify([file])); sessionStorage.setItem("pi-fixture-seeded", "1");}
          window.fixtureEvents = [];
          window.fixtureStreamUnavailable = !sessionStorage.getItem("pi-fixture-stream-tested");
          window.EventSource = class {constructor() {window.fixtureEvents.push(this); queueMicrotask(() => {if (!window.fixtureStreamUnavailable) this.onopen?.(); else if (window.fixtureEvents.length === 1) this.onerror?.();});} addEventListener() {} close() {}};
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
            if (failReads) return route.fulfill({status: 503, json: {error: "Fixture transcript unavailable"}});
            const wait = nextRead; nextRead = null;
            if (wait) {wait.started = true; await wait.promise;}
            await fulfill({threadId: "pi-thread", status: observed, inputDelivery, latestTurnId: "turn-1", model, reasoningEffort: effort, entries: [], hasOlder: false}).catch(() => {});
            if (wait) wait.finished = true;
            return;
          }
          if (operation === "settings") {
            const body = request.postDataJSON(); settingsWrites.push(body);
            assert.deepEqual(Object.keys(body).sort(), ["model", "reasoningEffort"]);
            if (settingsFailure !== "reject") { model = body.model; effort = body.reasoningEffort; }
            if (settingsFailure) return route.fulfill({status: 503, json: {error: "Fixture settings response unavailable"}});
            return fulfill({model, reasoningEffort: effort});
          }
          if (operation === "models") {
            modelReads++;
            return fulfill([{id: "model-1", model: "model-1", displayName: "Actual model", supportedReasoningEfforts: [{reasoningEffort: "high"}], defaultReasoningEffort: "high"},
              {id: "model-2", model: "model-2", displayName: "Other model", supportedReasoningEfforts: [{reasoningEffort: "low"}], defaultReasoningEffort: "low"}]);
          }
          if (operation === "message") {
            const body = request.postDataJSON(); sends.push(body);
            if (body.delivery) {
              assert.ok(["steer", "followUp"].includes(body.delivery));
              const id = body.clientUserMessageId;
              if (body.retry) {
                enquiries.push(body);
                assert.ok(nativeInputs.has(id), "enquiry must refer to the original native input");
                const original = nativeInputs.get(id);
                assert.equal(body.delivery, original.delivery); assert.equal(body.message, original.message);
                assert.deepEqual(body.attachmentIds, original.attachmentIds);
              } else {assert.equal(nativeInputs.has(id), false, "one native enqueue per original ID"); nativeInputs.set(id, body);}
              if (consumed.has(id)) return route.fulfill({status: 202, json: {clientUserMessageId: id, turnId: "native-" + id, steered: body.delivery === "steer"}});
              return route.fulfill({status: 409, json: {error: "Message is queued for native delivery.", code: "message_pending", pending: true, clientUserMessageId: id, delivery: body.delivery}});
            }
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
        const notice = page.locator("#message-input-status");
        await expect(send).toBeEnabled(); await expect(send).toHaveText("Send");
        // The real sync publishes reconnecting AFTER applying these idle reads.
        await expect.poll(() => reads).toBeGreaterThan(1);
        await expect(send).toHaveText("Send"); await expect(notice).toBeHidden();
        await expect(page.locator(".message-receipt")).toHaveCount(0);
        const summary = page.locator("#codex-settings-summary"), settings = page.locator("#codex-settings-dialog");
        const openSettings = () => page.getByRole("button", {name: "Edit Pi settings", exact: true}).click();
        const settingsSave = page.locator("#codex-settings-save");
        await expect(summary).toHaveText("model-1 · high");
        await openSettings(); await expect(settings).toBeVisible();
        await expect.poll(() => modelReads).toBe(1);
        await expect(page.locator("#codex-model")).toHaveValue("model-1");
        await expect(page.locator("#codex-effort")).toHaveValue("high");
        await expect(settingsSave).toBeDisabled();
        await page.locator("#codex-model").selectOption("model-2");
        await expect(page.locator("#codex-effort")).toHaveValue("low");
        assert.equal(settingsWrites.length, 0, "opening and editing a draft never configures");
        await page.locator("#codex-settings-close").click();
        await expect(summary).toHaveText("model-1 · high");
        await openSettings(); await expect.poll(() => modelReads).toBe(2);
        await expect(page.locator("#codex-model")).toHaveValue("model-1");
        await page.locator("#codex-model").selectOption("model-2");
        await expect(settingsSave).toBeEnabled(); await settingsSave.click();
        await expect(settings).not.toBeVisible(); await expect(summary).toHaveText("model-2 · low");
        assert.deepEqual(settingsWrites, [{model: "model-2", reasoningEffort: "low"}]);
        await page.reload(); await expect(send).toBeEnabled(); await expect(summary).toHaveText("model-2 · low");
        assert.equal(settingsWrites.length, 1, "reload only observes actual settings");
        settingsFailure = "reject";
        await openSettings(); await page.locator("#codex-model").selectOption("model-1");
        await expect(settingsSave).toBeEnabled(); await settingsSave.click();
        await expect(page.locator("#codex-settings-status")).toContainText("could not be confirmed");
        await expect(settings).toBeVisible(); await expect(summary).toHaveText("model-2 · low");
        await expect(page.locator("#codex-model")).toHaveValue("model-1");
        await page.locator("#codex-settings-close").click();
        settingsFailure = "lost";
        await openSettings(); await page.locator("#codex-model").selectOption("model-1");
        await expect(settingsSave).toBeEnabled(); await settingsSave.click();
        await expect(settings).not.toBeVisible(); await expect(summary).toHaveText("model-1 · high");
        assert.equal(settingsWrites.length, 3, "lost success uses a read, no Configure retry");
        settingsFailure = false; status = "active"; await refresh();
        await openSettings(); await expect(settingsSave).toBeDisabled();
        await expect(summary).toHaveText("model-1 · high"); await page.locator("#codex-settings-close").click();
        status = "idle"; await refresh(); await expect(send).toBeEnabled();
        // A real picker change rerenders the existing ready file via onChange.
        await page.locator('#message-upload-controls input[type="file"]').setInputFiles([]);
        await expect(send).toHaveText("Send"); await expect(notice).toBeHidden();
        status = "active";
        await page.evaluate(() => {window.fixtureStreamUnavailable = false; sessionStorage.setItem("pi-fixture-stream-tested", "1"); window.dispatchEvent(new Event("online"));});
        await expect(send).toBeEnabled(); await expect(send).toHaveText("Send");
        await expect(page.locator("#message-queue")).toBeHidden();
        await expect.poll(() => reads).toBeGreaterThan(2);
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
        const resumeFirst = receipt("first Pi FIFO").getByRole("button", {name: "Resume sending", exact: true});
        failReads = true; await refresh(); await expect(resumeFirst).toBeDisabled(); await expect(notice).toBeHidden();
        failReads = false; await refresh(); await expect(resumeFirst).toBeEnabled();
        const beforeSendRead = nextRead = hold(), firstSend = nextSend = hold();
        await refresh(); await expect.poll(() => beforeSendRead.started).toBe(true);
        await receipt("first Pi FIFO").getByRole("button", {name: "Resume sending", exact: true}).click();
        await expect.poll(() => sends.length).toBe(1);
        assert.equal(sends[0].clientUserMessageId, storedOrder[0].id);
        assert.deepEqual(sends[0].attachmentIds, [file.id]);
        beforeSendRead.release(); await expect.poll(() => beforeSendRead.finished).toBe(true);
        await expect(send).toHaveText("Send");
        const inFlightRead = nextRead = hold();
        await refresh(); await expect.poll(() => inFlightRead.started).toBe(true);
        firstSend.release(); await expect(receipt("first Pi FIFO")).toContainText("Sent");
        const settledReads = reads;
        inFlightRead.release();
        await expect.poll(() => reads).toBeGreaterThan(settledReads);
        await expect(receipt("second Pi FIFO")).toContainText("Waiting to send");
        await expect(send).toHaveText("Send");
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
        // Native Pending releases only the HTTP barrier; a steer passes followUp.
        inputDelivery = true; status = "active"; rejectMessage = false;
        await page.evaluate(() => {sessionStorage.clear(); localStorage.clear();});
        await page.reload();
        await expect(send).toHaveText("Steer now");
        const queue = page.locator("#message-queue"); await expect(queue).toHaveText("Queue next"); await expect(queue).toBeVisible();
        await input.fill("Identical native input"); await queue.click();
        await expect.poll(() => nativeInputs.size).toBe(1);
        const followUp = [...nativeInputs.values()][0];
        assert.equal(followUp.delivery, "followUp"); assert.deepEqual(followUp.attachmentIds, [file.id]);
        const nativeCard = receipt("Identical native input");
        await openSettings(); await expect(settingsSave).toBeDisabled(); await page.locator("#codex-settings-close").click();
        await expect(nativeCard).toContainText("Queued for Pi"); await expect(nativeCard).not.toContainText("Sent");
        await expect(nativeCard.getByRole("button", {name: "Drop message", exact: true})).toHaveCount(0);
        await expect(nativeCard.locator("textarea")).toHaveCount(0);
        await expect(notice).toBeHidden();
        await page.evaluate(({draftKey, file}) => localStorage.setItem(draftKey, JSON.stringify([file])), {draftKey, file});
        await page.reload(); await expect(send).toHaveText("Steer now");
        assert.equal(nativeInputs.size, 1, "reload never enqueues a saved Pending again");
        await input.fill("Identical native input"); await send.click();
        await expect.poll(() => nativeInputs.size).toBe(2);
        const steer = [...nativeInputs.values()][1];
        assert.equal(steer.delivery, "steer"); assert.notEqual(steer.clientUserMessageId, followUp.clientUserMessageId);
        assert.deepEqual(steer.attachmentIds, [file.id]);
        // Both queued cards retain independent identities; consume steer first.
        const cards = page.locator('.message-receipt[data-receipt-state="pending"]');
        await expect(cards).toHaveCount(2);
        await cards.nth(1).getByRole("button", {name: "Check delivery", exact: true}).click();
        assert.equal(nativeInputs.size, 2, "explicit enquiry never enqueues another input");
        consumed.add(steer.clientUserMessageId); await refresh();
        await expect(page.locator('.message-receipt[data-receipt-state="accepted"]')).toHaveCount(1, {timeout: 10000});
        await expect(page.locator('.message-receipt[data-receipt-state="pending"]')).toHaveCount(1);
        assert.ok(enquiries.some(body => body.clientUserMessageId === steer.clientUserMessageId));
        consumed.add(followUp.clientUserMessageId); await refresh();
        await expect(page.locator('.message-receipt[data-receipt-state="accepted"]')).toHaveCount(2, {timeout: 10000});
        await page.reload(); await expect(send).toHaveText("Steer now");
        assert.equal(nativeInputs.size, 2); await expect(notice).toBeHidden();
        // A response from the old page cannot authorize a changed scope.
        const oldPageRead = nextRead = hold(); await refresh(); await expect.poll(() => oldPageRead.started).toBe(true);
        await page.evaluate(() => {document.body.dataset.conversationScope = "pi-workflow-v1:" + "b".repeat(64);});
        oldPageRead.release(); await expect(notice).toBeHidden();
        assert.equal(nativeInputs.size, 2, "stale-scope idle cannot dispatch");
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
