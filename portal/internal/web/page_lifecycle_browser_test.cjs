"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
const baseURL = process.argv[2];
const checkSidebarMenu = async page => {
  const toggle = page.getByRole("button", {name: "Workspace menu", exact: true});
  const menu = page.locator("#sidebar-menu");
  await toggle.focus();
  await page.keyboard.press("Enter");
  await expect(menu).toBeVisible();
  await expect(menu.getByRole("link", {name: "Operations", exact: true})).toBeFocused();
  await expect.poll(async () => {
    const button = await toggle.boundingBox(), bounds = await menu.boundingBox(), viewport = page.viewportSize();
    return bounds.x >= 0 && bounds.x + bounds.width <= viewport.width &&
      bounds.y >= button.y + button.height && bounds.y + bounds.height <= viewport.height;
  }).toBe(true);
  assert(await menu.evaluate(element => {
    const bounds = element.getBoundingClientRect();
    return element.contains(document.elementFromPoint(bounds.right - 10, bounds.top + 15));
  }), "popover is not clipped by the sidebar");
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
  await expect(toggle).toBeFocused();
  await toggle.click();
  await expect(menu).toBeVisible();
  await page.locator("main").click({position: {x: 5, y: 5}});
  await expect(menu).toBeHidden();
};
(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: "chromium", args: ["--ignore-certificate-errors"]} : {})});
    try {
      const page = await browser.newPage({ignoreHTTPSErrors: true});
      const errors = [], notices = [];
      let details = 0, activity = 0, failActivity = false, holdDetails = false, releaseDetails;
      let staleRepliesConstructed = 0, staleRepliesFulfilled = 0;
      let recoveryRepliesConstructed = 0, recoveryReplyFulfilled = null;
      let recoveryOverride = null;
      let recoveryNoticeID = "notice-one", noticeDismissed = false, failDismissal = false, staleNoticeRead = false;
      let inputBlockedReason = "", completedPlan = false, transcriptReads = 0;
      const messages = [];
      let dismissals = 0, continuationRequests = 0;
      page.on("pageerror", error => errors.push(error.message));
      await page.exposeFunction("recordNotice", value => notices.push(value));
      await page.addInitScript(() => {
        document.addEventListener("DOMContentLoaded", () => {
          const warning = document.getElementById("session-details-warning");
          if (warning) new MutationObserver(() => {
            if (!warning.hidden) window.recordNotice(warning.textContent);
          }).observe(warning, {attributes: true, childList: true, subtree: true});
        });
      });
      await page.route("**/api/collaboration-modes", route => route.fulfill({json: [
        {mode: "default", name: "Default"}, {mode: "plan", name: "Plan"},
      ]}));
      await page.route("**/api/sessions/example/**", async route => {
        const operation = new URL(route.request().url()).pathname.split("/").at(-1);
        if (operation === "continue" || operation === "message") continuationRequests++;
        if (operation === "message") {
          const message = route.request().postDataJSON(); messages.push(message);
          return route.fulfill({status: 202, json: {clientUserMessageId: message.clientUserMessageId, turnId: "manual-turn"}});
        }
        if (["thread", "page"].includes(operation) && (completedPlan || recoveryOverride === "idle")) {
          const read = ++transcriptReads;
          return route.fulfill({json: {
            threadId: "thread-1", status: "idle", collaborationMode: completedPlan ? "plan" : "default", model: "model-1", reasoningEffort: "medium",
            latestTurnId: "plan-turn", hasOlder: false,
            entries: [...(completedPlan ? [{turnId: "plan-turn", itemId: "plan", kind: "plan", turnStatus: "completed", text: "A completed plan"}] : []),
              {turnId: "plan-turn", itemId: "read-marker", kind: "agentMessage", text: `Recovery fixture read ${read}`}],
          }});
        }
        if (operation === "dismiss-recovery") {
          dismissals++;
          assert.equal(route.request().postDataJSON().noticeId, recoveryNoticeID);
          if (failDismissal) return route.fulfill({status: 503, json: {error: "Unable to save dismissal"}});
          noticeDismissed = true;
          return route.fulfill({json: {state: "waiting", executionHeld: true, workKind: "interrupted",
            inputBlockedReason, noticeId: recoveryNoticeID, noticeDismissed: true}});
        }
        if (operation === "details") {
          details++;
          if (holdDetails) {
            holdDetails = false;
            await new Promise(resolve => { releaseDetails = resolve; });
            return route.abort("failed").catch(() => {});
          }
          if (recoveryOverride) {
            const state = recoveryOverride, staleReply = staleNoticeRead && state === "waiting";
            const noticeId = recoveryNoticeID, dismissed = noticeDismissed;
            const response = await route.fetch(), payload = await response.json();
            const recoveryReplyID = ++recoveryRepliesConstructed;
            payload.repositoriesHTML += `<span hidden data-fixture-recovery-reply="${recoveryReplyID}"></span>`;
            const staleReplyID = staleReply ? ++staleRepliesConstructed : 0;
            if (staleReply) assert(dismissed, "stale reply must contradict a completed dismissal");
            await route.fulfill({response, json: {...payload,
              ...(staleReply ? {repositoriesHTML: payload.repositoriesHTML +
                `<span hidden data-fixture-stale-notice-reply="${staleReplyID}"></span>`} : {}),
              interactive: state !== "stopped", recovery: {state, executionHeld: state !== "active", inputBlockedReason,
                ...(state === "waiting" ? {noticeId, noticeDismissed: dismissed && !staleReply} : {})}}});
            recoveryReplyFulfilled = {id: recoveryReplyID, state, noticeId, dismissed, staleReply};
            if (staleReply) staleRepliesFulfilled = staleReplyID;
            return;
          }
        }
        if (operation === "activity") {
          activity++;
          if (failActivity) return route.fulfill({status: 503, body: "Temporary read failure"});
          return route.fulfill({contentType: "application/json", body: JSON.stringify({
            currentState: "working", workingMs: 30000, waitingMs: 10000,
            startedAtMs: Date.now() - 30000, observedAtMs: Date.now(), coverageComplete: true,
          })});
        }
        return route.continue();
      });
      await page.route("**/fixture/next", async route => {
        releaseDetails?.();
        await new Promise(resolve => setTimeout(resolve, 250));
        return route.fulfill({contentType: "text/html", body: "<p>Next page</p>"});
      });
      const refreshRecovery = async state => {
        const previous = recoveryRepliesConstructed;
        await expect.poll(async () => {
          await page.evaluate(() => dispatchEvent(new Event("focus")));
          const reply = recoveryReplyFulfilled;
          return reply?.id > previous && reply.state === state && await page.locator(
            `#repositories [data-fixture-recovery-reply="${reply.id}"]`).count() === 1;
        }).toBe(true);
      };
      const refreshTranscript = async () => {
        const previous = transcriptReads;
        // Details and transcript refresh independently; focus can be coalesced.
        // Wait for an ordinary forced cold read and its visible application.
        await expect.poll(async () => {
          await page.evaluate(() => dispatchEvent(new Event("online")));
          return transcriptReads;
        }).toBeGreaterThan(previous);
        await expect(page.locator("#transcript")).toContainText(`Recovery fixture read ${transcriptReads}`);
      };
      await page.goto(baseURL + "/example/");
      await expect(page.locator("#codex-duration")).toContainText("Working 30s");
      await page.setViewportSize({width: 420, height: 720});
      await checkSidebarMenu(page);
      await page.setViewportSize({width: 1280, height: 720});
      // Native download links must not retire the conversation read cycle.
      await page.evaluate(() => {
        const link = document.createElement("a");
        link.id = "fixture-download"; link.href = "/fixture/download"; link.download = "";
        document.body.append(link);
      });
      const downloaded = page.waitForEvent("download");
      await page.locator("#fixture-download").evaluate(link => link.click());
      await (await downloaded).delete();
      const beforeDownload = details;
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect.poll(() => details).toBeGreaterThan(beforeDownload);

      // Recovery updates the mounted composer in place through cold and live
      // states. A draft and textarea identity survive every status refresh.
      const composer = page.locator('#message-form textarea[name="message"]');
      await composer.fill("Keep this unsent recovery draft");
      await composer.evaluate(element => { window.recoveryComposer = element; });
      for (const state of ["stopped", "recovering", "waiting", "idle", "unavailable", "active"]) {
        recoveryOverride = state;
        const beforeStateRefresh = details, beforeStateReply = recoveryRepliesConstructed;
        await expect.poll(async () => {
          await page.evaluate(() => dispatchEvent(new Event("focus")));
          const reply = recoveryReplyFulfilled;
          return {started: details > beforeStateRefresh,
            applied: reply?.id > beforeStateReply && reply.state === state && await page.locator(
              `#repositories [data-fixture-recovery-reply="${reply.id}"]`).count() === 1,
            visible: await page.locator("#session-recovery").isVisible()};
        }).toEqual({started: true, applied: true, visible: !["active", "idle"].includes(state)});
        expect(details).toBeGreaterThan(beforeStateRefresh);
        if (state === "waiting") await expect(page.locator("#session-continue")).toBeVisible();
        if (state === "stopped") await expect(page.locator("#message-send")).toBeDisabled();
        await expect(composer).toHaveValue("Keep this unsent recovery draft");
        assert(await composer.evaluate(element => element === window.recoveryComposer), "recovery replaced composer");
      }
      recoveryOverride = "waiting";
      const beforeWaitingRefresh = details, beforeWaitingReply = recoveryRepliesConstructed;
      await expect.poll(async () => {
        await page.evaluate(() => dispatchEvent(new Event("focus")));
        const reply = recoveryReplyFulfilled;
        return details > beforeWaitingRefresh && reply?.id > beforeWaitingReply && reply.state === "waiting" &&
          reply.noticeId === recoveryNoticeID && !reply.dismissed && !reply.staleReply && await page.locator(
            `#repositories [data-fixture-recovery-reply="${reply.id}"]`).count() === 1;
      }).toBe(true);
      expect(details).toBeGreaterThan(beforeWaitingRefresh);
      const dismiss = page.getByRole("button", {name: "Dismiss restored-session notice", exact: true});
      await expect(dismiss).toBeVisible();
      const sendsBeforeDismissal = continuationRequests;
      failDismissal = true;
      await dismiss.click();
      await expect(page.locator("#session-recovery-message")).toContainText("Unable to save dismissal");
      await expect(page.locator("#session-recovery")).toBeVisible();
      failDismissal = false;
      await page.setViewportSize({width: 420, height: 720});
      assert(await dismiss.evaluate(element => {
        const bounds = element.getBoundingClientRect();
        return bounds.left >= 0 && bounds.right <= innerWidth;
      }), "dismiss control fits the mobile viewport");
      await dismiss.focus();
      await page.keyboard.press("Enter");
      await expect(page.locator("#session-recovery")).toBeHidden();
      await expect(page.locator("#codex-settings-open")).toBeEnabled();
      await expect(page.locator("#message-send")).toBeEnabled();
      await expect(composer).toHaveValue("Keep this unsent recovery draft");
      assert(await composer.evaluate(element => element === window.recoveryComposer));
      staleNoticeRead = true;
      const beforeStaleRefresh = details, beforeStaleReply = staleRepliesConstructed;
      await expect.poll(async () => {
        if (details <= beforeStaleRefresh) await page.evaluate(() => dispatchEvent(new Event("focus")));
        const replyID = staleRepliesFulfilled;
        return {started: details > beforeStaleRefresh, constructed: staleRepliesConstructed > beforeStaleReply,
          fulfilled: replyID > beforeStaleReply,
          consumed: replyID > beforeStaleReply && await page.locator(
            `#repositories [data-fixture-stale-notice-reply="${replyID}"]`).count() === 1};
      }).toEqual({started: true, constructed: true, fulfilled: true, consumed: true});
      expect(details).toBeGreaterThan(beforeStaleRefresh);
      await expect(page.locator("#session-recovery")).toBeHidden();
      staleNoticeRead = false;
      await page.reload();
      const beforeReloadRefresh = details, beforeReloadReply = recoveryRepliesConstructed, reloadedNoticeID = recoveryNoticeID;
      await expect.poll(async () => {
        await page.evaluate(() => dispatchEvent(new Event("focus")));
        const reply = recoveryReplyFulfilled;
        return details > beforeReloadRefresh && reply?.id > beforeReloadReply && reply.state === "waiting" &&
          reply.noticeId === reloadedNoticeID && reply.dismissed && !reply.staleReply && await page.locator(
            `#repositories [data-fixture-recovery-reply="${reply.id}"]`).count() === 1;
      }).toBe(true);
      expect(details).toBeGreaterThan(beforeReloadRefresh);
      await expect(page.locator("#session-recovery")).toBeHidden();
      recoveryNoticeID = "notice-two"; noticeDismissed = false;
      const beforeNewNoticeRefresh = details, beforeNewNoticeReply = recoveryRepliesConstructed;
      await expect.poll(async () => {
        await page.evaluate(() => dispatchEvent(new Event("focus")));
        const reply = recoveryReplyFulfilled;
        return details > beforeNewNoticeRefresh && reply?.id > beforeNewNoticeReply && reply.state === "waiting" &&
          reply.noticeId === "notice-two" && !reply.dismissed && !reply.staleReply && await page.locator(
            `#repositories [data-fixture-recovery-reply="${reply.id}"]`).count() === 1;
      }).toBe(true);
      expect(details).toBeGreaterThan(beforeNewNoticeRefresh);
      await expect(dismiss).toBeVisible();
      await dismiss.click();
      await expect(page.locator("#session-recovery")).toBeHidden();
      assert.equal(dismissals, 3);
      assert.equal(continuationRequests, sendsBeforeDismissal, "dismissal never sends a message or continues work");
      // A known blocker preserves the editable draft; an ordinary idle cold
      // classification has no restoration choice and grants no implicit load.
      // The reload above checks persisted notice dismissal, not persistence of
      // unsubmitted textarea text. Start this mounted-page check in its new document.
      await composer.fill("Keep this unsent recovery draft");
      await composer.evaluate(element => { window.recoveryComposer = element; });
      inputBlockedReason = "Saved queued messages must be handled first.";
      await expect.poll(async () => {
        await page.evaluate(() => dispatchEvent(new Event("focus")));
        return page.locator("#message-input-status").textContent();
      }).toBe(inputBlockedReason);
      await expect(page.locator("#message-send")).toBeDisabled();
      await expect(composer).toHaveValue("Keep this unsent recovery draft");
      assert(await composer.evaluate(element => element === window.recoveryComposer), "blocker refresh replaced composer");
      inputBlockedReason = ""; recoveryOverride = "idle"; completedPlan = true;
      await refreshRecovery("idle");
      await expect(page.locator("#session-recovery")).toBeHidden();
      await refreshTranscript();
      await expect(page.locator("#plan-actions")).toBeVisible();
      await expect(page.locator("#session-continue")).toBeHidden();
      // A new work episode needs a new notice identity. Reusing the dismissed
      // notice-two would correctly keep it hidden as a stale response.
      recoveryNoticeID = "notice-three"; recoveryOverride = "waiting"; noticeDismissed = false;
      await refreshRecovery("waiting");
      await expect(page.locator("#session-recovery")).toBeVisible();
      await expect(page.locator("#plan-actions")).toBeHidden();
      await dismiss.click();
      await expect(page.locator("#session-recovery")).toBeHidden();
      await expect(page.locator("#plan-actions")).toBeHidden();
      await expect(composer).toBeVisible();
      assert.equal(messages.length, 0, "details, plan display and dismissal cannot dispatch input");
      // Retain the already-saved waiting identity across a document replacement.
      recoveryNoticeID = "notice-four";
      completedPlan = false; recoveryOverride = "waiting"; noticeDismissed = false;
      const waitingID = "00000000-0000-4000-8000-000000000029";
      await page.evaluate(id => {
        const attempt = {id, message: "say hi", action: "send", attachmentIds: [], context: "", state: "waiting"};
        sessionStorage.setItem(`workspace-portal.send-attempt.example.thread-1.${id}`, JSON.stringify(attempt));
        sessionStorage.setItem("workspace-portal.message-dispatch.example.thread-1", JSON.stringify({schema: 1, entries: [attempt]}));
      }, waitingID);
      await page.reload();
      await refreshRecovery("waiting");
      await expect(page.locator("#session-recovery")).toBeVisible();
      const waiting = page.locator(".message-receipt").filter({hasText: "say hi"});
      await expect(waiting).toContainText("Waiting to send");
      assert.equal(messages.length, 0, "deployment/reload/readiness never replays the saved waiting message");
      const dismissalsBeforeIdle = dismissals;
      recoveryOverride = "idle";
      await refreshRecovery("idle");
      await expect(page.locator("#session-recovery")).toBeHidden();
      await expect(page.locator("#message-send")).toBeEnabled();
      await refreshTranscript();
      await expect(page.locator('#codex-mode button[data-codex-mode="default"]')).toBeVisible();
      await expect(page.locator('#codex-mode button[data-codex-mode="default"]')).toHaveAttribute("aria-pressed", "true");
      await expect(waiting).toContainText("Waiting to send");
      assert.equal(dismissals, dismissalsBeforeIdle, "verified idle removes an obsolete undismissed notice by observation alone");
      assert.equal(messages.length, 0, "waiting-to-idle does not grant automatic dispatch");
      await waiting.getByRole("button", {name: "Resume sending", exact: true}).click();
      await expect.poll(() => messages.length).toBe(1);
      assert.equal(messages[0].clientUserMessageId, waitingID);
      assert.equal(messages[0].message, "say hi");
      assert.equal(messages[0].retry, false);
      recoveryOverride = "failed";
      const beforeFailedRefresh = details, beforeFailedReply = recoveryRepliesConstructed;
      await expect.poll(async () => {
        await page.evaluate(() => dispatchEvent(new Event("focus")));
        const reply = recoveryReplyFulfilled;
        return details > beforeFailedRefresh && reply?.id > beforeFailedReply && reply.state === "failed" &&
          reply.noticeId === recoveryNoticeID && await page.locator(
            `#repositories [data-fixture-recovery-reply="${reply.id}"]`).count() === 1;
      }).toBe(true);
      expect(details).toBeGreaterThan(beforeFailedRefresh);
      await expect(page.locator("#session-recovery")).toBeVisible();
      await expect(dismiss).toBeHidden();
      await page.setViewportSize({width: 1280, height: 720});
      recoveryOverride = null;
      await composer.fill("");

      // A hidden document retains the last display while it obtains fresh timing.
      await page.evaluate(() => {
        Object.defineProperty(document, "hidden", {configurable: true, value: true});
        document.dispatchEvent(new Event("visibilitychange"));
      });
      failActivity = true;
      const beforeWake = activity;
      await page.evaluate(() => {
        Object.defineProperty(document, "hidden", {configurable: true, value: false});
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await expect.poll(() => activity).toBeGreaterThan(beforeWake);
      await page.waitForTimeout(1500);
      await expect(page.locator("#codex-work-elapsed")).not.toContainText("unavailable");
      failActivity = false;
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect(page.locator("#codex-duration")).toContainText("Working 30s");

      // Firefox can reject an in-flight read while the next document is loading.
      holdDetails = true;
      // A preceding focus refresh may still be in flight and coalesce this
      // event. Ask again once it finishes so the held read is actually started.
      await expect.poll(async () => {
        await page.evaluate(() => dispatchEvent(new Event("focus")));
        return Boolean(releaseDetails);
      }).toBe(true);
      await page.evaluate(() => {
        const link = document.createElement("a"); link.id = "fixture-next";
        link.href = "/fixture/next"; link.textContent = "Next page"; document.body.append(link);
      });
      await page.locator("#fixture-next").click();
      await page.waitForURL("**/fixture/next");
      const beforeBack = details;
      await page.goBack();
      await expect.poll(() => details).toBeGreaterThan(beforeBack);
      await expect(page.locator("#session-details-warning")).toBeHidden();
      await page.goto(baseURL + "/");
      await checkSidebarMenu(page);
      await page.getByRole("button", {name: "Workspace menu", exact: true}).click();
      await page.locator('#sidebar-menu a[href="/automatic-archival"]').click();
      await page.waitForURL("**/automatic-archival");
      assert.deepEqual(notices, []);
      assert.deepEqual(errors, []);
      console.log(engine.name() + ": sidebar menu, download, timing wake and navigation restoration passed");
    } finally { await browser.close(); }
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
