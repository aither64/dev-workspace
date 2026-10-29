"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
const [baseURL, scenario, engineName] = process.argv.slice(2);
assert(["accepted", "unknown"].includes(scenario), "expected an isolated accepted or unknown fixture");
assert(["chromium", "firefox"].includes(engineName), "expected a browser engine");
const originalID = "00000000-0000-4000-8000-000000000091";
const originalText = "say hi";
const messagePath = "/api/sessions/example/message";
const attemptKey = `workspace-portal.send-attempt.example.thread-1.${originalID}`;
const dispatchKey = "workspace-portal.message-dispatch.example.thread-1";

// Go supplies an older active/open TLS fixture and checks the real native
// protocol effects after this script. No browser route replaces HTTP admission.
(async () => {
  const engine = {chromium, firefox}[engineName];
  const browser = await engine.launch({headless: true, ...(engineName === "chromium" ? {channel: "chromium"} : {})});
  try {
    const page = await browser.newPage({ignoreHTTPSErrors: true});
    const errors = [], executionRequests = [];
    let activityReads = 0;
    page.on("pageerror", error => errors.push(error.message));
    page.on("request", request => {
      const path = new URL(request.url()).pathname;
      if (request.method() === "GET" && path === "/api/sessions/example/activity") activityReads++;
      if (request.method() === "POST" && [messagePath, "/api/sessions/example/continue",
        "/api/sessions/example/queue", "/api/sessions/example/queue/start"].includes(path)) {
        executionRequests.push({path, body: request.postDataJSON()});
      }
    });
    const send = page.locator("#message-send");
    const receipt = page.locator("#message-receipts .message-receipt").filter({hasText: originalText});
    const savedAttempt = () => page.evaluate(key => JSON.parse(sessionStorage.getItem(key)), attemptKey);
    const savedDispatch = () => page.evaluate(key => JSON.parse(sessionStorage.getItem(key)), dispatchKey);
    const assertRequests = count => {
      assert.equal(executionRequests.length, count, "only explicit original message requests may execute");
      assert(executionRequests.every(request => request.path === messagePath), "no Continue or queue action");
    };
    await page.goto(`${baseURL}/example/#codex`, {waitUntil: "domcontentloaded"});
    await expect(page.locator("body")).toHaveAttribute("data-lifecycle", "active");
    await expect(send).toBeEnabled();
    await expect.poll(() => activityReads).toBeGreaterThan(0);
    await expect(page.locator("#codex-duration")).not.toHaveText("Timing unavailable");
    assertRequests(0); // Idle timing must not activate execution.
    const attachmentIds = [];
    if (scenario === "accepted") {
      await expect(page.locator("#message-upload-controls .codex-upload-toggle")).toBeEnabled();
      const uploadResponse = page.waitForResponse(response => response.request().method() === "POST" &&
        /^\/uploads\/s-example\/[^/]+$/.test(new URL(response.url()).pathname));
      await page.locator('#message-form input[type="file"]').setInputFiles({
        name: "recovery-original.txt", mimeType: "text/plain", buffer: Buffer.from("Saved original attachment"),
      });
      const upload = await uploadResponse;
      assert.equal(upload.status(), 200, await upload.text());
      attachmentIds.push((await upload.json()).id);
      assert.equal(typeof attachmentIds[0], "string");
      assert(attachmentIds[0], "a real completed upload must supply the saved attachment ID");
      await expect(page.locator("#message-uploads .codex-attachment-detail").last()).toContainText("Ready");
      const plan = page.locator('#codex-mode button[data-codex-mode="plan"]');
      await expect(plan).toBeVisible();
      const settingsResponse = page.waitForResponse(response => response.request().method() === "POST" &&
        new URL(response.url()).pathname === "/api/sessions/example/settings");
      await plan.click();
      const settings = await settingsResponse;
      assert.equal(settings.status(), 200, await settings.text());
      assert.equal(settings.request().postDataJSON().collaborationMode, "plan");
      await expect(plan).toHaveAttribute("aria-pressed", "true");
      await expect(send).toBeEnabled();
    }
    assertRequests(0);
    const attempt = {id: originalID, message: originalText, action: "send", attachmentIds,
      context: "", state: "unknown"};
    await page.evaluate(({attempt, attemptKey, dispatchKey}) => {
      sessionStorage.setItem(attemptKey, JSON.stringify({message: attempt.message, context: "",
        ...(attempt.attachmentIds.length ? {attachmentIds: attempt.attachmentIds} : {})}));
      sessionStorage.setItem(dispatchKey, JSON.stringify({schema: 1, entries: [attempt]}));
    }, {attempt, attemptKey, dispatchKey});
    // The Go fixture creates a real exit75 failure, then runs the production
    // coordinator step on the second read. These GETs replace no HTTP response.
    const recoveryRead = async () => {
      const response = await page.request.get(`${baseURL}/api/sessions/example/recovery`);
      assert.equal(response.status(), 200);
      return response.json();
    };
    const failed = await recoveryRead();
    assert.equal(failed.state, "failed");
    assert.match(failed.error, /another dev-session command is changing example/);
    const readsBeforeFailureReload = activityReads;
    await page.reload({waitUntil: "domcontentloaded"});
    await expect(receipt).toContainText("Outcome unknown.");
    await expect(send).toBeDisabled();
    await expect(page.locator("#session-recovery")).toContainText(failed.error.trim());
    await expect(page.locator("#codex-duration")).toHaveText("Timing unavailable");
    assert.equal(activityReads, readsBeforeFailureReload, "unavailable recovery fetched activity");
    const retry = receipt.getByRole("button", {name: "Retry original", exact: true});
    await expect(retry).toBeDisabled();
    assertRequests(0);
    const recovered = await recoveryRead();
    assert.equal(recovered.state, "idle");
    assert.equal(recovered.executionHeld, true);
    // Ordinary page refresh discovers readiness without another reload or send.
    await expect(send).toBeEnabled({timeout: 20000});
    await expect(page.locator("#session-recovery")).toBeHidden();
    await expect(retry).toBeEnabled();
    await expect.poll(() => activityReads).toBeGreaterThan(readsBeforeFailureReload);
    await expect(page.locator("#codex-duration")).not.toHaveText("Timing unavailable");
    assertRequests(0); // Reload, details and restored dispatcher must stay read-only.
    const messageResponse = page.waitForResponse(response => response.request().method() === "POST" &&
      new URL(response.url()).pathname === messagePath);
    await retry.click();
    const response = await messageResponse;
    const raw = await response.text();
    console.log(`${engineName}/${scenario} actual message HTTP ${response.status()}: ${raw}`);
    assertRequests(1);
    assert.deepEqual(executionRequests[0].body, {
      message: originalText, clientUserMessageId: originalID, retry: true,
      ...(attachmentIds.length ? {attachmentIds} : {}),
    });
    const result = JSON.parse(raw);
    if (scenario === "accepted") {
      assert.equal(response.status(), 202, raw);
      assert.equal(result.clientUserMessageId, originalID);
      assert(result.turnId, "real admission must return the original accepted receipt");
      await expect(receipt).toContainText("Sent to Codex");
      const saved = await savedAttempt();
      assert.equal(saved.state, "accepted");
      assert.equal(saved.message, originalText);
      assert.deepEqual(saved.attachmentIds, attachmentIds);
    } else {
      assert.equal(response.status(), 409, raw);
      assert.equal(result.code, "message_delivery_unknown");
      assert.equal(result.notSent, false);
      await expect(receipt).toContainText("Outcome unknown.");
      await expect(page.locator("#message-submission-status")).toContainText("Retry the original submission to check its status.");
      const saved = await savedAttempt();
      assert.equal(saved.state, undefined, "the send receipt encodes uncertainty by omitting a delivery state");
      assert.equal(saved.message, originalText);
      assert.deepEqual(saved.attachmentIds || [], []);
      const dispatch = await savedDispatch();
      assert.equal(dispatch.entries.length, 1);
      assert.equal(dispatch.entries[0].id, originalID);
      assert.equal(dispatch.entries[0].state, "unknown");
    }
    await page.reload({waitUntil: "domcontentloaded"});
    await expect(send).toBeEnabled();
    await expect(receipt).toContainText(scenario === "accepted" ? "Sent to Codex" : "Outcome unknown.");
    assertRequests(1); // Neither an accepted nor uncertain original is replayed.
    const saved = await savedAttempt();
    assert(saved, "the original receipt key must remain present");
    assert.equal(saved.message, originalText);
    assert.deepEqual(saved.attachmentIds || [], attachmentIds);
    const dispatch = await savedDispatch();
    assert.deepEqual(dispatch.entries, scenario === "accepted" ? [] : [{...attempt, retryable: false, retry: true}]);
    if (scenario === "unknown") {
      await expect(page.locator("#codex-duration")).not.toHaveText("Timing unavailable");
    } else {
      await expect.poll(() => activityReads).toBeGreaterThan(0);
    }
    assert.deepEqual(errors, []);
    console.log(`${engineName}/${scenario}: original identity retained through actual HTTP admission and reload`);
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
