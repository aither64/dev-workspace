"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {chromium, expect} = require("@playwright/test");
const baseURL = process.argv[2];
const capture = process.env.PORTAL_RETRY_SCREENSHOTS;
const entries = (first, last) => Array.from({length: last - first + 1}, (_, index) => ({
  turnId: "task", itemId: `item-${first + index}`, kind: "agentMessage",
  text: `Saved conversation message ${first + index}.`, turnStatus: "completed",
  timestamp: new Date(1792770000000 + (first + index) * 1000).toISOString(),
}));
const activity = (overrides = {}) => ({
  chainKey: "fixture-chain-1", class: "capacity", phase: "scheduled",
  confirmedStarts: 3, unconfirmed: 0, countQuality: "exact", startedAtMs: 30000,
  elapsedMs: 70000, timingQuality: "exact", finishedAtMs: null,
  nextAttemptAtMs: 115000, outcome: "", reason: "",
  retryNowToken: "attempt-1", stopToken: "chain-1", ...overrides,
});

(async () => {
  if (capture) fs.mkdirSync(capture, {recursive: true});
  const browser = await chromium.launch({headless: true, channel: "chromium"});
  let reportFailure = null;
  try {
    for (const mobile of [false, true]) {
      const page = await browser.newPage({ignoreHTTPSErrors: true,
        viewport: mobile ? {width: 390, height: 844} : {width: 1440, height: 1000}});
      const errors = [], requestFailures = [], httpFailures = [], writes = [], actions = [];
      const timeline = [];
      const trace = (event, detail = {}) => {
        timeline.push({sequence: timeline.length ? timeline.at(-1).sequence + 1 : 1, event, ...detail});
        if (timeline.length > 200) timeline.shift();
      };
      const suffix = mobile ? "mobile" : "desktop";
      let phase = "startup";
      page.on("pageerror", error => errors.push(error.message));
      page.on("requestfailed", request => requestFailures.push({url: request.url(), error: request.failure()?.errorText}));
      page.on("response", response => { if (response.status() >= 400) httpFailures.push({url: response.url(), status: response.status()}); });
      page.on("console", message => {
        const prefix = "retry-fixture-geometry ";
        if (message.text().startsWith(prefix)) trace("geometry", JSON.parse(message.text().slice(prefix.length)));
      });
      reportFailure = async () => {
        const evidence = {viewport: suffix, phase, url: page.url(), pageErrors: errors.slice(-20).map(message => message.slice(0, 2048)),
          requestFailures: requestFailures.slice(-30), httpFailures: httpFailures.slice(-30)};
        try { evidence.geometry = await page.evaluate(() => window.retryGeometry?.("failure")); }
        catch (failure) { evidence.geometryError = failure.message; }
        evidence.timeline = timeline.slice();
        if (capture) {
          try {
            const file = `failure-${suffix}.png`;
            await page.screenshot({path: path.join(capture, file), animations: "disabled", timeout: 5000});
            evidence.screenshot = file;
          } catch (failure) { evidence.screenshotError = failure.message; }
          try { fs.writeFileSync(path.join(capture, `failure-${suffix}.json`), JSON.stringify(evidence, null, 2) + "\n"); }
          catch (failure) { evidence.artifactError = failure.message; }
        }
        console.error("Retry fixture failure evidence:", JSON.stringify(evidence));
      };
      const fixtureTime = new Date("2026-10-23T18:00:00Z");
      await page.clock.install({time: fixtureTime});
      await page.clock.pauseAt(fixtureTime);
      let state = null, policy = {revision: "policy-1", capacity: true, cybersecurity: true};
      let failPolicy = false, conflictPolicy = false, policyAvailable = true, activityAvailable = true;
      let newer = entries(101, 120), threadReads = 0, threadResponses = 0;
      let holdTranscript = false;
      const heldTranscriptReads = [];
      let legacyAssets = false;
      await page.addInitScript(() => {
        window.EventSource = class {
          constructor() { window.retryFixtureEvents = this; queueMicrotask(() => this.onopen?.()); }
          addEventListener() {}
          close() {}
        };
      });
      await page.route("**/codex/assets/conversation.js*", async route => {
        if (!legacyAssets) return route.continue();
        const response = await route.fetch();
        let source = await response.text();
        // Model an older embedding without the optional paging exports while
        // retaining its actual renderer/client/assets and the new notice.
        for (const name of ["createTranscriptHistory", "readTranscriptPage", "transcriptEntryKey"]) {
          source = source.replace(new RegExp(`export (async )?function ${name}\\(`), (_, async = "") => `${async}function ${name}(`);
        }
        return route.fulfill({response, body: source});
      });
      const serve = async route => {
        const url = new URL(route.request().url()), operation = url.pathname.split("/").at(-1);
        const json = (data, status = 200) => route.fulfill({status, json: data});
        if (url.pathname.endsWith("/retry-policy")) {
          if (!policyAvailable) return json({error: "Fixture optional policy unavailable"}, 404);
          if (route.request().method() === "POST") {
            const body = route.request().postDataJSON(); writes.push({path: url.pathname, body});
            if (conflictPolicy) { conflictPolicy = false; policy = {...policy, revision: "other-policy", cybersecurity: false}; return json({error: "Fixture revision changed"}, 409); }
            policy = {revision: `policy-${writes.length + 1}`, capacity: body.capacity, cybersecurity: body.cybersecurity};
            return json(policy);
          }
          return failPolicy ? json({error: "Fixture unavailable"}, 503) : json(policy);
        }
        if (url.pathname.endsWith("/retry/activity")) {
          trace("activity-fulfill", {class: state?.class, phase: state?.phase});
          return activityAvailable ? json({serverNowMs: 100000, activity: state}) : json({error: "Fixture optional activity unavailable"}, 404);
        }
        if (url.pathname.endsWith("/retry/now") || url.pathname.endsWith("/retry/stop")) {
          const body = route.request().postDataJSON(); actions.push({operation, body});
          assert.deepEqual(Object.keys(body), ["token"]);
          if (operation === "now") state = activity({...state, nextAttemptAtMs: 100000, retryNowToken: ""});
          else state = activity({...state, phase: "terminal", outcome: "stopped", finishedAtMs: 100000, nextAttemptAtMs: null, retryNowToken: "", stopToken: ""});
          return json({serverNowMs: 100000, activity: state});
        }
        if (url.pathname.endsWith("/retry") || url.pathname.endsWith("/retry/enabled") || url.pathname.endsWith("/retry/cancel")) throw new Error("Removed thread retry control used a legacy route");
        if (operation === "thread" || operation === "page") {
          threadReads++;
          const read = threadReads;
          trace("transcript-request", {read, held: holdTranscript});
          if (holdTranscript) await new Promise(resolve => heldTranscriptReads.push(resolve));
          threadResponses++;
          trace("transcript-fulfill", {read, response: threadResponses});
          return json({threadId: url.pathname.includes("~") ? "member-thread" : "thread-1",
            latestTurnId: "task", status: "idle", collaborationMode: "default", model: "model-1",
            reasoningEffort: "medium", entries: url.searchParams.has("cursor") ? entries(81, 100) : newer,
            hasOlder: !url.searchParams.has("cursor"), olderCursor: url.searchParams.has("cursor") ? null : "older-1"});
        }
        if (operation === "pending" || operation === "queue") return json([]);
        if (operation === "reconcile") return json({ok: true});
        if (operation === "activity") return json({currentState: "idle", workingMs: 30000, waitingMs: 10000, idleMs: 10000, coverageComplete: true, stateSinceMs: Date.now(), observedAtMs: Date.now()});
        if (operation === "details") return json({repositoriesHTML: "", artifactsHTML: ""});
        return route.continue();
      };
      await page.route("**/api/sessions/example/**", serve);
      await page.route("**/codex/conversations/**", serve);
      const screenshot = async name => { phase = name; if (capture) await page.screenshot({path: path.join(capture, `${name}-${suffix}.png`), animations: "disabled"}); };
      const compareDismissControls = async dismiss => {
        const receipt = page.locator('#message-receipts .message-receipt[data-receipt-state="accepted"]');
        const ordinary = receipt.getByRole("button", {name: "Dismiss message", exact: true});
        await expect(ordinary).toBeVisible();
        await expect(ordinary).toHaveText("×");
        const appearance = (locator, focused = false) => locator.evaluate((node, focused) => {
          const style = getComputedStyle(node);
          const fields = ["fontFamily", "fontSize", "fontWeight", "lineHeight", "color", "backgroundColor", "borderRadius"];
          if (focused) fields.push("outlineWidth", "outlineStyle", "outlineColor", "outlineOffset");
          return Object.fromEntries(fields.map(key => [key, style[key]]));
        }, focused);
        const compare = async stateName => {
          const expected = await appearance(ordinary), actual = await appearance(dismiss);
          trace("dismiss-comparison", {state: stateName, ordinary: expected, retry: actual});
          assert.deepEqual(actual, expected, `${stateName} retry dismiss differs from ordinary receipt`);
          assert.equal(actual.fontWeight, "400");
          return {ordinary: expected, retry: actual};
        };
        await page.locator("#message-form textarea").focus();
        await page.mouse.move(0, 0);
        await expect(ordinary).toBeInViewport();
        await expect(dismiss).toBeInViewport();
        const resting = await compare("rest");
        await screenshot("dismiss-controls-rest");
        await ordinary.hover(); const hover = await appearance(ordinary);
        await screenshot("dismiss-ordinary-hover");
        await dismiss.hover();
        const retryHover = await appearance(dismiss);
        trace("dismiss-comparison", {state: "hover", ordinary: hover, retry: retryHover});
        assert.deepEqual(retryHover, hover, "hover retry dismiss differs from ordinary receipt");
        await screenshot("dismiss-controls-hover");
        await page.mouse.move(0, 0);
        // Enter from keyboard navigation so :focus-visible is exercised on
        // both real controls, without activating either dismissal.
        await ordinary.focus(); await page.keyboard.press("Tab"); await page.keyboard.press("Shift+Tab");
        await expect(ordinary).toBeFocused();
        assert.equal(await ordinary.evaluate(node => node.matches(":focus-visible")), true);
        const keyboard = await appearance(ordinary, true);
        await screenshot("dismiss-ordinary-focus");
        await dismiss.focus(); await page.keyboard.press("Tab"); await page.keyboard.press("Shift+Tab");
        await expect(dismiss).toBeFocused();
        assert.equal(await dismiss.evaluate(node => node.matches(":focus-visible")), true);
        const retryKeyboard = await appearance(dismiss, true);
        trace("dismiss-comparison", {state: "keyboard-focus", ordinary: keyboard, retry: retryKeyboard});
        assert.deepEqual(retryKeyboard, keyboard, "keyboard-focus retry dismiss differs from ordinary receipt");
        assert.ok(parseFloat(retryKeyboard.outlineWidth) > 0, "retry keyboard outline has zero width");
        assert.ok(!["none", "hidden"].includes(retryKeyboard.outlineStyle), "retry keyboard outline is hidden");
        assert.ok(!["transparent", "rgba(0, 0, 0, 0)"].includes(retryKeyboard.outlineColor), "retry keyboard outline is transparent");
        const geometry = await dismiss.evaluate(node => {
          const rect = node.getBoundingClientRect(), heading = node.parentElement.getBoundingClientRect();
          return {width: rect.width, height: rect.height, right: rect.right, headingRight: heading.right,
            rootFont: parseFloat(getComputedStyle(document.documentElement).fontSize)};
        });
        const target = geometry.rootFont * (mobile ? 2.5 : 2);
        assert.ok(Math.abs(geometry.width - target) < 1 && geometry.height >= target,
          "retry dismiss lost its existing hit target: " + JSON.stringify(geometry));
        assert.ok(Math.abs(geometry.right - geometry.headingRight) < 1,
          "retry dismiss lost right alignment: " + JSON.stringify(geometry));
        trace("dismiss-geometry", geometry);
        const comparison = {viewport: suffix, resting, hover: {ordinary: hover, retry: retryHover},
          keyboardFocus: {ordinary: keyboard, retry: retryKeyboard}, geometry};
        console.log("Retry dismiss comparison: " + JSON.stringify(comparison));
        if (capture) fs.writeFileSync(path.join(capture, `dismiss-comparison-${suffix}.json`), JSON.stringify(comparison, null, 2) + "\n");
        await screenshot("dismiss-controls-focus");
        await ordinary.click();
        await expect(receipt).toHaveCount(0);
        await page.locator("#message-form textarea").focus();
      };
      const notice = page.locator("#automatic-turn-retry");
      const refresh = async () => {
        trace("focus-refresh", {phase, class: state?.class, activityPhase: state?.phase});
        await page.evaluate(() => dispatchEvent(new Event("focus")));
      };
      const startTranscript = async () => {
        // App sync schedules its first read at time zero. Drain that turn only
        // after module initialization, retaining the frozen capture timestamp.
        await expect.poll(() => page.evaluate(() => Boolean(window.retryFixtureEvents))).toBe(true);
        await page.clock.runFor(0);
      };
      const reload = async () => { phase = "reload"; await page.reload(); await startTranscript(); };
      const load = async (member, visible = true) => {
        phase = member ? "load-member" : "load-root";
        await page.goto(baseURL + `/example/${member ? "?member=implementer0" : ""}#codex`);
        await startTranscript();
        await expect(page.locator("#transcript-entries .message")).toHaveCount(newer.length);
        if (visible) await expect(notice).toBeVisible();
        else await expect(notice).toBeHidden();
        await expect(page.locator("#automatic-turn-retry input")).toHaveCount(0);
      };
      const scroll = page.locator("#transcript");
      const bottomDistance = () => scroll.evaluate(node => node.scrollHeight - node.clientHeight - node.scrollTop);
      await load(false, false);
      await page.evaluate(() => {
        const log = document.getElementById("transcript"), footer = document.getElementById("automatic-turn-retry");
        const bounds = node => {
          const rect = node?.getBoundingClientRect();
          return rect ? {top: rect.top, bottom: rect.bottom, height: rect.height} : null;
        };
        window.retryGeometry = (label, detail = {}) => {
          if (label === "wheel" && detail.deltaY < 0) window.retryUpwardWheels = (window.retryUpwardWheels || 0) + 1;
          if (label === "scroll") window.retryScrollEvents = (window.retryScrollEvents || 0) + 1;
          const anchor = window.retryAnchor?.node;
          const value = {label, ...detail, scrollTop: log.scrollTop, scrollHeight: log.scrollHeight,
            clientHeight: log.clientHeight, viewport: bounds(log), footer: bounds(footer), footerHidden: footer.hidden,
            entries: bounds(document.getElementById("transcript-entries")), anchor: bounds(anchor),
            anchorConnected: anchor?.isConnected, capturedAnchorTop: window.retryAnchor?.top,
            messages: document.querySelectorAll("#transcript-entries .message").length};
          console.debug("retry-fixture-geometry " + JSON.stringify(value));
          return value;
        };
        log.addEventListener("wheel", event => window.retryGeometry("wheel", {deltaY: event.deltaY}), {passive: true});
        log.addEventListener("scroll", () => window.retryGeometry("scroll"), {passive: true});
        new ResizeObserver(() => window.retryGeometry("resize")).observe(footer);
        new MutationObserver(() => window.retryGeometry("entries-mutation")).observe(document.getElementById("transcript-entries"), {childList: true});
        new MutationObserver(() => window.retryGeometry("footer-mutation")).observe(footer, {childList: true, subtree: true, attributes: true});
      });
      await expect.poll(bottomDistance).toBeLessThanOrEqual(2);
      // Focus refresh may request both lanes. Hold every transcript response
      // so its render cannot accidentally supply the footer's missing follow.
      holdTranscript = true;
      const initialThreadResponses = threadResponses;
      state = activity();
      await refresh();
      await expect(notice).toBeVisible();
      await expect.poll(bottomDistance).toBeLessThanOrEqual(2);
      await expect(notice).toContainText("3");
      await expect(notice).toContainText(/15s|15 seconds/);
      await expect(notice.getByRole("button", {name: "Retry now", exact: true})).toBeVisible();
      await expect(notice.getByRole("button", {name: "Stop retrying", exact: true})).toBeVisible();
      assert.equal(await page.locator("#transcript > #transcript-entries").count(), 1);
      assert.equal(await page.locator("#transcript > #automatic-turn-retry").count(), 1);
      await screenshot("capacity");
      const announcement = await notice.locator('[role="status"]').textContent();
      await page.clock.runFor(1000);
      await expect(notice).toContainText(/14s|14 seconds/);
      assert.equal(await notice.locator('[role="status"]').textContent(), announcement, "countdown was announced again");
      assert.equal(actions.length, 0, "browser timer dispatched retry action");
      const initialHeight = await notice.evaluate(node => node.getBoundingClientRect().height);
      state = activity({reason: "Synthetic fixture detail. ".repeat(16)});
      await refresh();
      await expect.poll(() => notice.evaluate(node => node.getBoundingClientRect().height)).toBeGreaterThan(initialHeight);
      await expect.poll(bottomDistance).toBeLessThanOrEqual(2);
      state = activity();
      await refresh();
      await expect.poll(() => notice.evaluate(node => node.getBoundingClientRect().height)).toBe(initialHeight);
      await expect.poll(bottomDistance).toBeLessThanOrEqual(2);
      assert.equal(threadResponses, initialThreadResponses, "transcript rendering masked the activity-only follow check");
      holdTranscript = false;
      trace("release-transcript", {held: heldTranscriptReads.length});
      for (const release of heldTranscriptReads.splice(0)) release();

      // Both render owners must leave the footer node, focus and selection intact.
      await page.locator("#message-form textarea").fill("Keep this draft.");
      await page.locator("#message-form textarea").evaluate(node => { node.focus(); node.setSelectionRange(2, 6); });
      phase = "scrolled-up-notice";
      await page.evaluate(() => window.retryGeometry("before-wheel"));
      const beforeWheel = await scroll.evaluate(node => ({top: node.scrollTop,
        wheels: window.retryUpwardWheels || 0, scrolls: window.retryScrollEvents || 0}));
      await scroll.hover(); await page.mouse.wheel(0, -120);
      // mouse.wheel resolves before native input delivery. The frozen JS clock
      // cannot establish that the compositor has scrolled or follow has paused.
      await expect.poll(() => scroll.evaluate((node, before) =>
        (window.retryUpwardWheels || 0) > before.wheels &&
        (window.retryScrollEvents || 0) > before.scrolls && node.scrollTop < before.top - 48 &&
        node.scrollHeight - node.clientHeight - node.scrollTop > 48, beforeWheel)).toBe(true);
      await page.clock.runFor(100);
      await page.evaluate(() => {
        window.retryFooter = document.getElementById("automatic-turn-retry");
        const log = document.getElementById("transcript");
        const message = [...document.querySelectorAll("#transcript-entries .message")].find(node => node.getBoundingClientRect().bottom > log.getBoundingClientRect().top);
        window.retryAnchor = {node: message, top: message.getBoundingClientRect().top};
        window.retryGeometry("anchor-captured");
      });
      state = activity({class: "cybersecurity", nextAttemptAtMs: 100000});
      await refresh();
      await expect(notice).toContainText(/Cybersecurity/i);
      await page.evaluate(() => window.retryGeometry("notice-updated"));
      assert.deepEqual(await page.locator("#message-form textarea").evaluate(node => [node.selectionStart, node.selectionEnd, document.activeElement === node]), [2, 6, true]);
      assert.ok(await page.evaluate(() => Math.abs(window.retryAnchor.node.getBoundingClientRect().top - window.retryAnchor.top) < 2), "notice update shifted a scrolled-up message");
      assert.ok(await page.evaluate(() => window.retryFooter === document.getElementById("automatic-turn-retry")));
      await scroll.evaluate(node => { node.scrollTop = node.scrollHeight; });
      await screenshot("cybersecurity");
      for (const filter of ["all", "team", "activity", "messages"]) {
        await page.locator(`[data-transcript-filter="${filter}"]`).click();
        assert.ok(await page.evaluate(() => window.retryFooter === document.getElementById("automatic-turn-retry")), "filter replaced retry footer");
      }
      // A live update and native history prepend retain the same footer and
      // keep an existing visible message anchored in the scroll owner.
      newer = entries(101, 121);
      await page.evaluate(() => window.retryFixtureEvents.onmessage());
      await page.clock.runFor(300);
      await expect(page.locator("#transcript-entries .message")).toHaveCount(21);
      await scroll.hover(); await page.mouse.wheel(0, -10000);
      await page.clock.runFor(300);
      await expect(page.locator("#transcript-entries .message")).toHaveCount(41);
      assert.ok(await page.evaluate(() => window.retryFooter === document.getElementById("automatic-turn-retry")), "backfill removed footer");
      await scroll.evaluate(node => { node.scrollTop = node.scrollHeight; });

      state = activity({phase: "reconciling", unconfirmed: 1, countQuality: "partial", retryNowToken: "", nextAttemptAtMs: null});
      await refresh();
      await expect(notice).toContainText(/unconfirmed|confirm/i);
      await expect(notice.getByRole("button", {name: "Retry now", exact: true})).toHaveCount(0);
      await screenshot("unconfirmed");
      state = activity({phase: "running", retryNowToken: "", nextAttemptAtMs: null});
      await refresh();
      await expect(notice).toContainText(/running|progress/i);
      await screenshot("running");

      for (const [outcome, extra] of [["success", {}], ["stopped", {confirmedStarts: 0}], ["exhausted", {class: "cybersecurity", confirmedStarts: null, countQuality: "unavailable", startedAtMs: null, elapsedMs: null, timingQuality: "unavailable"}]]) {
        state = activity({chainKey: `result-${outcome}`, phase: "terminal", outcome, retryNowToken: "", stopToken: "", nextAttemptAtMs: null, finishedAtMs: 100000, ...extra});
        if (outcome === "success") {
          // Hydrate the actual ordinary-receipt renderer from its supported
          // browser record; this fixture does not submit native input.
          await page.evaluate(() => sessionStorage.setItem(
            "workspace-portal.send-attempt.example.thread-1.00000000-0000-4000-8000-000000000099",
            JSON.stringify({message: "Keep this draft.", context: "", state: "accepted"}),
          ));
          await reload();
        }
        await refresh();
        await expect(notice).toBeVisible();
        const dismiss = notice.getByRole("button", {name: "Dismiss retry notice", exact: true});
        await expect(dismiss).toBeVisible();
        await expect(dismiss).toHaveText("×");
        await expect(dismiss).toHaveAttribute("title", "Dismiss retry notice");
        await scroll.evaluate(node => { node.scrollTop = node.scrollHeight; });
        await screenshot(outcome);
        if (outcome === "success") {
          await compareDismissControls(dismiss);
          await dismiss.click();
          await expect(notice).toBeHidden();
          assert.equal(await notice.evaluate(node => node.getBoundingClientRect().height), 0);
        }
      }
      const dismiss = notice.getByRole("button", {name: "Dismiss retry notice", exact: true});
      await dismiss.focus(); await page.keyboard.press("Enter");
      await expect(notice).toBeHidden();
      assert.equal(await notice.evaluate(node => node.getBoundingClientRect().height), 0);
      assert.equal(await scroll.evaluate(node => node.children.length), 2);
      await screenshot("dismissed");
      await reload();
      await expect(notice).toBeHidden();
      state = activity({chainKey: "next-chain"});
      await refresh(); await expect(notice).toBeVisible();
      await notice.getByRole("button", {name: "Retry now", exact: true}).click();
      await expect.poll(() => actions.at(-1)?.operation).toBe("now");
      assert.equal(actions.at(-1).body.token, "attempt-1");
      await notice.getByRole("button", {name: "Stop retrying", exact: true}).click();
      await expect.poll(() => actions.at(-1)?.operation).toBe("stop");
      assert.equal(actions.at(-1).body.token, "chain-1");
      assert.equal(writes.length, 0, "chain actions changed session policy");

      await page.locator('[data-session-tab="settings"]').click();
      const policyPanel = page.locator("#session-retry-policy");
      await expect(policyPanel).toBeVisible();
      await expect(policyPanel.getByRole("switch")).toHaveCount(2);
      await expect(policyPanel).toContainText("lead and all current and future retained team members");
      const capacity = policyPanel.getByRole("switch", {name: /Retry capacity errors/});
      const cyber = policyPanel.getByRole("switch", {name: /Retry cybersecurity errors/});
      await expect(capacity).toBeChecked();
      await capacity.focus(); await page.keyboard.press("Space");
      await expect(capacity).not.toBeChecked();
      await expect(page.locator("#session-retry-policy-status")).toHaveText("Retry settings saved.");
      assert.deepEqual(writes.at(-1), {path: "/api/sessions/example/retry-policy", body: {capacity: false, cybersecurity: true, expectedRevision: "policy-1"}});
      conflictPolicy = true;
      await cyber.click();
      await expect(page.locator("#session-retry-policy-status")).toContainText("changed elsewhere");
      await expect(cyber).not.toBeChecked();
      await screenshot("settings");
      failPolicy = true; await refresh();
      await expect(capacity).toBeDisabled();
      failPolicy = false;

      state = activity({chainKey: "member-chain"});
      await load(true);
      await expect(page.locator("#codex-member")).toHaveValue("implementer0");
      await scroll.evaluate(node => { node.scrollTop = node.scrollHeight; });
      await screenshot("member");
      await page.locator('[data-session-tab="settings"]').click();
      await expect(capacity).not.toBeChecked();
      await expect(cyber).not.toBeChecked();
      await capacity.click();
      await expect(capacity).toBeChecked();
      assert.equal(writes.at(-1).path, "/api/sessions/example/retry-policy", "member view wrote per-thread preference");
      state = null;
      await page.locator('[data-session-tab="codex"]').click();
      await refresh(); await expect(notice).toBeHidden();
      assert.equal(await notice.evaluate(node => node.getBoundingClientRect().height), 0);
      await screenshot("idle");

      for (const count of [1, 0]) {
        newer = entries(1, count); state = activity({chainKey: `short-${count}`});
        await reload(); await expect(notice).toBeVisible();
        await expect(page.locator("#transcript-entries .message")).toHaveCount(count);
        const geometry = await notice.evaluate(node => {
          const viewport = node.parentElement, bounds = viewport.getBoundingClientRect();
          return {bottom: node.getBoundingClientRect().bottom,
            wanted: bounds.bottom - parseFloat(getComputedStyle(viewport).paddingBottom),
            scrollHeight: viewport.scrollHeight, clientHeight: viewport.clientHeight};
        });
        assert.ok(Math.abs(geometry.bottom - geometry.wanted) <= 2, "short conversation footer is not at the viewport bottom: " + JSON.stringify(geometry));
        assert.ok(geometry.scrollHeight <= geometry.clientHeight + 1, "short notice introduced scrolling");
        await screenshot(count ? "short" : "empty");
      }
      state = null; await refresh(); await expect(notice).toBeHidden();
      assert.equal(await notice.evaluate(node => node.getBoundingClientRect().height), 0);
      assert.ok(await scroll.evaluate(node => node.scrollHeight <= node.clientHeight + 1), "idle empty footer leaves spacer");

      // Exercise the full-render compatibility path with the same real DOM.
      legacyAssets = true; state = activity({chainKey: "legacy-full-render"}); newer = entries(1, 2);
      await reload(); await expect(notice).toBeVisible();
      await expect(page.locator("#transcript-entries .message")).toHaveCount(2);
      await page.evaluate(() => { window.retryFooter = document.getElementById("automatic-turn-retry"); });
      newer = entries(1, 3); await page.evaluate(() => window.retryFixtureEvents.onmessage());
      await page.clock.runFor(300);
      await expect(page.locator("#transcript-entries .message")).toHaveCount(3);
      assert.ok(await page.evaluate(() => window.retryFooter === document.getElementById("automatic-turn-retry")), "full renderer removed footer");
      activityAvailable = false; policyAvailable = false;
      await refresh(); await expect(notice).toBeHidden();
      await page.locator('[data-session-tab="settings"]').click();
      await expect(policyPanel).toBeHidden();
      assert.ok(threadReads > 0);
      assert.deepEqual(errors, []);
      console.log(JSON.stringify({viewport: suffix, threadReads, preferenceWrites: writes.length, actions: actions.length, screenshots: Boolean(capture)}));
      await page.close(); reportFailure = null;
    }
  } catch (error) {
    try { await reportFailure?.(); } catch (failure) { console.error("Fixture evidence failed:", failure.message); }
    throw error;
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
