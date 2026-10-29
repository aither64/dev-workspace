// Packaged VM acceptance: real shipped browser code and application responses.
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE);

const [slug, action] = process.argv.slice(2);
assert(/^[a-z0-9-]+$/.test(slug));
assert(["archive", "revive", "retry"].includes(action));

(async () => {
  const browser = await chromium.launch({executablePath: process.env.CHROMIUM_EXECUTABLE,
    headless: true, args: ["--no-sandbox", "--disable-dev-shm-usage"]});
  let page;
  let phase = "open page";
  const errors = [], requests = [];
  const origin = "https://fixture.workspace.example.test";
  const operationURL = `${origin}/api/sessions/${slug}/operation`;
  const startedAt = performance.now(), operationRequests = new Map();
  const elapsedMs = () => Math.round(performance.now() - startedAt);
  try {
    const context = await browser.newContext({ignoreHTTPSErrors: true,
      httpCredentials: {username: "developer", password: fs.readFileSync(
        "/var/lib/dev-workspaces/password/password", "utf8").trim()}});
    page = await context.newPage();
    page.on("pageerror", error => errors.push(error.message));
    page.on("request", request => {
      if (request.method() !== "GET" || request.url() !== operationURL) return;
      operationRequests.set(request, {startedAtMs: elapsedMs()});
      if (operationRequests.size > 8) operationRequests.delete(operationRequests.keys().next().value);
    });
    page.on("response", response => {
      requests.push({path: new URL(response.url()).pathname,
        method: response.request().method(), status: response.status()});
      const observed = operationRequests.get(response.request());
      if (observed) Object.assign(observed, {responseAtMs: elapsedMs(), status: response.status()});
    });
    page.on("requestfailed", request => {
      requests.push({path: new URL(request.url()).pathname, failure: request.failure()?.errorText});
      const observed = operationRequests.get(request);
      if (observed) Object.assign(observed, {failedAtMs: elapsedMs(), failure: request.failure()?.errorText});
    });
    // app.js imports conversation assets asynchronously before binding controls.
    // Its initial operation read occurs after lifecycle handlers are installed.
    const [hydrated] = await Promise.all([
      page.waitForResponse(response => response.request().method() === "GET" &&
        response.url() === operationURL),
      page.goto(`${origin}/${slug}/`),
    ]);
    assert(hydrated.ok());
    const endpoint = action === "retry" ? "operation/retry" : action;
    let control;
    if (action === "retry") {
      // A failed worker may have exited before creating a lifecycle journal.
      // Wait for app.js to load its receipt before selecting the Retry branch.
      phase = "wait for retry receipt";
      await page.locator("#lifecycle-operation-retry").waitFor({state: "visible"});
      assert.equal(await page.locator("#lifecycle-operation-retry").textContent(), "Retry archive");
      control = page.locator("#lifecycle-operation-retry");
    } else {
      phase = "open confirmation";
      await page.locator(`#${action}-session-open`).click();
      await page.locator(`#${action}-session-dialog`).waitFor({state: "visible"});
      control = page.locator(`#${action}-session-form button[type=submit]`);
    }
    phase = "submit " + action;
    const [accepted] = await Promise.all([
      page.waitForResponse(response => response.request().method() === "POST" &&
        response.url() === `${origin}/api/sessions/${slug}/${endpoint}`),
      control.click(),
    ]);
    assert.equal(accepted.status(), 202, accepted.status() === 202 ? "" : await accepted.text());
    // The application redirects after acceptance. Read the durable receipt via
    // the API context, whose response body survives page navigation.
    const retained = await context.request.get(`${origin}/api/sessions/${slug}/operation`);
    assert(retained.ok());
    const operation = await retained.json();
    assert.equal(operation.kind, action === "retry" ? "archive" : action);
    assert(operation.receiptId);
    assert.deepEqual(errors, []);
    process.stdout.write(JSON.stringify(operation) + "\n");
  } catch (error) {
    let hydration = {state: "no-page"}, diagnosticDeadline;
    try {
      if (page) hydration = await Promise.race([
        page.evaluate(() => ({state: "observed", readyState: document.readyState,
          visibility: document.visibilityState, focused: document.hasFocus(),
          appScriptPresent: Boolean(document.querySelector('script[src^="/static/app.js"]')),
          controls: [...document.querySelectorAll("#archive-session-open, #revive-session-open, #lifecycle-operation-retry, " +
            "#lifecycle-operation-status, #session-details-warning, dialog[open]")]
            .slice(0, 8).map(element => element.outerHTML.slice(0, 1200))})),
        new Promise(resolve => {diagnosticDeadline = setTimeout(() => resolve({state: "timeout"}), 1500);}),
      ]);
    } catch (_) { hydration = {state: "unavailable"}; }
    finally { clearTimeout(diagnosticDeadline); }
    const operation = [...operationRequests].map(([request, observed]) => ({...observed, timing: request.timing()}));
    console.error(JSON.stringify({phase, elapsedMs: elapsedMs(), errors, requests: requests.slice(-40), operation, hydration}));
    throw error;
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
