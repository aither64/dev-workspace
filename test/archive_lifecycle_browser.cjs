// Packaged VM acceptance: real shipped browser code and application responses.
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const {performance} = require("node:perf_hooks");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE);

const [slug, action] = process.argv.slice(2);
assert(/^[a-z0-9-]+$/.test(slug));
assert(["archive", "revive", "retry"].includes(action));

(async () => {
  const browser = await chromium.launch({executablePath: process.env.CHROMIUM_EXECUTABLE,
    headless: true, args: ["--no-sandbox", "--disable-dev-shm-usage"]});
  let page;
  let phase = "open page";
  const started = performance.now(), elapsed = () => Math.round(performance.now() - started);
  const errors = [], requests = [], consoleErrors = [], inflight = new Map();
  const dropped = {errors: 0, requests: 0, console: 0, inflight: 0};
  let activeRequests = 0;
  const navigation = {state: "pending"};
  const pathOnly = value => {
    try {
      const url = new URL(value);
      return ["http:", "https:"].includes(url.protocol) ? url.pathname.slice(0, 200) : "non-http";
    } catch (_) { return "invalid-url"; }
  };
  const record = (entries, value, key, limit) => {
    if (entries.length === limit) {entries.shift(); dropped[key]++;}
    entries.push(value);
  };
  const requestInfo = request => ({path: pathOnly(request.url()),
    method: request.method(), type: request.resourceType()});
  try {
    const context = await browser.newContext({ignoreHTTPSErrors: true,
      httpCredentials: {username: "developer", password: fs.readFileSync(
        "/var/lib/dev-workspaces/password/password", "utf8").trim()}});
    page = await context.newPage();
    page.on("pageerror", error => record(errors, {at: elapsed(), name:
      ["Error", "TypeError", "ReferenceError", "SyntaxError", "RangeError", "URIError", "EvalError", "AggregateError"].includes(error.name) ? error.name : "Error"}, "errors", 20));
    page.on("console", message => {
      if (!["error", "warning"].includes(message.type())) return;
      const location = message.location();
      record(consoleErrors, {at: elapsed(), type: message.type(), path: pathOnly(location.url),
        line: location.lineNumber, column: location.columnNumber}, "console", 20);
    });
    page.on("request", request => {
      const entry = {...requestInfo(request), at: elapsed()};
      activeRequests++;
      if (inflight.size < 40) inflight.set(request, entry);
      else dropped.inflight++;
      record(requests, {...entry, event: "start"}, "requests", 120);
    });
    page.on("response", response => record(requests, {...requestInfo(response.request()),
      at: elapsed(), event: "headers", status: response.status()}, "requests", 120));
    for (const event of ["requestfinished", "requestfailed"]) page.on(event, request => {
      const entry = inflight.get(request), failure = request.failure()?.errorText;
      activeRequests = Math.max(0, activeRequests - 1);
      inflight.delete(request);
      record(requests, {...requestInfo(request), at: elapsed(),
        event: event === "requestfinished" ? "finish" : "failure",
        ...(entry ? {duration: elapsed() - entry.at} : {}),
        ...(event === "requestfailed" ? {failure: /^net::ERR_[A-Z0-9_]+$/.test(failure || "") ? failure : "request failed"} : {})}, "requests", 120);
    });
    const origin = "https://fixture.workspace.example.test";
    // app.js imports conversation assets asynchronously before binding controls.
    // Its initial operation read occurs after lifecycle handlers are installed.
    const [hydrated] = await Promise.all([
      page.waitForResponse(response => response.request().method() === "GET" &&
        response.url() === `${origin}/api/sessions/${slug}/operation`),
      page.goto(`${origin}/${slug}/`).then(response => {
        Object.assign(navigation, {state: "completed", at: elapsed(), status: response?.status()});
        phase = "wait for initial operation";
        return response;
      }, error => {
        Object.assign(navigation, {state: "failed", at: elapsed()});
        throw error;
      }),
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
    assert.equal(accepted.status(), 202);
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
    let readiness = {state: "no page"}, deadline;
    try {
      if (page) readiness = await Promise.race([
        page.evaluate(() => {
          const pathOnly = value => {
            try {const url = new URL(value); return ["http:", "https:"].includes(url.protocol) ? url.pathname.slice(0, 200) : "non-http";}
            catch (_) {return "invalid-url";}
          };
          const resources = performance.getEntriesByType("resource");
          return {state: document.readyState, resources: resources.slice(-40).map(entry => ({
            path: pathOnly(entry.name), type: entry.initiatorType, start: Math.round(entry.startTime),
            duration: Math.round(entry.duration), responseStart: Math.round(entry.responseStart),
            responseEnd: Math.round(entry.responseEnd), transferSize: entry.transferSize,
            encodedBodySize: entry.encodedBodySize, decodedBodySize: entry.decodedBodySize})),
          resourcesOmitted: Math.max(0, resources.length - 40),
          controls: [...document.querySelectorAll("#archive-session-open, #revive-session-open, " +
            "#lifecycle-operation-status, #session-details-warning, dialog[open]")].slice(0, 10)
            .map(element => ({id: element.id.slice(0, 80), tag: element.tagName,
              hidden: element.hidden, open: Boolean(element.open), disabled: Boolean(element.disabled)}))};
        }),
        new Promise(resolve => {deadline = setTimeout(() => resolve({state: "diagnostic timeout"}), 1000);}),
      ]);
    } catch (_) { readiness = {state: "diagnostic unavailable"}; }
    finally { clearTimeout(deadline); }
    try {
      console.error(JSON.stringify({phase, at: elapsed(), navigation, errors, console: consoleErrors,
        requests, inflight: {total: activeRequests, listed: [...inflight.values()]}, dropped, readiness}));
    } catch (_) { /* Preserve the original failure if diagnostics cannot be serialized. */ }
    throw error;
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
