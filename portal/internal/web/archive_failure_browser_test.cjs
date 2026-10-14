"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: "chromium"} : {})});
    try {
      const page = await browser.newPage({ignoreHTTPSErrors: true});
      const errors = [];
      page.on("pageerror", error => { errors.push(error.message); console.error("Page error:", error.stack); });
      page.on("requestfailed", request => console.error("Request failed:", request.url(), request.failure()));
      await page.addInitScript(() => {
        const realNow = Date.now;
        window.archiveTestOffset = 0;
        Date.now = () => realNow() + window.archiveTestOffset;
      });
      let reads = 0, offline = false, held = false, release;
      let detailsMode = "actual", mutations = 0;
      let operationReads = 0, holdOperation = false, releaseOperation;
      let documents = 0, clearNextDocument = false;
      let operation = {kind: "archive", state: "paused", phase: "tracking_committed", receiptId: "receipt-1",
        startedAt: "2026-09-16T13:02:17Z", updatedAt: "2026-09-16T13:02:17Z",
        options: {journalId: "a".repeat(64), journalExpected: true}};
      const failure = {identity: "thread-1", operation: {id: "a".repeat(64), identity: "thread-1"}, result: "deferred",
        checked_at: "2026-09-16T20:02:12Z", blockers: ["command failed with exit 124: /nix/store/fixture/bin/workspace-portal thread retire"]};
      await page.route("**/example/", async route => {
        documents++;
        const response = await route.fetch();
        let html = await response.text();
        if (operation.state === "complete" || clearNextDocument && documents > 1) {
          html = html.replace('data-pending-lifecycle="archive"', 'data-pending-lifecycle=""')
            .replace(/(<button[^>]*id="archive-session-open")/, "$1 hidden");
        }
        return route.fulfill({response, body: html});
      });
      await page.route("**/api/sessions/example/**", async route => {
        const method = new URL(route.request().url()).pathname.split("/").at(-1);
        const json = (value, status = 200) => route.fulfill({status, contentType: "application/json", body: JSON.stringify(value)});
        if (route.request().method() !== "GET") {
          mutations++;
          return json({error: "Unexpected mutation in read-only archive fixture"}, 503);
        }
        if (method === "details" && detailsMode !== "actual") {
          if (detailsMode === "offline") return json({error: "Fixture unavailable"}, 503);
          const response = await route.fetch(), payload = await response.json();
          if (detailsMode === "older") delete payload.pendingLifecycle;
          else payload.pendingLifecycle = "";
          return json(payload);
        }
        if (method === "operation") {
          operationReads++;
          if (holdOperation) {
            holdOperation = false;
            await new Promise(resolve => { releaseOperation = resolve; });
          }
          return json(operation);
        }
        if (method === "auto-archive") {
          reads++;
          if (held) await new Promise(resolve => { release = resolve; });
          if (offline) return json({error: "Fixture unavailable"}, 503);
          return json(failure);
        }
        if (method === "thread") return json({threadId: "thread-1", status: "idle", entries: []});
        if (["pending", "queue"].includes(method)) return json([]);
        return route.continue();
      });
      await page.goto(process.argv[2] + "/example/");
      const banner = page.locator("#archive-last-failure");
      await expect(banner).toContainText("Closing the conversation timed out.");
      await expect(page.locator("#lifecycle-operation-detail")).toContainText("since the last completed step");
      await expect(page.locator("#message-form")).toHaveCount(0);
      const initial = await banner.textContent();
      const operationDetail = page.locator("#lifecycle-operation-detail");
      const responseFor = endpoint => page.waitForResponse(response =>
        new URL(response.url()).pathname === `/api/sessions/example/${endpoint}` &&
        response.request().method() === "GET");
      const wake = async () => page.evaluate(() => {
        window.archiveTestOffset += 31_000;
        document.dispatchEvent(new Event("visibilitychange"));
      });
      // A settings visit shares the pending-page read, and focus does not evade throttling.
      await banner.getByRole("link", {name: "Technical details"}).click();
      await expect(page.locator("#settings")).toBeVisible();
      await expect(banner).toBeHidden();
      await expect(page.locator("#auto-archive-values")).toContainText("Closing the conversation timed out.");
      await expect(page.locator("#auto-archive-details")).not.toHaveAttribute("open");
      assert.equal(reads, 1);
      offline = true;
      const offlineOperationResponse = responseFor("operation");
      await wake();
      await expect.poll(() => reads).toBe(2);
      assert.equal((await (await offlineOperationResponse).json()).state, "paused");
      await expect(page.locator("#auto-archive-status")).toContainText("Could not refresh");
      assert.equal(await banner.textContent(), initial);
      offline = false; held = true;
      const heldOperationResponse = responseFor("operation");
      await wake();
      await expect.poll(() => reads).toBe(3);
      assert.equal((await (await heldOperationResponse).json()).state, "paused");
      await page.evaluate(() => {
        document.dispatchEvent(new Event("visibilitychange"));
        window.dispatchEvent(new Event("pageshow"));
      });
      assert.equal(reads, 3);
      held = false; release();
      await expect(page.locator("#auto-archive-status")).toBeHidden();
      await page.locator("#session-tab-codex").click();
      failure.operation.id = "b".repeat(64);
      const priorClearedDetail = await operationDetail.textContent();
      operation = {...operation, updatedAt: "2026-09-16T13:07:17Z"};
      const clearedOperationResponse = responseFor("operation");
      await wake();
      assert.equal((await (await clearedOperationResponse).json()).state, "paused");
      await expect(banner).toBeHidden();
      await expect(operationDetail).not.toHaveText(priorClearedDetail);
      failure.operation.id = "a".repeat(64);
      // Hidden pages do not poll. Waking after the deadline refreshes the warning.
      await page.evaluate(() => Object.defineProperty(document, "hidden", {configurable: true, value: true}));
      const hiddenReads = reads;
      await wake();
      assert.equal(reads, hiddenReads);
      const previousPausedDetail = await operationDetail.textContent();
      operation = {...operation, updatedAt: "2026-09-16T13:12:17Z"};
      const pausedOperationResponse = responseFor("operation");
      const resumedArchiveResponse = responseFor("auto-archive");
      await page.evaluate(() => {
        Object.defineProperty(document, "hidden", {configurable: true, value: false});
        document.dispatchEvent(new Event("visibilitychange"));
      });
      const [pausedOperation, resumedArchive] = await Promise.all([pausedOperationResponse, resumedArchiveResponse]);
      assert.equal((await pausedOperation.json()).state, "paused");
      assert.equal((await resumedArchive.json()).operation.id, "a".repeat(64));
      await expect(banner).toBeVisible();
      await expect(operationDetail).not.toHaveText(previousPausedDetail);
      await expect(operationDetail).toContainText("Paused");
      operation = {...operation, state: "running"};
      const runningOperationResponse = responseFor("operation");
      await wake();
      assert.equal((await (await runningOperationResponse).json()).state, "running");
      await expect(operationDetail).toContainText("Running");
      await expect(banner).toContainText("Last automatic attempt failed");
      failure.last_attempt_error = "archived tracking changed during recovery: <script>example</script>";
      failure.last_attempt_at = "2026-10-07T12:06:43Z";
      failure.journal = {operation: "archive", phase: "clusters_released"};
      failure.diagnostics = [
        {code: "archive_proof_failed", category: "tracking", message: "Archival proof failed; retry after resolving the recorded failure."},
        {code: "archive_operation_pending", category: "lifecycle_pending", message: "An accepted lifecycle operation needs its recorded retry."},
        {code: "observation_stale", category: "observation_stale", message: "The cached observation is older than two hours."},
      ];
      operation = {...operation, state: "paused"};
      await page.locator("#session-tab-settings").click();
      await wake();
      const values = page.locator("#auto-archive-values");
      await expect(values).toContainText("Archiving session records: archived tracking changed during recovery:");
      await expect(values).not.toContainText("An accepted lifecycle operation");
      await expect(banner).toBeHidden();
      assert.equal(await values.locator("script").count(), 0);
      await page.reload();
      await expect(values).toContainText("Archiving session records: archived tracking changed during recovery:");
      await expect(page.locator("#auto-archive-details")).not.toHaveAttribute("open");
      await page.locator("#auto-archive-details summary").click();
      await expect(page.locator("#auto-archive-details pre")).toContainText(failure.last_attempt_error);
      failure.result = "archived";
      failure.last_attempt_error = null;
      failure.diagnostics = [{code: "observation_stale", category: "observation_stale", message: "The cached observation is older than two hours."}];
      await page.reload();
      await expect(values).toHaveText("Session archived.");
      await expect(page.locator("#auto-archive-details")).not.toHaveAttribute("open");
      await page.locator("#auto-archive-details summary").click();
      await expect(page.locator("#auto-archive-details pre")).toContainText("The cached observation is older than two hours.");
      // Completion must clear the historical warning and reload the final page.
      operationReads = 0;
      operation = {...operation, state: "complete"};
      const nextNavigation = page.waitForEvent("framenavigated", {predicate: frame => frame === page.mainFrame()});
      await wake();
      await nextNavigation;
      await expect(banner).toBeHidden();
      await expect.poll(() => operationReads).toBeGreaterThanOrEqual(2);

      // Expired receipts leave idle status. Neither idle nor an unavailable
      // journal check proves completion; an actual journal retains recovery.
      operation = {state: "idle", options: {}};
      for (const mode of ["actual", "older", "offline"]) {
        detailsMode = mode;
        documents = 0;
        operationReads = 0;
        await page.goto(process.argv[2] + "/example/");
        await expect.poll(() => operationReads).toBeGreaterThanOrEqual(1);
        await expect(page.locator("#lifecycle-operation-retry")).toBeVisible();
        const response = responseFor("operation");
        await wake();
        await response;
        await expect(page.locator("#lifecycle-operation-retry")).toBeVisible();
        assert.equal(documents, 1, `${mode} read incorrectly reloaded`);
      }

      // Hiding the page while Retry awaits operation status cancels its
      // details preflight. It must not fall through to the cached receipt.
      detailsMode = "actual";
      operation = {kind: "archive", state: "paused", receiptId: "receipt-1",
        options: {journalId: "a".repeat(64), journalExpected: true}};
      operationReads = 0;
      documents = 0;
      await page.goto(process.argv[2] + "/example/");
      await expect(page.locator("#lifecycle-operation-retry")).toBeVisible();
      await expect.poll(() => operationReads).toBeGreaterThanOrEqual(2);
      holdOperation = true;
      await page.locator("#archive-session-open").click();
      await expect.poll(() => Boolean(releaseOperation)).toBe(true);
      await page.evaluate(() => {
        Object.defineProperty(document, "hidden", {configurable: true, value: true});
        document.dispatchEvent(new Event("visibilitychange"));
      });
      detailsMode = "completed";
      releaseOperation();
      await expect(page.locator("#archive-session-open")).toBeEnabled();
      assert.equal(mutations, 0, "canceled preflight issued a mutation");
      assert.equal(documents, 1);

      // A fresh details response clears an old tab even with no receipt.
      operation = {state: "idle", options: {}};
      detailsMode = "completed";
      clearNextDocument = true;
      documents = 0;
      operationReads = 0;
      await page.goto(process.argv[2] + "/example/");
      await expect.poll(() => documents).toBe(2);
      await expect.poll(() => operationReads).toBeGreaterThanOrEqual(2);
      await expect(page.locator("body")).toHaveAttribute("data-pending-lifecycle", "");
      await expect(page.locator("#archive-session-open")).toBeHidden();
      await expect(page.locator("#lifecycle-operation-retry")).toBeHidden();
      await wake();
      assert.equal(documents, 2, "completed page entered a reload loop");

      // Retry must check fresh details even when its old receipt still matches.
      detailsMode = "actual";
      operation = {kind: "archive", state: "paused", receiptId: "receipt-1",
        options: {journalId: "a".repeat(64), journalExpected: true}};
      documents = 0;
      operationReads = 0;
      await page.goto(process.argv[2] + "/example/");
      await expect(page.locator("#lifecycle-operation-retry")).toBeVisible();
      await expect.poll(() => operationReads).toBeGreaterThanOrEqual(2);
      await page.evaluate(() => Object.defineProperty(document, "hidden", {configurable: true, value: true}));
      detailsMode = "completed";
      await page.locator("#archive-session-open").click();
      await expect.poll(() => documents).toBe(2);
      await expect.poll(() => operationReads).toBeGreaterThanOrEqual(4);
      await expect(page.locator("#lifecycle-operation-retry")).toBeHidden();
      assert.equal(mutations, 0, "completed archive issued a mutation");
      await page.route("**/api/index-status*", async route => {
        const response = await route.fetch(), payload = await response.json();
        payload.authoritative = true;
        payload.operations = [];
        for (const item of payload.sessions || []) delete item.pendingLifecycle;
        return route.fulfill({response, json: payload});
      });
      await page.goto(process.argv[2] + "/");
      await expect(page.locator('[data-session-slug="example"] [data-session-lifecycle]')).toBeHidden();
      assert.deepEqual(errors, []);
      console.log(engine.name() + ": archive diagnostics, throttling, recovery and read-only state passed");
    } finally { await browser.close(); }
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
