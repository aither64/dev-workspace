"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
const baseURL = process.argv[2];
const member = (address, currentState, waitReason = "", state = "ready") => ({address, state,
  snapshot: {currentState, waitReason, latestTurnStatus: currentState === "idle" ? "completed" : "inProgress",
    stateSinceMs: Date.now() - 1000, observedAtMs: Date.now(), coverageComplete: true}});

(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: "chromium"} : {})});
    let releaseTeamRead = () => {};
    try {
      const page = await browser.newPage({ignoreHTTPSErrors: true, viewport: {width: 1280, height: 720}});
      const errors = [];
      let focused = "lead", rows = [member("lead", "idle"), member("implementer0", "working")];
      let failTeam = false, holdTeam = false, teamReads = 0, heldReadFinished = false, planDecision = false;
      let addresses = ["architect0", "implementer0", "reviewer0", "worker0"];
      page.on("pageerror", error => errors.push(error.message));
      await page.route("**/example/**", async route => {
        focused = new URL(route.request().url()).searchParams.get("member") || "lead";
        const response = await route.fetch({url: baseURL + "/example/"});
        const body = (await response.text()).replace('data-selected-member=""',
          `data-selected-member="${focused === "lead" ? "" : focused}"`)
          .replace(/data-conversation-id="[^"]*"/,
            `data-conversation-id="example${focused === "lead" ? "" : "~" + focused}"`);
        await route.fulfill({response, body});
      });
      await page.route(/\/(?:api\/sessions\/example|codex\/conversations\/example~[^/]+)\//, async route => {
        const operation = new URL(route.request().url()).pathname.split("/").at(-1);
        const snapshot = rows.find(row => row.address === focused)?.snapshot || member(focused, "idle").snapshot;
        if (operation === "team-stats") {
          teamReads++;
          const observed = rows.map(row => ({...structuredClone(row), snapshot: {...row.snapshot, observedAtMs: Date.now()}}));
          const held = holdTeam;
          if (held) await new Promise(resolve => { releaseTeamRead = resolve; });
          await route.fulfill(failTeam ? {status: 503, json: {error: "Fixture unavailable"}} : {json: {members: observed}});
          if (held) heldReadFinished = true;
          return;
        }
        if (operation === "activity") return route.fulfill({json: snapshot});
        if (operation === "thread" || operation === "page") return route.fulfill({json: {
          threadId: "thread-1", status: snapshot.currentState === "idle" ? "idle" : "active",
          entries: planDecision ? [{kind: "plan", text: "Fixture plan", turnId: "plan-1", turnStatus: "completed"}] : [],
          latestTurnId: planDecision ? "plan-1" : "", collaborationMode: planDecision ? "plan" : "default",
          model: "model-1", reasoningEffort: "medium", hasOlder: false,
        }});
        if (operation === "details") return route.fulfill({json: {
          readyMembers: addresses, repositoriesHTML: "", artifactsHTML: "", repositoryCount: 0, artifactCount: 0, clusterCount: 0,
        }});
        if (operation === "pending" || operation === "queue") return route.fulfill({json: []});
        if (operation === "settings") return route.fulfill({json: {model: "model-1", reasoningEffort: "medium"}});
        return route.continue();
      });
      const refresh = () => page.evaluate(() => dispatchEvent(new Event("focus")));
      const label = page.locator("#codex-work-label"), work = page.locator("#codex-work");
      const dot = page.locator("#codex-waiting-indicator");
      const assertCompactStatus = async () => {
        const value = await label.textContent();
        await expect(label).toHaveAttribute("title", value);
        await expect(label).toHaveAttribute("aria-label", value);
        await expect(page.locator("#session-tab-codex")).toHaveAttribute("title", `Codex: ${value}`);
        for (const width of [1280, 420]) {
          await page.setViewportSize({width, height: 720});
          assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "team activity must fit viewport");
          await expect(work).toBeVisible();
          assert(await work.evaluate(element => {
            const bounds = element.getBoundingClientRect();
            const label = element.querySelector("#codex-work-label"), timer = element.querySelector("time");
            const labelStyle = getComputedStyle(label), timerBounds = timer.getBoundingClientRect();
            return getComputedStyle(element).flexWrap === "nowrap" && labelStyle.whiteSpace === "nowrap" &&
              labelStyle.textOverflow === "ellipsis" && labelStyle.overflow === "hidden" &&
              timerBounds.right <= bounds.right && timerBounds.left >= bounds.left &&
              timer.scrollWidth <= timer.clientWidth && timer.textContent.includes("waiting");
          }), "one status line must keep the timer visible");
          if (width === 420 && value.length > 100) assert(await label.evaluate(element => element.scrollWidth > element.clientWidth), "long status should clip");
        }
        await page.setViewportSize({width: 1280, height: 720});
      };
      await page.goto(baseURL + "/example/#codex");
      await expect(label).toHaveText("Other team members are working: implementer0.");
      await expect(dot).toBeHidden();
      assert(await page.locator("#team").evaluate(element => !element.classList.contains("active")), "Team tab must stay closed");

      rows = [member("lead", "idle"), member("architect0", "waiting", "sleep"),
        member("implementer0", "working"), member("reviewer0", "waiting", "subagents"),
        member("worker0", "unclassified"), member("blocked0", "waiting", "approval"),
        member("answer0", "waiting", "userInput"), member("removed0", "working", "", "removed")];
      await refresh();
      await expect(label).toHaveText("Other team members are working: architect0 (sleeping), implementer0, reviewer0 (waiting for team), and 1 other.");
      const fullStatus = await label.textContent();
      await assertCompactStatus();

      // Completed-plan decisions retain the peer status, including stale data.
      planDecision = true;
      await page.request.post(baseURL + "/fixture/refresh");
      await refresh();
      await expect(page.locator("#plan-actions")).toBeVisible();
      await expect(label).toHaveText(fullStatus);
      await assertCompactStatus();
      failTeam = true;
      await refresh();
      await expect(label).toHaveText("Team activity update unavailable.");
      await expect(work).toBeVisible();
      failTeam = false;
      const activeRows = rows;
      rows = [member("lead", "idle")];
      await refresh();
      await expect(work).toBeHidden();
      rows = activeRows;
      await refresh();
      await expect(work).toBeVisible();
      planDecision = false;
      await page.request.post(baseURL + "/fixture/refresh");
      await refresh();
      await expect(page.locator("#plan-actions")).toBeHidden();

      // A peer summary remains visible even before idle timing has an origin.
      rows[0].snapshot.stateSinceMs = 0;
      await refresh();
      await expect(label).toHaveText(fullStatus);
      await expect(work).toBeVisible();
      rows[0].snapshot.stateSinceMs = Date.now() - 1000;
      await refresh();

      // Automatic waits retain their own explanation first on the same line.
      rows[0] = member("lead", "waiting", "sleep");
      await refresh();
      await expect(label).toHaveText(`Sleeping · wakes automatically · ${fullStatus}`);
      await assertCompactStatus();
      rows[0] = member("lead", "waiting", "subagents");
      await refresh();
      await expect(label).toHaveText(`Waiting for team members · resumes automatically · ${fullStatus}`);
      await assertCompactStatus();
      rows[0] = member("lead", "idle");
      await refresh();

      // Losing a read invalidates the current-work claim without inviting input.
      failTeam = true;
      await refresh();
      await expect(label).toHaveText("Team activity update unavailable.");
      await expect(dot).toBeHidden();
      failTeam = false;
      await refresh();
      await expect(label).toContainText("architect0 (sleeping)");

      // A slow read cannot make an old observation remain current indefinitely.
      holdTeam = true;
      const beforeHold = teamReads;
      await refresh();
      await expect.poll(() => teamReads).toBeGreaterThan(beforeHold);
      await page.evaluate(() => { window.teamWallNow = Date.now; Date.now = () => window.teamWallNow() + 16000; });
      await expect(label).toHaveText("Team activity update unavailable.");
      holdTeam = false;
      releaseTeamRead();
      await expect.poll(() => heldReadFinished).toBe(true);
      await expect(label).toHaveText("Team activity update unavailable.");
      await page.evaluate(() => { Date.now = window.teamWallNow; });
      await refresh();
      await expect(label).toContainText("architect0 (sleeping)");

      // Authoritative roster removal clears cached work, including late replies.
      holdTeam = true; heldReadFinished = false;
      const beforeRemoval = teamReads;
      await refresh();
      await expect.poll(() => teamReads).toBeGreaterThan(beforeRemoval);
      addresses = [];
      await refresh();
      await expect(page.locator("#codex-member option")).toHaveCount(1);
      await expect(page.locator("#codex-team-work")).toHaveCount(0);
      await expect(label).toHaveText("Idle · waiting for instructions");
      holdTeam = false;
      releaseTeamRead();
      await expect.poll(() => heldReadFinished).toBe(true);
      await expect(page.locator("#codex-team-work")).toHaveCount(0);
      addresses = ["architect0", "implementer0", "reviewer0", "worker0"];
      await refresh();
      await expect(label).toContainText("architect0 (sleeping)");

      // The real member selector navigates to the selected conversation.
      rows = [member("lead", "working"), member("implementer0", "idle")];
      await page.locator("#codex-member").selectOption("implementer0");
      await expect(label).toHaveText("Other team members are working: lead.");
      rows = [member("lead", "working"), member("implementer0", "waiting", "approval")];
      await refresh();
      await expect(label).toHaveText("Waiting for approval · Other team members are working: lead.");
      await assertCompactStatus();
      rows = [member("lead", "working"), member("implementer0", "waiting", "userInput")];
      await refresh();
      await expect(label).toHaveText("Waiting for your answer · Other team members are working: lead.");
      await assertCompactStatus();

      // Completion restores ordinary idle presentation.
      rows = [member("lead", "idle"), member("implementer0", "idle")];
      await refresh();
      await expect(page.locator("#codex-team-work")).toHaveCount(0);
      await expect(label).toHaveText("Idle · waiting for instructions");

      const beforeHidden = teamReads;
      await page.evaluate(() => {
        Object.defineProperty(document, "hidden", {configurable: true, get: () => true});
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await page.waitForTimeout(5100);
      assert.equal(teamReads, beforeHidden, "hidden page must stop team reads");
      rows = [member("lead", "working"), member("implementer0", "idle")];
      await page.evaluate(() => {
        Object.defineProperty(document, "hidden", {configurable: true, get: () => false});
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await expect(label).toHaveText("Other team members are working: lead.");
      assert.deepEqual(errors, []);
    } finally {
      releaseTeamRead();
      await browser.close();
    }
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
