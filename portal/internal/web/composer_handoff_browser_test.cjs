"use strict";
const assert = require("node:assert/strict"), fs = require("node:fs"), path = require("node:path");
const {createHash} = require("node:crypto");
const {chromium, expect} = require("@playwright/test");
const baseURL = process.argv[2], capture = process.env.PORTAL_RETRY_SCREENSHOTS;
const digest = text => createHash("sha256").update(text).digest("hex");
(async () => {
  const browser = await chromium.launch({headless: true, channel: "chromium"});
  try {
    for (const member of [false, true]) {
      const context = await browser.newContext({ignoreHTTPSErrors: true,
        viewport: member ? {width: 390, height: 844} : {width: 1440, height: 1000}});
      const page = await context.newPage(), sends = [], errors = [], held = [], readHolds = [];
      const threadId = member ? "member-thread" : "thread-1", label = member ? "mobile" : "desktop";
      let entries = [], holdReads = false, holdAck = true, ackCount = 0, interruptCount = 0, queueCount = 0;
      let lost = false, phase = "startup";
      let threadReads = 0, threadResponses = 0, streamHints = 0, lastTranscript = null;
      const transcriptTimeline = [];
      const uploadTimeline = [];
      const traceUpload = (event, detail = {}) => {
        uploadTimeline.push({sequence: (uploadTimeline.at(-1)?.sequence || 0) + 1, event, phase, ...detail});
        if (uploadTimeline.length > 100) uploadTimeline.shift();
      };
      const trace = (event, detail = {}) => {
        transcriptTimeline.push({event, phase, ...detail});
        if (transcriptTimeline.length > 100) transcriptTimeline.shift();
      };
      page.on("pageerror", error => errors.push(error.message));
      page.on("request", request => {
        const pathname = new URL(request.url()).pathname;
        if (pathname.startsWith("/uploads/")) traceUpload("request", {path: pathname, method: request.method()});
      });
      page.on("response", response => {
        const pathname = new URL(response.url()).pathname;
        if (pathname.startsWith("/uploads/")) traceUpload("response", {path: pathname, status: response.status()});
      });
      page.on("requestfailed", request => {
        const pathname = new URL(request.url()).pathname;
        if (pathname.startsWith("/uploads/")) traceUpload("request-failed", {path: pathname, error: request.failure()?.errorText});
      });
      await page.addInitScript(() => {
        window.EventSource = class {
          constructor() {window.composerFixtureEvents = this; queueMicrotask(() => this.onopen?.());}
          addEventListener() {} close() {}
        };
        const set = Storage.prototype.setItem, get = Storage.prototype.getItem;
        Storage.prototype.setItem = function(key, value) {
          const fault = window.composerFault;
          if (fault === "write" && key.startsWith("workspace-portal.send-attempt.")) throw new Error("Fixture write failure");
          if (fault === "detach" && key.startsWith("workspace-portal.upload-draft.") && value === "[]") throw new Error("Fixture detach failure");
          set.call(this, key, value);
          if (fault === "readback" && key.startsWith("workspace-portal.send-attempt.")) window.composerFaultKey = key;
        };
        Storage.prototype.getItem = function(key) {
          if (window.composerFault === "readback" && key === window.composerFaultKey) return null;
          return get.call(this, key);
        };
      });
      const serve = async route => {
        const operation = new URL(route.request().url()).pathname.split("/").at(-1);
        const json = (data, status = 200) => route.fulfill({status, json: data});
        if (operation === "thread" || operation === "page") {
          const read = ++threadReads;
          trace("request", {read, held: holdReads});
          if (holdReads) await new Promise(resolve => readHolds.push(resolve));
          const payload = {threadId, latestTurnId: "turn-1", status: "active", collaborationMode: "default",
            model: "model-1", reasoningEffort: "medium", entries, hasOlder: false};
          lastTranscript = {read, response: ++threadResponses, identities: entries.filter(entry => entry.clientUserMessageId)
            .map(entry => ({id: entry.clientUserMessageId, digest: entry.clientUserMessageDigest}))};
          trace("fulfill", lastTranscript);
          return json(payload);
        }
        if (operation === "pending" || operation === "queue" && route.request().method() === "GET") return json([]);
        if (operation === "queue") {queueCount++; return json({queued: true});}
        if (operation === "message-ack") {
          ackCount++;
          return holdAck ? json({error: "Fixture acknowledgment unavailable"}, 503) : json({acknowledgedClientUserMessageIds:
            route.request().postDataJSON().acknowledgements.map(row => row.clientUserMessageId)});
        }
        if (operation === "message") {
          const body = route.request().postDataJSON(); sends.push({body, url: route.request().url()});
          await new Promise(resolve => held.push(resolve));
          if (lost) {lost = false; return route.abort("failed");}
          return json({turnId: "turn-1", clientUserMessageId: body.clientUserMessageId, steered: true}, 202);
        }
        if (operation === "interrupt") {interruptCount++; return json({ok: true});}
        if (operation === "reconcile") return json({ok: true});
        if (operation === "activity") return json({currentState: "working", coverageComplete: true});
        if (operation === "details") return json({repositoriesHTML: "", artifactsHTML: ""});
        return route.continue();
      };
      await page.route("**/api/sessions/example/**", serve);
      await page.route("**/codex/conversations/**", serve);
      const prompt = page.locator("#message-form textarea[name=message]"), send = page.locator("#message-send");
      const rows = page.locator("#message-receipts .message-receipt");
      const row = text => rows.filter({has: page.locator("span", {hasText: text})});
      const draftKey = `workspace-portal.upload-draft.example.${threadId}`;
      const stored = () => page.evaluate(thread => Object.keys(sessionStorage)
        .filter(key => key.startsWith(`workspace-portal.send-attempt.example.${thread}.`))
        .map(key => ({...JSON.parse(sessionStorage.getItem(key)), id: key.split(".").at(-1)})), threadId);
      const release = () => {assert(held.length, "message request reached barrier"); held.shift()();};
      const refresh = async (identity = null) => {
        const before = threadResponses;
        trace("stream-hint", {hint: ++streamHints});
        await page.evaluate(() => window.composerFixtureEvents.onmessage());
        await expect.poll(() => threadResponses).toBeGreaterThan(before);
        if (identity) await expect.poll(() => lastTranscript?.identities.some(value =>
          value.id === identity.id && value.digest === identity.digest)).toBe(true);
      };
      const load = async () => {
        phase = "load";
        await page.goto(baseURL + `/example/${member ? "?member=implementer0" : ""}#codex`);
        await expect.poll(() => page.evaluate(() => Boolean(window.composerFixtureEvents))).toBe(true);
        await expect(page.locator(".codex-upload-toggle")).toBeEnabled();
        await expect(send).toBeEnabled();
      };
      const attach = async name => {
        traceUpload("attach-wait", {name, control: await page.evaluate(() => {
          const toggle = document.querySelector("#message-upload-controls .codex-upload-toggle");
          return {mounted: Boolean(toggle), disabled: toggle?.disabled};
        })});
        // Direct picker injection bypasses the disabled attachment menu. Match
        // its real admission boundary, including after a document reload.
        await expect(page.locator("#message-upload-controls .codex-upload-toggle")).toBeEnabled();
        traceUpload("attach-ready", {name});
        await page.locator('#message-form input[type=file]').setInputFiles({name, mimeType: "text/plain", buffer: Buffer.from(name)});
        traceUpload("picker-selected", {name});
        await expect(page.locator("#message-uploads")).toContainText(name);
        await expect(page.locator("#message-uploads .codex-attachment-detail").last()).toContainText("Ready");
        return page.evaluate(key => JSON.parse(localStorage.getItem(key)).map(file => file.id), draftKey);
      };
      try {
        await load();
        for (const kind of ["text", "attachments", "both"]) {
          phase = kind;
          const text = kind === "attachments" ? "" : `Explicit ${kind} message`;
          const ids = kind === "text" ? [] : await attach(`${kind}.txt`);
          await prompt.fill(text); const before = sends.length;
          const acceptedBefore = await page.locator('#message-receipts [data-receipt-state="accepted"]').count();
          await send.click(); await expect.poll(() => sends.length).toBe(before + 1);
          await expect(page.locator('#message-receipts [data-receipt-state="accepted"]')).toHaveCount(acceptedBefore);
          await expect(prompt).toBeEmpty(); await expect(prompt).toBeEditable();
          await expect(rows.filter({hasText: "Sending…"})).toHaveCount(1);
          assert.deepEqual(sends.at(-1).body.attachmentIds || [], ids);
          assert.deepEqual((await stored()).find(value => value.id === sends.at(-1).body.clientUserMessageId).attachmentIds || [], ids);
          await expect(page.locator("#message-uploads .codex-attachment")).toHaveCount(0);
          await expect(send).toBeDisabled(); await expect(page.locator("#message-queue")).toBeDisabled();
          await expect(page.locator("#interrupt")).toBeEnabled();
          const nextText = text || "Next attachment draft";
          await prompt.fill(nextText);
          const nextIDs = await attach(`next-${kind}.txt`);
          await prompt.press("Enter"); await page.locator("#message-form").evaluate(form => {form.requestSubmit(); form.requestSubmit();});
          assert.equal(sends.length, before + 1); assert.equal(queueCount, 0);
          if (kind === "both" && capture) {
            fs.mkdirSync(capture, {recursive: true});
            await page.screenshot({path: path.join(capture, `sending-next-draft-${label}.png`), animations: "disabled"});
          }
          // Receipt settlement releases Send even when transcript reads cannot finish.
          holdReads = true;
          release(); await expect(rows.filter({hasText: "Sending…"})).toHaveCount(0);
          await expect(send).toBeEnabled(); await expect(prompt).toHaveValue(nextText);
          assert.deepEqual(await page.evaluate(key => JSON.parse(localStorage.getItem(key)).map(file => file.id), draftKey), nextIDs);
          holdReads = false; for (const resume of readHolds.splice(0)) resume();
          // Known equal text is fresh intent, while its accepted row is retained.
          const firstID = sends.at(-1).body.clientUserMessageId;
          await send.click(); await expect.poll(() => sends.length).toBe(before + 2);
          assert.notEqual(sends.at(-1).body.clientUserMessageId, firstID);
          assert.equal(sends.at(-1).body.retry, false);
          release(); await expect(send).toBeEnabled();
        }
        phase = "unknown-reload";
        await prompt.fill("Lost original"); await send.click();
        await expect.poll(() => held.length).toBe(1);
        const original = sends.at(-1).body;
        await prompt.fill("Unrelated next draft"); lost = true; release();
        await expect(row("Lost original")).toContainText("Outcome unknown.");
        await expect(prompt).toHaveValue("Unrelated next draft");
        await page.reload(); await expect(send).toBeEnabled();
        await prompt.fill("Lost original");
        await row("Lost original").getByRole("button", {name: "Retry original"}).click();
        await expect.poll(() => held.length).toBe(1);
        assert.equal(sends.at(-1).body.clientUserMessageId, original.clientUserMessageId);
        assert.equal(sends.at(-1).body.retry, true);
        await prompt.fill("Lost original"); // Equal content is a separately owned new draft.
        release(); await expect(send).toBeEnabled(); await expect(prompt).toHaveValue("Lost original");

        phase = "observed-before-reply";
        await send.click(); await expect.poll(() => held.length).toBe(1);
        const observed = sends.at(-1).body;
        assert.notEqual(observed.clientUserMessageId, original.clientUserMessageId, "same accepted text is fresh explicit intent");
        await prompt.fill(observed.message);
        entries = [{turnId: "turn-1", itemId: "observed", kind: "userMessage", text: observed.message,
          clientUserMessageId: observed.clientUserMessageId, clientUserMessageDigest: digest(observed.message)}];
        await refresh({id: observed.clientUserMessageId, digest: digest(observed.message)});
        await expect(page.locator("#transcript-entries")).toContainText(observed.message);
        await expect.poll(async () => (await stored()).some(value => value.id === observed.clientUserMessageId &&
          value.state === "observed" && value.transcriptDigest === digest(observed.message))).toBe(true);
        assert.equal(held.length, 1, "transcript proof arrived while the original reply remained held");
        assert.equal(ackCount, 0, "an in-flight original cannot be acknowledged yet");
        lost = true; release(); await expect(send).toBeEnabled();
        await expect(prompt).toHaveValue(observed.message);
        await expect.poll(() => ackCount).toBeGreaterThan(0);
        holdAck = false; await refresh();
        await expect.poll(async () => (await stored()).some(value => value.id === observed.clientUserMessageId)).toBe(false);
        await expect(prompt).toHaveValue(observed.message);

        for (const fault of ["write", "readback", "detach"]) {
          phase = fault;
          await attach(`preserve-${fault}.txt`); await prompt.fill(`Preserve ${fault}`);
          const before = sends.length;
          await page.evaluate(value => {window.composerFault = value;}, fault);
          await send.click();
          await expect(page.locator("#message-submission-status")).toBeVisible();
          await expect(prompt).toHaveValue(`Preserve ${fault}`);
          await expect(page.locator("#message-uploads")).toContainText(`preserve-${fault}.txt`);
          assert.equal(sends.length, before);
          // Reset only this disposable fixture's selection between fault cases;
          // retain every original attempt and its immutable attachment IDs.
          await page.evaluate(key => {window.composerFault = null; localStorage.setItem(key, "[]");}, draftKey);
          await page.reload(); await expect(send).toBeEnabled();
          await expect(page.locator("#message-uploads .codex-attachment")).toHaveCount(0);
        }
        phase = "interrupt-while-sending";
        await prompt.fill("Interrupt boundary"); await send.click(); await expect.poll(() => held.length).toBe(1);
        await page.locator("#interrupt").click(); await expect.poll(() => interruptCount).toBe(1);
        await prompt.fill("Keep next selection"); release(); await expect(send).toBeEnabled();
        await expect(prompt).toHaveValue("Keep next selection");
        assert.deepEqual(errors, []);
      } catch (error) {
        const uploadState = await page.evaluate(() => ({
          hidden: document.getElementById("message-uploads")?.hidden,
          toggleDisabled: document.querySelector("#message-upload-controls .codex-upload-toggle")?.disabled,
          noticeHidden: document.querySelector("#message-uploads .codex-upload-notice")?.hidden,
          notice: document.querySelector("#message-uploads .codex-upload-notice")?.textContent?.slice(0, 1024),
          retryHidden: document.querySelector("#message-uploads button")?.hidden,
        })).catch(() => null);
        console.error(JSON.stringify({fixture: "composer-handoff", member, phase, pageErrors: errors,
          messageRequests: sends.length, held: held.length, ackCount, interruptCount, queueCount,
          threadReads, threadResponses, streamHints, holdReads, readHolds: readHolds.length, lastTranscript, transcriptTimeline,
          uploadState, uploadTimeline}));
        throw error;
      } finally {
        for (const resume of [...held.splice(0), ...readHolds.splice(0)]) resume();
        await context.close();
      }
    }
    console.log("Real template composer: durable handoff, editable next draft, receipts, reload, storage failures and one in-flight dispatch passed.");
  } finally {await browser.close();}
})().catch(error => {console.error(error); process.exitCode = 1;});
