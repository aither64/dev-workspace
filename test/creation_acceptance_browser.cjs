// Real packaged browser/native acceptance. Only the external model is synthetic.
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const {randomUUID} = require("node:crypto");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE);
const [action, name, optionsJSON = "{}"] = process.argv.slice(2);
const options = JSON.parse(optionsJSON);
const origin = "https://fixture.workspace.example.test";
const filename = `/tmp/creation-${name}.json`;
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));

(async () => {
  const browser = await chromium.launch({executablePath: process.env.CHROMIUM_EXECUTABLE,
    headless: true, args: ["--no-sandbox", "--disable-dev-shm-usage"]});
  const context = await browser.newContext({ignoreHTTPSErrors: true,
    permissions: ["clipboard-read", "clipboard-write"],
    httpCredentials: {username: "developer", password: fs.readFileSync(
      "/var/lib/dev-workspaces/password/password", "utf8").trim()}});
  const page = await context.newPage(), errors = [], requests = [];
  page.on("pageerror", error => errors.push(error.message));
  page.on("response", response => requests.push({path: new URL(response.url()).pathname,
    method: response.request().method(), status: response.status()}));
  let record, phase = action;
  const get = async path => {
    const response = await context.request.get(origin + path);
    assert(response.ok(), `${path}: ${response.status()} ${await response.text()}`);
    return response.json();
  };
  const statusPath = () => record.requestId ? `/api/session-creations/${record.requestId}` :
    `/api/sessions/${record.slug}/creation`;
  const save = () => fs.writeFileSync(filename, JSON.stringify(record, null, 2));
  async function openConversation(destination, slug) {
    const [hydrated] = await Promise.all([
      page.waitForResponse(response => response.request().method() === "GET" &&
        [`/api/sessions/${slug}/thread`, `/api/sessions/${slug}/thread/page`]
          .includes(new URL(response.url()).pathname)),
      page.goto(origin + destination),
    ]);
    assert(hydrated.ok());
  }
  async function terminal(wanted = "ready") {
    phase = "wait for " + wanted;
    const deadline = Date.now() + 180000;
    while (Date.now() < deadline) {
      const status = await get(statusPath());
      assert.equal(status.receiptId, record.receiptId);
      if (["ready", "failed", "paused", "conflict", "gone", "cancelled"].includes(status.state)) {
        assert([wanted].flat().includes(status.state), JSON.stringify(status));
        Object.assign(record, status); save(); return status;
      }
      await pause(250);
    }
    throw Error("creation did not reach a terminal result: " + JSON.stringify(record));
  }
  async function settings(form) {
    await form.locator("[name=provider]:not([disabled])").waitFor();
    await form.locator("[name=provider]").selectOption(options.provider || "");
    if (await form.locator("[name=team]").count()) {
      await form.locator("[name=team]").selectOption(options.team || "solo");
    }
    await form.locator("[name=model]:not([disabled])").waitFor();
    await form.locator("[name=model]").selectOption("gpt-5.5");
    await form.locator("[name=effort]").selectOption("high");
    if (options.sensitive) await form.locator("[name=sensitive]").check();
    await form.locator("[name=browserNotificationsEnabled]").setChecked(options.notifications !== false);
  }
  async function followup(marker) {
    phase = "follow-up exchange";
    const before = await get(`/api/sessions/${record.slug}/thread`);
    assert(!before.entries.some(entry => entry.kind === "userMessage" && entry.text === marker),
      "follow-up marker already exists before send");
    await page.locator("#message-form textarea[name=message]").fill(marker);
    await page.locator("#message-send:not([disabled])").waitFor();
    await page.locator("#message-send").click();
    const deadline = Date.now() + 45000;
    while (Date.now() < deadline) {
      const thread = await get(`/api/sessions/${record.slug}/thread`);
      const entries = thread.entries || [];
      const users = entries.filter(entry => entry.kind === "userMessage" && entry.text === marker);
      assert(users.length <= 1, "follow-up user turn was duplicated");
      if (users.length === 1 && entries.some(entry => entry.kind === "agentMessage" &&
          entry.turnId === users[0].turnId && entry.turnStatus === "completed" &&
          entry.text === "CREATION_REPLY_" + (record.options.provider || "primary"))) return thread;
      await pause(200);
    }
    throw Error("follow-up exchange did not finish");
  }
  async function usable() {
    await terminal();
    phase = "canonical redirect and transcript";
    const destination = record.requestId ? record.canonicalUrl : record.url;
    assert.equal(destination, `/${record.slug}/`);
    record.destination = destination;
    // The shipped status page may already be redirecting after ready. Do not
    // race that navigation by opening the status page again.
    if (![origin + record.url, origin + destination].includes(page.url())) {
      await page.goto(origin + record.url, {waitUntil: "commit"});
    }
    await page.waitForURL(origin + destination, {timeout: 30000});
    // Slug-based fork/plan status pages already have the final URL. Wait for
    // their shipped ready replacement to render the conversation document.
    await page.locator("body[data-thread-id]:not([data-thread-id=''])").waitFor();
    record.threadId = await page.locator("body").getAttribute("data-thread-id");
    assert(record.threadId);
    await page.locator("#transcript").getByText("CREATION_REPLY_", {exact: false}).first().waitFor();
    record.followupMarker = `CREATION_FOLLOWUP_${record.slug}_${randomUUID()}`;
    save();
    const thread = await followup(record.followupMarker);
    const initial = thread.entries.filter(entry => entry.kind === "userMessage" &&
      entry.text?.includes(record.marker));
    if (record.kind !== "fork") assert.equal(initial.length, 1, JSON.stringify(thread));
    assert.equal(thread.model, "gpt-5.5");
    assert.equal(thread.reasoningEffort, "high");
    const attention = await get(`/api/sessions/${record.slug}/attention`);
    assert.equal(attention.enabled, record.options.notifications !== false);
    record.transcript = thread; save();
    // Ready proof survives a full portal read and browser reload.
    await page.reload();
    assert.equal(await page.locator("body").getAttribute("data-thread-id"), record.threadId);
    await terminal();
  }
  try {
    if (["create", "admit"].includes(action)) {
      await page.goto(origin);
      const form = page.locator("#new-session-form");
      await form.locator("details summary").click();
      await settings(form);
      const marker = "CREATION_INITIAL_" + name;
      await form.locator("[name=goal]").fill(marker);
      if (!options.auto) await form.locator("[name=name]").fill(name);
      if (options.attachment) {
        await form.locator("#creation-upload-controls button[aria-label='Add attachments']:not([disabled])").waitFor();
        const rejectedUpload = page.waitForResponse(r => r.status() >= 400 &&
          new URL(r.url()).pathname.startsWith("/uploads/")).then(async response => {
          throw Error(`upload rejected (${response.status()}): ${await response.text()}`);
        });
        await form.locator("input[type=file]").setInputFiles({name: "creation-evidence.txt",
          mimeType: "text/plain", buffer: Buffer.from("CREATION_ATTACHMENT_CONTENT")});
        await Promise.race([rejectedUpload,
          form.locator("#creation-uploads").getByText("Ready", {exact: false}).first().waitFor()]);
      }
      // Exercise the shipped draft serializer before actual admission.
      phase = "draft restoration after reload";
      await page.reload();
      await form.locator("[name=provider]:not([disabled])").waitFor();
      assert.equal(await form.locator("[name=goal]").inputValue(), marker);
      assert.equal(await form.locator("[name=provider]").inputValue(), options.provider || "");
      assert.equal(await form.locator("[name=sensitive]").isChecked(), Boolean(options.sensitive));
      assert.equal(await form.locator("[name=browserNotificationsEnabled]").isChecked(), options.notifications !== false);
      phase = "admit creation";
      const [accepted] = await Promise.all([
        page.waitForResponse(r => new URL(r.url()).pathname === "/sessions" && r.request().method() === "POST"),
        form.locator("button[type=submit]:not([disabled])").click(),
      ]);
      assert.equal(accepted.status(), 202);
      // Chromium can retire the previous document's POST body on navigation.
      // Read the real durable receipt by the actual serialized request ID.
      const requestId = new URLSearchParams(accepted.request().postData()).get("clientRequestId");
      record = {...await get(`/api/session-creations/${requestId}`), marker, options, kind: "new"}; save();
      assert.equal(record.requestId, requestId);
      assert(record.requestId && record.receiptId);
      await page.waitForURL(origin + `/creations/${record.requestId}/`);
      if (action === "create") await usable();
    } else {
      record = JSON.parse(fs.readFileSync(filename));
      if (action === "change-effort") {
        await openConversation(record.canonicalUrl || record.url, record.slug);
        await page.locator("#codex-settings-open:not([disabled])").click();
        await page.locator("#codex-effort:not([disabled])").waitFor();
        await page.locator("#codex-effort").selectOption("xhigh");
        const [saved] = await Promise.all([
          page.waitForResponse(response => response.request().method() === "POST" &&
            new URL(response.url()).pathname === `/api/sessions/${record.slug}/settings`),
          page.locator("#codex-settings-save:not([disabled])").click(),
        ]);
        assert(saved.ok(), `save effort: ${saved.status()} ${await saved.text()}`);
        await page.locator("#codex-settings-dialog").waitFor({state: "hidden"});
        const deadline = Date.now() + 30000;
        while ((await get(`/api/sessions/${record.slug}/thread`)).reasoningEffort !== "xhigh") {
          assert(Date.now() < deadline, "reasoning change did not become effective");
          await pause(100);
        }
      } else if (action === "failed") await terminal("failed");
      else if (action === "finish") await usable();
      else if (action === "retry") {
        await terminal(["failed", "paused"]);
        const previous = {...record};
        await page.goto(origin + record.url);
        const error = page.locator("#creation-error");
        if (options.error) {
        await error.waitFor({state: "visible"});
        const displayed = await error.textContent();
        assert(displayed);
        await error.evaluate(element => {
          const range = document.createRange(); range.selectNodeContents(element);
          const selection = getSelection(); selection.removeAllRanges(); selection.addRange(range);
        });
        await pause(1600);
        assert.equal(await page.evaluate(() => getSelection().toString()), displayed);
        await page.getByRole("button", {name: "Copy error", exact: true}).click();
        assert.equal(await page.evaluate(() => navigator.clipboard.readText()), displayed);
        }
        const [retried] = await Promise.all([
          page.waitForResponse(r => r.request().method() === "POST" &&
            new URL(r.url()).pathname === statusPath() + "/retry"),
          page.locator("#creation-retry").click(),
        ]);
        assert.equal(retried.status(), 202);
        await usable();
        assert.equal(record.receiptId, previous.receiptId);
        if (previous.slug) assert.equal(record.slug, previous.slug);
        // Completion compacts the preparation mapping back to its own attempt.
        // Read the initializer's durable receipt for a handed-off retry.
        const counter = previous.slug ? "attempt" : "preparationAttempt";
        const completed = previous.slug ? await get(`/api/sessions/${record.slug}/creation`) : record;
        assert.equal(completed.receiptId, previous.receiptId);
        assert(completed[counter] > previous[counter]);
      } else if (["fork", "plan"].includes(action)) {
        const source = {...record};
        await openConversation(source.destination, source.slug);
        if (action === "plan") {
          await page.locator("#codex-mode").getByRole("button", {name: "Plan", exact: true}).click();
          await page.locator("#message-form textarea").fill("CREATION_PLAN_REQUEST");
          await page.locator("#message-send").click();
          await page.locator("#plan-implement-new").waitFor({state: "visible", timeout: 45000});
          await page.locator("#plan-implement-new").click();
        } else await page.locator("#fork-open:not([disabled])").click();
        const form = page.locator(action === "fork" ? "#fork-form" : "#plan-session-form");
        await settings(form);
        await form.locator("[name=name]").fill(name + "-" + action);
        const [accepted] = await Promise.all([
          page.waitForResponse(r => r.request().method() === "POST" &&
            new URL(r.url()).pathname === `/api/sessions/${source.slug}/${action === "fork" ? "fork" : "implement-plan"}`),
          form.locator("button[type=submit]").click(),
        ]);
        assert.equal(accepted.status(), 202);
        const submitted = JSON.parse(accepted.request().postData());
        const slug = `${submitted.creationDate}-${submitted.name}`;
        record = {...await get(`/api/sessions/${slug}/creation`), marker: action === "plan" ? "CREATION_PLAN_DONE" : source.marker,
          kind: action, options, source: {slug: source.slug, threadId: source.threadId}};
        await usable();
        assert.notEqual(record.threadId, source.threadId);
        const response = await context.request.get(origin + source.destination);
        assert(response.ok());
        fs.writeFileSync(`/tmp/creation-${name}-${action}.json`, JSON.stringify(record, null, 2));
        // Keep the source checkpoint for independent fork and plan runs.
        fs.writeFileSync(filename, JSON.stringify(source, null, 2));
      } else if (action === "delete-stale") {
        await openConversation(record.destination, record.slug);
        await page.locator("#delete-session-open").click();
        await page.locator("#delete-session-form button[type=submit]").click();
        const deadline = Date.now() + 60000;
        let removed = false;
        while (Date.now() < deadline) {
          const response = await context.request.get(origin + statusPath());
          const status = await response.json();
          if (["gone", "conflict"].includes(status.state)) { removed = true; break; }
          const operation = await get(`/api/sessions/${record.slug}/operation`);
          assert(!["failed", "paused"].includes(operation.state), JSON.stringify(operation));
          await pause(250);
        }
        assert(removed, "fixture deletion did not finish");
        const stale = await context.request.post(origin + statusPath() + "/retry", {
          headers: {Origin: origin}, data: {receiptId: record.receiptId, attempt: record.attempt}});
        assert.equal(stale.status(), 409, await stale.text());
        assert.equal((await context.request.get(origin + record.destination)).status(), 404);
      } else throw Error("unknown action " + action);
    }
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({action, name, requestId: record.requestId, receiptId: record.receiptId,
      slug: record.slug, threadId: record.threadId, state: record.state}));
  } catch (error) {
    console.error(JSON.stringify({phase, record, errors, requests: requests.slice(-30),
      body: await page.locator("body").innerText().catch(() => "unavailable")}));
    throw error;
  } finally { await browser.close(); }
})().catch(error => {console.error(error); process.exitCode = 1;});
