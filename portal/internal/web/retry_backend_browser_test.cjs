"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium, expect} = require("@playwright/test");
const [base, manualBase, name] = process.argv.slice(2);
const mobile = name.startsWith("mobile"), member = name.endsWith("member");
const output = process.env.PORTAL_RETRY_BACKEND_OUTPUT;
const selected = member ? "?member=implementer0" : "";
const other = member ? "" : "?member=implementer0";
const conversationPath = member ? "/codex/conversations/example~implementer0" : "/api/sessions/example";
const policyPath = "/api/sessions/example/retry-policy";
const ancillaryExpectations = [
  {method: "GET", path: "/api/sessions/example/attention", query: "", status: 503,
    reason: "Minimal fixture does not initialize notifications", source: "notifications.go:477"},
  {method: "GET", path: "/api/browser-notifications", query: "?registrationId=", status: 503,
    reason: "Fresh fixture context has no registration or notifications service", source: "notifications.go:316; static/browser-notifications.js:308"},
  {method: "GET", path: "/api/sessions/example/coordination", query: "", status: 503,
    reason: "Disposable DevSession has no coordination status reader", source: "coordination.go:15"},
  {method: "GET", path: "/api/codex-providers", query: "", status: 503,
    reason: "newTestServer deliberately selects an absent provider catalog", source: "server_test.go:4276; codex_providers.go:113"},
  {method: "GET", path: "/api/codex-providers", query: "?session=example", status: 503,
    reason: "newTestServer deliberately selects an absent provider catalog", source: "server_test.go:4276; codex_providers.go:113"},
  {method: "GET", path: "/api/codex-limits", query: "?session=example", status: 503,
    reason: "Session limits first resolves the absent provider catalog", source: "codex_limits.go:35"},
  {method: "GET", path: "/api/sessions/example/auto-archive", query: "", status: 409,
    reason: "Disposable DevSession has no auto-archive status fixture", source: "auto_archive.go:53"},
];
const ancillaryExpectation = failure => failure.kind === "http" && [base, manualBase].includes(failure.origin) &&
  ancillaryExpectations.find(expected => ["method", "path", "query", "status"].every(key => failure[key] === expected[key]));
const cases = [], requests = [], failures = [], errors = [], timeline = [], payloads = [];
const requestDiagnostics = [], requestRecords = new WeakMap();
let requestSequence = 0, watchedPageSequence = 0;
let phase = "startup", page, browser;
const trace = (event, values = {}) => {
  timeline.push({sequence: timeline.length + 1, phase, event, ...values});
};
const watch = p => {
  const watchedPage = ++watchedPageSequence;
  let documentOrdinal = 0;
  p.on("pageerror", error => errors.push(error.message));
  p.on("request", request => {
    if (request.isNavigationRequest() && request.frame() === p.mainFrame()) documentOrdinal++;
    const url = new URL(request.url());
    const row = {sequence: ++requestSequence, watchedPage, documentOrdinal, startPhase: phase, startMs: performance.now()};
    requestRecords.set(request, row);
    if (url.origin === base && [
      "POST /api/sessions/example/queue/reconcile", "GET /api/sessions/example/thread/page",
      "POST /codex/conversations/example~implementer0/queue/reconcile", "GET /codex/conversations/example~implementer0/thread/page",
    ].includes(request.method() + " " + url.pathname)) {
      Object.assign(row, {origin: url.origin, method: request.method(), path: url.pathname, query: url.search,
        activeAtSnapshot: true, join: "unjoined"});
      requestDiagnostics.push(row);
    }
    if (request.method() === "POST" && !request.url().includes("/__retry_backend__/")) {
      const row = {url: request.url(), body: request.postDataJSON()};
      requests.push(row); trace("post", row);
    }
  });
  p.on("response", async response => {
    const url = new URL(response.url());
    const row = requestRecords.get(response.request());
    if (row?.join) {
      const serverRequest = response.headers()["x-retry-fixture-request"];
      Object.assign(row, {status: response.status(), responsePhase: phase, responseMs: performance.now()});
      if (serverRequest) Object.assign(row, {serverRequest, join: "response-header"});
    }
    if (response.status() >= 400) failures.push({kind: "http", origin: url.origin, method: response.request().method(),
      path: url.pathname, query: url.search, status: response.status(), phase});
    if (url.pathname.endsWith("/retry/activity")) {
      try { const value = await response.json(); payloads.push({path: url.pathname, status: response.status(), value}); }
      catch (_) { /* Page teardown may abort a completed read body. */ }
    }
  });
  p.on("requestfinished", request => {
    const row = requestRecords.get(request);
    if (row?.join) Object.assign(row, {activeAtSnapshot: false, endPhase: phase, endMs: performance.now(), end: "finished"});
  });
  p.on("requestfailed", request => {
    const row = requestRecords.get(request);
    if (row?.join) Object.assign(row, {activeAtSnapshot: false, endPhase: phase, endMs: performance.now(),
      end: "failed", failureKind: request.failure()?.errorText});
    if (request.resourceType() === "eventsource" || request.failure()?.errorText === "net::ERR_ABORTED") return;
    const url = new URL(request.url());
    failures.push({kind: "transport", origin: url.origin, method: request.method(), path: url.pathname,
      query: url.search, error: request.failure()?.errorText, phase});
  });
};
const control = async (operation, host = base) => {
  const response = await page.request.post(host + "/__retry_backend__/control", {
    headers: {Origin: host}, data: {operation},
  });
  assert.equal(response.status(), 200, `${operation}: ${await response.text()}`);
  const value = await response.json(); trace("control", {operation, ...value}); return value;
};
const diagnostics = async (host = base) => {
  const response = await page.request.get(host + "/__retry_backend__/diagnostics");
  assert.equal(response.status(), 200); return response.json();
};
// These requests reach production routes. No browser route interception, retry
// DTO, comparison token, receipt or client UUID is synthesized in this fixture.
const api = (pathname, body = null, target = page) => target.evaluate(async ({pathname, body}) => {
  const response = await fetch(pathname, {method: body === null ? "GET" : "POST",
    headers: {"content-type": "application/json"}, ...(body === null ? {} : {body: JSON.stringify(body)})});
  return {status: response.status, value: await response.json()};
}, {pathname, body});
const changePolicySwitch = async (target, selector, checked, expectedStatus = 200) => {
  const input = target.locator(selector), url = new URL(policyPath, target.url()).href;
  await expect(input).toBeEnabled();
  if (checked) await expect(input).not.toBeChecked();
  else await expect(input).toBeChecked();
  const rows = () => requests.filter(row => row.url === url), before = rows().length;
  const response = expectedStatus === null ? null : target.waitForResponse(r => r.url() === url && r.request().method() === "POST");
  // The real handler restores the saved value while its asynchronous write is
  // pending. Assert the committed value after the response, not inside click.
  await input.click();
  await expect.poll(() => rows().length).toBe(before + 1);
  const body = rows().at(-1).body;
  assert.deepEqual(Object.keys(body).sort(), ["capacity", "cybersecurity", "expectedRevision"]);
  if (response) {
    const result = await response, value = await result.json();
    trace("policy-response", {status: result.status(), value});
    assert.equal(result.status(), expectedStatus);
    if (expectedStatus === 200) {
      assert.equal(value.capacity, body.capacity); assert.equal(value.cybersecurity, body.cybersecurity);
      assert.equal(typeof value.revision, "string"); assert(value.revision.length > 0);
      assert.notEqual(value.revision, body.expectedRevision);
      await expect(input).toBeEnabled();
      if (checked) await expect(input).toBeChecked();
      else await expect(input).not.toBeChecked();
      const readback = await api(policyPath, null, target);
      assert.equal(readback.status, 200); assert.deepEqual(readback.value, value);
    } else {
      assert.equal(value.code, "retry_action_stale");
    }
  }
  assert.equal(rows().length, before + 1, "one switch action writes policy exactly once");
};
const readActivity = () => api(conversationPath + "/retry/activity");
const refresh = async () => {
  const response = page.waitForResponse(r => r.url() === base + conversationPath + "/retry/activity");
  await page.evaluate(() => dispatchEvent(new Event("focus")));
  const result = await response; assert.equal(result.status(), 200); return result.json();
};
const load = async (host = base, suffix = "#codex", target = page, selection = selected) => {
  const response = await target.goto(host + "/example/" + selection + suffix);
  assert.equal(response.status(), 200);
  assert(response.headers()["content-security-policy"]?.includes("script-src"), "actual template CSP is present");
  await expect(target.locator("#message-form textarea[name=message]")).toBeVisible();
  await expect(target.locator("#transcript-entries .message")).toHaveCount(20);
  await expect(target.locator(".codex-upload-toggle")).toBeEnabled();
};
const settings = async (target = page) => {
  await target.locator("#session-tab-settings").click();
  await expect(target.locator("#retry-capacity")).toBeEnabled();
  await expect(target.locator("#retry-cybersecurity")).toBeEnabled();
};
const screenshot = async label => {
  await page.screenshot({path: path.join(output, label + ".png"), animations: "disabled"});
};
const reloadActivity = async () => {
  const response = page.waitForResponse(r => r.url() === base + conversationPath + "/retry/activity");
  await page.reload();
  const value = await (await response).json();
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  return value;
};
const complete = label => { cases.push(label); trace("case-pass", {label}); };

(async () => {
  assert(output, "Go fixture must supply its unique artifact directory");
  fs.mkdirSync(output, {recursive: true});
  browser = await chromium.launch({headless: true, channel: "chromium"});
  const context = await browser.newContext({ignoreHTTPSErrors: true,
    viewport: mobile ? {width: 390, height: 844} : {width: 1440, height: 1000}});
  page = await context.newPage(); watch(page);
  try {
    phase = "policy-scope";
    await load();
    const firstPolicy = await api(policyPath);
    assert.equal(firstPolicy.status, 200); assert.equal(firstPolicy.value.capacity, true);
    assert.equal(firstPolicy.value.cybersecurity, true);
    await settings();
    await expect(page.locator("#session-retry-scope")).toContainText(/all|every/i);
    assert.equal(await page.locator("#turn-retry-enabled").count(), 0, "per-thread switch removed");
    const revisionBeforeRead = (await api(policyPath)).value.revision;
    assert.equal(revisionBeforeRead, firstPolicy.value.revision);
    await changePolicySwitch(page, "#retry-capacity", false);
    await expect(page.locator("#retry-capacity")).toBeEnabled();
    const disabled = (await api(policyPath)).value;
    assert.notEqual(disabled.revision, firstPolicy.value.revision);
    assert.equal(disabled.capacity, false);
    const second = await context.newPage(); watch(second);
    await load(base, "#codex", second, other); await settings(second);
    await expect(second.locator("#retry-capacity")).not.toBeChecked();
    await expect(second.locator("#retry-cybersecurity")).toBeChecked();
    await changePolicySwitch(second, "#retry-capacity", true);
    await expect(second.locator("#session-retry-policy-status")).toHaveText("Retry settings saved.");
    const enabled = (await api(policyPath)).value;
    assert.equal(enabled.capacity, true); assert.notEqual(enabled.revision, disabled.revision);
    complete(phase);

    phase = "policy-stale-and-lost-reply";
    // Keep the first tab in front while the second tab makes its real HTTP
    // write. Returning focus must not refresh away the intended stale revision.
    await page.bringToFront();
    const firstRead = page.waitForResponse(r => r.url() === base + policyPath && r.request().method() === "GET");
    await page.evaluate(() => dispatchEvent(new Event("focus"))); await firstRead;
    await expect(page.locator("#retry-capacity")).toBeChecked();
    assert.equal((await api(policyPath, {capacity: true, cybersecurity: false, expectedRevision: enabled.revision}, second)).status, 200);
    await changePolicySwitch(page, "#retry-capacity", false, 409);
    await expect(page.locator("#session-retry-policy-status")).toHaveText("Retry settings changed elsewhere. Current settings have been reloaded.");
    await expect(page.locator("#retry-capacity")).toBeChecked();
    await expect(page.locator("#retry-cybersecurity")).not.toBeChecked();
    const beforeLoss = await diagnostics();
    await control("lose-policy-reply");
    await changePolicySwitch(page, "#retry-capacity", false, null);
    await expect(page.locator("#session-retry-policy-status")).toHaveText("Could not confirm the change. Current retry settings have been reloaded.");
    await expect(page.locator("#retry-capacity")).not.toBeChecked();
    const afterLoss = await diagnostics();
    assert.equal(afterLoss.posts, beforeLoss.posts + 1, "uncertain preference reply is read back, never repeated");
    assert.equal(afterLoss.dropped, 1);
    const current = (await api(policyPath)).value;
    const beforeReplay = await diagnostics();
    const replay = await api(policyPath, {capacity: current.capacity, cybersecurity: current.cybersecurity, expectedRevision: enabled.revision});
    assert.equal(replay.status, 200); assert.equal(replay.value.revision, current.revision);
    const afterReplay = await diagnostics();
    assert.equal(afterReplay.policyHash, beforeReplay.policyHash); assert.equal(afterReplay.ledgerHash, beforeReplay.ledgerHash);
    await changePolicySwitch(page, "#retry-capacity", true);
    await expect(page.locator("#session-retry-policy-status")).toHaveText("Retry settings saved.");
    await changePolicySwitch(page, "#retry-cybersecurity", true);
    await expect(page.locator("#session-retry-policy-status")).toHaveText("Retry settings saved.");
    await second.close();
    await page.locator("#session-tab-codex").click();
    complete(phase);

    phase = "scheduled-now";
    const scheduled = await control("schedule");
    const scheduledPayload = await refresh(), issued = scheduledPayload.activity;
    assert(issued && issued.phase === "scheduled");
    const footer = page.locator("#automatic-turn-retry");
    const now = footer.getByRole("button", {name: "Retry now", exact: true});
    const stop = footer.getByRole("button", {name: "Stop retrying", exact: true});
    await expect(footer).toBeVisible(); await expect(footer).toContainText("0 retries started");
    await expect(footer).toContainText(/Retrying in \d+s\./); await expect(footer).toContainText("Total elapsed");
    await expect(footer.getByRole("button", {name: "Dismiss retry notice", exact: true})).toBeHidden();
    assert(await footer.evaluate(node => node.parentNode.id === "transcript" && node.previousElementSibling.id === "transcript-entries"));
    await screenshot("active");
    const actionResponse = page.waitForResponse(r => r.url() === base + conversationPath + "/retry/now" && r.request().method() === "POST");
    await now.click(); const acceleratedResponse = await actionResponse;
    assert.equal(acceleratedResponse.status(), 200);
    assert.deepEqual(acceleratedResponse.request().postDataJSON(), {token: issued.retryNowToken});
    const accelerated = await diagnostics();
    for (const key of ["pending", "chain", "attempt", "charges", "starts"]) assert.equal(accelerated[key], scheduled[key], key);
    const acceleratedPayload = await acceleratedResponse.json();
    assert(accelerated.deadline < scheduled.deadline, "Retry now must shorten the future deadline");
    assert(accelerated.deadline <= acceleratedPayload.serverNowMs);
    const nowReplay = await api(conversationPath + "/retry/now", {token: issued.retryNowToken});
    assert.equal(nowReplay.status, 200);
    assert.equal((await diagnostics()).ledgerHash, accelerated.ledgerHash);
    complete(phase);

    phase = "stop-and-stale-token";
    const choiceBeforeStop = await api(policyPath);
    const stoppedResponse = page.waitForResponse(r => r.url() === base + conversationPath + "/retry/stop" && r.request().method() === "POST");
    await stop.click(); const stopped = await stoppedResponse;
    assert.equal(stopped.status(), 200); assert.deepEqual(stopped.request().postDataJSON(), {token: issued.stopToken});
    const cancelled = await diagnostics();
    assert.equal(cancelled.cancelled, true); assert.equal(cancelled.pending, scheduled.pending);
    assert.equal(cancelled.chain, scheduled.chain); assert.equal(cancelled.starts, scheduled.starts);
    assert.deepEqual((await api(policyPath)).value, choiceBeforeStop.value);
    assert.equal((await api(conversationPath + "/retry/stop", {token: issued.stopToken})).status, 200);
    assert.equal((await diagnostics()).ledgerHash, cancelled.ledgerHash);
    const oldPolicy = (await api(policyPath)).value;
    const revisionChange = await api(policyPath, {capacity: false, cybersecurity: true, expectedRevision: oldPolicy.revision});
    assert.equal(revisionChange.status, 200);
    assert.equal((await api(conversationPath + "/retry/stop", {token: issued.stopToken})).status, 409);
    assert.equal((await diagnostics()).ledgerHash, cancelled.ledgerHash);
    assert.equal((await api(policyPath, {capacity: true, cybersecurity: true, expectedRevision: revisionChange.value.revision})).status, 200);
    await control("schedule");
    const newChain = await diagnostics(); assert.notEqual(newChain.chain, cancelled.chain);
    const writesBeforeStale = requests.length;
    assert.equal((await api(conversationPath + "/retry/stop", {token: issued.stopToken})).status, 409);
    assert.equal((await api(conversationPath + "/retry/now", {token: issued.retryNowToken})).status, 409);
    assert.equal((await diagnostics()).ledgerHash, newChain.ledgerHash);
    assert.equal(requests.length, writesBeforeStale + 2, "stale comparison does not fall back to another operation");
    assert.equal(requests.filter(row => /\/(message|interrupt)$/.test(new URL(row.url).pathname)).length, 0);
    complete(phase);

    phase = "unknown-and-confirmed";
    const unknown = await control("unknown");
    const unknownPayload = await refresh();
    assert.equal(unknownPayload.activity.unconfirmed, 1);
    assert.equal(unknownPayload.activity.confirmedStarts, 0);
    await expect(footer).toContainText("1 retry awaiting confirmation"); await screenshot("uncertain");
    const unknownChain = unknownPayload.activity.chainKey;
    await stop.click(); await expect(footer).toContainText("Retrying stopped.");
    const stoppedUnknown = await diagnostics();
    assert.equal(stoppedUnknown.uncertain, true); assert.equal(stoppedUnknown.pending, unknown.pending);
    assert.equal(stoppedUnknown.starts, unknown.starts);
    const confirmed = await control("confirm-gap");
    const gap = await refresh();
    assert.equal(gap.activity.chainKey, unknownChain); assert.equal(gap.activity.unconfirmed, 0);
    assert.equal(gap.activity.countQuality, "unavailable"); assert.equal(gap.activity.confirmedStarts, null);
    await expect(footer).toContainText("Retry count unavailable"); await screenshot("checkpoint-gap");
    assert.equal(confirmed.pending, unknown.pending); assert(confirmed.result); assert.equal(confirmed.starts, unknown.starts);
    await control("checkpoint"); const exact = await refresh();
    assert.equal(exact.activity.confirmedStarts, 1); assert.equal(exact.activity.unconfirmed, 0);
    assert.equal(exact.activity.chainKey, unknownChain);
    await control("checkpoint"); const duplicate = await refresh();
    assert.equal(duplicate.activity.confirmedStarts, 1); assert.equal((await diagnostics()).starts, unknown.starts);
    assert.equal((await diagnostics()).charges, 1);
    complete(phase);

    phase = "terminal-dismissal";
    const terminal = (await readActivity()).value;
    assert.equal(terminal.activity.phase, "terminal"); assert.equal(terminal.activity.confirmedStarts, 1);
    await screenshot("terminal"); await reloadActivity();
    await expect(footer).toBeVisible(); await expect(footer).toContainText("1 retry started");
    const reloaded = (await readActivity()).value;
    assert.equal(reloaded.activity.elapsedMs, terminal.activity.elapsedMs);
    assert.equal(reloaded.activity.finishedAtMs, terminal.activity.finishedAtMs);
    const beforeDismiss = await diagnostics(), dismissPosts = requests.length;
    await footer.getByRole("button", {name: "Dismiss retry notice", exact: true}).click();
    await expect(footer).toBeHidden();
    assert.equal(requests.length, dismissPosts);
    const afterDismiss = await diagnostics();
    assert.equal(afterDismiss.ledgerHash, beforeDismiss.ledgerHash); assert.equal(afterDismiss.policyHash, beforeDismiss.policyHash);
    const dismissedReload = await reloadActivity();
    assert.equal(dismissedReload.activity.chainKey, terminal.activity.chainKey);
    await expect(footer).toBeHidden();
    assert.equal(await footer.evaluate(node => node.getBoundingClientRect().height), 0);
    await screenshot("dismissed");
    await control("schedule"); const next = await refresh();
    assert.notEqual(next.activity.chainKey, terminal.activity.chainKey); await expect(footer).toBeVisible();
    complete(phase);

    phase = "footer-refresh";
    await footer.evaluate(node => { window.backendFooter = node; });
    await page.locator('[data-transcript-filter="all"]').click();
    await page.locator('[data-transcript-filter="messages"]').click();
    const viewport = page.locator("#transcript");
    // The owning renderer requires directional user input for history reads.
    // Position near the top, then use its real keyboard Home path.
    await viewport.evaluate(node => { node.scrollTop = 100; });
    await viewport.focus(); await viewport.press("Home");
    await expect(page.locator("#transcript-entries .message")).toHaveCount(40);
    await viewport.evaluate(node => { node.scrollTop = Math.round(node.scrollHeight / 3); });
    await expect.poll(() => viewport.evaluate(node => node.scrollHeight - node.clientHeight - node.scrollTop)).toBeGreaterThan(48);
    const anchor = page.getByText("Backend fixture conversation message 31.", {exact: false}).first();
    const beforeTop = (await anchor.boundingBox()).y;
    const draft = page.locator('#message-form textarea[name="message"]');
    await draft.fill("Retain the next draft"); await draft.focus();
    await draft.evaluate(node => node.setSelectionRange(3, 9));
    const reads = (await diagnostics()).displayReads;
    await control("append");
    await expect.poll(async () => (await diagnostics()).displayReads).toBeGreaterThan(reads);
    await expect(page.locator("#transcript-entries .message")).toHaveCount(41);
    assert(Math.abs((await anchor.boundingBox()).y - beforeTop) < 2, "ordinary refresh retains the same scrolled-up anchor");
    assert(await footer.evaluate(node => node === window.backendFooter));
    await expect(draft).toBeFocused(); await expect(draft).toHaveValue("Retain the next draft");
    assert.deepEqual(await draft.evaluate(node => [node.selectionStart, node.selectionEnd]), [3, 9]);
    for (const operation of ["idle", "legacy-off"]) {
      await control(operation); const empty = await refresh(); assert.equal(empty.activity, null);
      await expect(footer).toBeHidden(); assert.equal(await footer.evaluate(node => node.getBoundingClientRect().height), 0);
    }
    await settings();
    await changePolicySwitch(page, "#retry-capacity", false);
    await expect(page.locator("#session-retry-policy-status")).toHaveText("Retry settings saved.");
    await changePolicySwitch(page, "#retry-cybersecurity", false);
    await expect(page.locator("#session-retry-policy-status")).toHaveText("Retry settings saved.");
    await page.locator("#session-tab-codex").click();
    assert.equal((await refresh()).activity, null);
    assert.equal(await footer.evaluate(node => node.getBoundingClientRect().height), 0);
    complete(phase);

    phase = "optional-unavailable-manual";
    await load(manualBase);
    const capable = await api(policyPath); assert.equal(capable.status, 200);
    assert.equal((await api(conversationPath + "/retry/activity")).status, 200);
    await settings(); await expect(page.locator("#retry-capacity")).toBeChecked();
    await control("corrupt-policy", manualBase);
    await page.evaluate(() => dispatchEvent(new Event("focus")));
    await expect(page.locator("#session-retry-policy-status")).toHaveText("Automatic retry settings are unavailable.");
    await expect(page.locator("#retry-capacity")).toBeDisabled(); await expect(page.locator("#retry-cybersecurity")).toBeDisabled();
    assert.equal((await api(conversationPath + "/retry/activity")).status, 503);
    await page.locator("#session-tab-codex").click();
    const manualBefore = await diagnostics(manualBase);
    const send = page.locator("#message-send"); await expect(send).toBeEnabled();
    await page.locator('#message-form textarea[name="message"]').fill("manual message");
    const messageResponse = page.waitForResponse(r => r.url() === manualBase + conversationPath + "/message" && r.request().method() === "POST");
    await send.click(); const sent = await messageResponse;
    assert.equal(sent.status(), 202, await sent.text());
    const original = sent.request().postDataJSON();
    assert.match(original.clientUserMessageId, /^[0-9a-f-]{36}$/);
    assert.equal(original.message, "manual message"); assert.equal(original.retry, false);
    const receipt = await sent.json();
    assert.equal(receipt.clientUserMessageId, original.clientUserMessageId);
    const stored = await page.evaluate(thread => Object.keys(sessionStorage)
      .filter(key => key.startsWith(`workspace-portal.send-attempt.example.${thread}.`))
      .map(key => ({id: key.split(".").at(-1), ...JSON.parse(sessionStorage.getItem(key))})), member ? "member-thread" : "thread-1");
    assert(stored.some(value => value.id === original.clientUserMessageId && value.message === "manual message"), "original browser record survives real acceptance");
    await expect(page.locator('#message-receipts [data-receipt-state="accepted"]')).toContainText("Sent to Codex");
    await expect(send).toBeEnabled();
    const manualAfter = await diagnostics(manualBase);
    assert.equal(manualAfter.starts, manualBefore.starts + 1);
    assert.equal(manualAfter.policyHash, manualBefore.policyHash, "manual delivery never repairs or reads optional policy as a prerequisite");
    assert.equal(requests.filter(row => row.url === manualBase + conversationPath + "/message").length, 1);
    assert.equal(manualAfter.settingsWrites, manualBefore.settingsWrites);
    await screenshot("manual-accepted");
    complete(phase);

    const expectedAncillary = failures.filter(ancillaryExpectation).map(failure => ({...failure, expectation: ancillaryExpectation(failure)}));
    const unexpected = failures.filter(failure => !ancillaryExpectation(failure) && !(
      failure.origin === base && failure.method === "POST" && failure.query === "" && failure.kind === "http" && failure.status === 409 && [policyPath, conversationPath + "/retry/stop", conversationPath + "/retry/now"].includes(failure.path) ||
      failure.origin === manualBase && failure.method === "GET" && failure.query === "" && failure.kind === "http" && failure.status === 503 && failure.phase === "optional-unavailable-manual" && [policyPath, conversationPath + "/retry/activity"].includes(failure.path) ||
      failure.origin === base && failure.method === "POST" && failure.query === "" && failure.kind === "transport" && failure.path === policyPath && failure.phase === "policy-stale-and-lost-reply"
    ));
    assert.deepEqual(errors, []); assert.deepEqual(unexpected, []);
    for (const row of requests.filter(row => /\/retry\/(now|stop)$/.test(new URL(row.url).pathname))) assert.deepEqual(Object.keys(row.body), ["token"]);
    const manifest = {name, viewport: mobile ? [390, 844] : [1440, 1000], member, cases, failures, expectedAncillary, requests, payloads, timeline, requestDiagnostics};
    fs.writeFileSync(path.join(output, "manifest.json"), JSON.stringify(manifest, null, 2) + "\n");
    console.log(JSON.stringify({name, cases, pass: true}));
  } catch (error) {
    const expectedAncillary = failures.filter(ancillaryExpectation).map(failure => ({...failure, expectation: ancillaryExpectation(failure)}));
    const evidence = {name, phase, url: page.url(), cases, errors, failures, expectedAncillary, requests, payloads, timeline, requestDiagnostics, error: error.stack};
    try { evidence.geometry = await page.locator("#transcript").evaluate(node => ({top: node.scrollTop, height: node.scrollHeight, viewport: node.clientHeight, footer: document.getElementById("automatic-turn-retry")?.getBoundingClientRect().toJSON()})); }
    catch (failure) { evidence.geometryError = failure.message; }
    try { await screenshot("failure"); } catch (failure) { evidence.screenshotError = failure.message; }
    fs.writeFileSync(path.join(output, "failure.json"), JSON.stringify(evidence, null, 2) + "\n");
    throw error;
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
