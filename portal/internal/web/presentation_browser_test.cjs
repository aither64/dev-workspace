"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
const baseURL = process.argv[2];
const head = "b".repeat(40), base = "a".repeat(40);
const workflowCounters = version => ["Total", "Queued", "Running", "Successful", "Failed"].map((label, index) => {
  const value = index === 0 || index === 3 ? version + 1 : 0;
  return `<span class="repository-workflow-counter ${label.toLowerCase()}" role="group" aria-label="${label} workflow runs: ${value}" title="Fixture ${label}">${label} <strong>${value}</strong></span>`;
}).join("");
const workflowMarkup = (version, mode, bodyVersion, runURL) => mode === "runs" ?
  `<details class="repository-workflows" data-repository-workflows><summary>Workflows <span class="repository-workflow-counters">${workflowCounters(version)}</span></summary><div data-repository-workflow-runs><a href="${runURL}">Fixture run ${bodyVersion}</a></div></details>` :
  `<p class="repository-workflows-compact" role="note" aria-label="Fixture ${mode}" title="Fixture ${mode}">Workflows · ${mode === "zero" ? "0 total" : "unavailable"}</p>`;
const cards = (version, mode, bodyVersion, runURL) => '<div class="repo-grid">' + ["project", "second"].map(name =>
  `<article class="panel repo-card" data-repository-id="${name}" data-repository-name="${name}" data-repository-head="${head}">` +
  `<div data-repository-status>${name} ${version}${workflowMarkup(version, mode, bodyVersion, runURL)}</div>` +
  '<div class="repository-review-actions"><button data-review-branch disabled>Compare</button><button data-review-worktree="staged">Staged changes</button><button data-review-worktree="unstaged">Unstaged changes</button><button data-review-refresh>Refresh commits</button></div>' +
  '<p class="repository-head-change" hidden></p><details class="repository-history"><summary>Local commits <span class="repository-history-summary muted" data-repository-history-summary>Loading totals…</span></summary><div data-repository-commits></div></details></article>'
).join("") + '</div>';
(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: "chromium"} : {})});
    let releaseReviewStyle = () => {};
    try {
      const page = await browser.newPage({ignoreHTTPSErrors: true, viewport: {width: 1440, height: 720}});
      const errors = [];
      const diagnostic = 'command failed with exit 1: /nix/store/example/bin/workspace-portal thread require-idle\nworkspace-portal: Codex thread thread-1 is not idle (latest turn turn-1 has status "inProgress")';
      let archive = {enabled: true, hold: false, tier: "merged", checked_at: "2026-09-14T18:01:59Z", eligible_at: "2026-09-21T18:01:59Z",
        blockers: ["Session has uncommitted worktree changes.", diagnostic]};
      let failArchive = false, failHold = false, failActivity = false, failCapture = false, repositoryVersion = 0, captureCount = 0;
      let workflowMode = "runs", workflowBodyVersion = 0;
      let workflowRunURL = "https://github.com/example/project/actions/runs/1";
      let reviewStyleHeld = true;
      await page.route(/\/static\/repository-review\.css\?v=3$/, async route => {
        if (reviewStyleHeld) await new Promise(resolve => { releaseReviewStyle = resolve; });
        await route.continue();
      });
      let threadStatus = "idle", activityState = "idle", automaticWait = "", pending = [];
      let workspaceArchiveReads = 0, indexReads = 0, indexProgressReads = 0, failWorkspaceArchive = false;
      await page.route("**/api/auto-archive", route => {
        workspaceArchiveReads++;
        return route.fulfill(failWorkspaceArchive ? {status: 503, json: {error: "Fixture overview failure"}} : {json: {
          policy: {enabled: false}, counts: {total: 3, held: 1}, last_scan: {checked_at: new Date().toISOString()},
          sessions: [
            {slug: "broken", repair_needed: true, diagnostics: [{category: "legacy_format", message: "Manifest is invalid."}]},
            {slug: "abandoned", lifecycle: "abandoned", hold: true},
            {slug: "unknown", activity_known: false},
          ],
        }});
      });
      const blockingPrompt = {
        id: "request-1", token: "token-1", method: "item/tool/requestUserInput", kind: "userInput",
        threadId: "thread-1", turnId: "turn-1", itemId: "item-1", authorityAvailable: true,
        isBlocking: true, questions: [{id: "question", header: "Question", question: "Continue?", options: [{label: "Continue"}]}],
      };
      page.on("pageerror", error => errors.push(error.message));
      await page.route("**/api/codex-limits", route => route.fulfill({json: {windows: [{windowDurationMins: 10080, usedPercent: 20}], updatedAt: Date.now()}}));
      await page.route("**/api/sessions/example/**", async route => {
        const operation = new URL(route.request().url()).pathname.split("/").at(-1);
        const pair = {base, head, baseLabel: "Merge base"};
        switch (operation) {
          case "auto-archive":
            if (route.request().method() === "POST") {
              if (failHold) return route.fulfill({status: 503, json: {error: "Fixture hold failure"}});
              archive = {...archive, hold: route.request().postDataJSON().hold, eligible_at: null, blockers: []};
              return route.fulfill({json: archive});
            }
            return route.fulfill(failArchive ? {status: 503, json: {error: "Fixture read failure"}} : {json: archive});
          case "thread": return route.fulfill({json: {threadId: "thread-1", latestTurnId: "turn-1", status: threadStatus, collaborationMode: "plan", model: "model-1", reasoningEffort: "medium", entries: []}});
          case "pending": return route.fulfill({json: pending});
          case "respond": activityState = "working"; return route.fulfill({json: {ok: true}});
          case "queue": return route.fulfill({json: []});
          case "reconcile": return route.fulfill({json: {ok: true}});
          case "activity": return route.fulfill(failActivity ? {status: 503, json: {error: "Fixture timing failure"}} : {json: {
            currentState: activityState, waitReason: automaticWait || (activityState === "waiting" && pending.length ? "userInput" : ""), workingMs: 30000, waitingMs: 10000,
            stateSinceMs: Date.now() - 1000, observedAtMs: Date.now(), coverageComplete: true,
          }});
          case "details": return route.fulfill({json: {repositoriesHTML: cards(repositoryVersion, workflowMode, workflowBodyVersion, workflowRunURL), artifactsHTML: "", repositoryCount: 2, artifactCount: 0, clusterCount: 0}});
          case "repository-histories": return route.fulfill({json: {repositories: ["project", "second"].map(repository => ({repository, pair, review: "frozen", snapshot: "history-" + repository, history: {commits: [], page: 0, hasMore: false}, summary: {commitCount: 0, stats: {files: 0, additions: 0, deletions: 0}}}))}});
          case "repository-states": return route.fulfill({json: {repositories: ["project", "second"].map(repository => ({repository, head}))}});
          case "repository-comparison": {
            if (route.request().method() === "POST") {
              if (failCapture) return route.fulfill({status: 503, json: {error: "Fixture capture failed"}});
              return route.fulfill({json: {snapshot: "staged-" + (++captureCount), kind: "staged", ephemeral: true}});
            }
            const snapshot = new URL(route.request().url()).searchParams.get("snapshot");
            return route.fulfill({json: snapshot ? {snapshot, kind: "staged", ephemeral: true, capturedAt: "2026-10-01T12:00:00Z", sourceHead: head, pair, files: [], stats: {files: 0}} : {pair, review: "frozen", files: [], stats: {files: 0}}});
          }
          default: return route.continue();
        }
      });
      const sidebar = page.locator(".workspace-sidebar");
      const width = async value => expect.poll(async () => Math.round((await sidebar.boundingBox()).width)).toBe(value);
      const repositoryLayout = () => page.evaluate(() => {
        const grid = document.querySelector("#repositories .repo-grid");
        const bounds = element => {
          const box = element.getBoundingClientRect();
          return {x: box.x, y: box.y, width: box.width, height: box.height, right: box.right};
        };
        return {grid: bounds(grid), viewport: innerWidth, cards: [...grid.querySelectorAll(".repo-card")].map(card => ({
          ...bounds(card), actions: [...card.querySelectorAll(".repository-review-actions > *")].map(action => Math.round(action.getBoundingClientRect().top)),
          actionMargin: getComputedStyle(card.querySelector(".repository-review-actions")).marginTop,
          actionGap: getComputedStyle(card.querySelector(".repository-review-actions")).gap,
          actionSeparation: card.querySelector(".repository-review-actions").getBoundingClientRect().top -
            card.querySelector("[data-repository-status]").getBoundingClientRect().bottom,
        }))};
      });
      const assertRepositoryLayout = (layout, desktop) => {
        assert.equal(layout.cards.length, 2);
        assert.ok(Math.abs(layout.cards[0].width - layout.grid.width) <= 2, "first card must span its grid row");
        assert.ok(Math.abs(layout.cards[1].width - layout.grid.width) <= 2, "second card must span its grid row");
        assert.ok(Math.abs(layout.cards[0].x - layout.cards[1].x) <= 2 && layout.cards[1].y >= layout.cards[0].y + layout.cards[0].height - 1,
          "repository cards must stack vertically");
        assert.ok(layout.cards.every(card => card.right <= layout.viewport + 1), "repository card must fit the viewport");
        for (const card of layout.cards) {
          assert.equal(card.actionMargin, "16px", "repository actions need 1rem top spacing");
          assert.equal(card.actionGap, "8px", "repository actions need .5rem gaps");
          assert.ok(card.actionSeparation >= 15, "repository actions overlap the status and workflow summary");
        }
        if (desktop) assert.equal(new Set(layout.cards[0].actions).size, 1,
          `desktop repository actions must share one row (card ${Math.round(layout.cards[0].width)}px, tops ${layout.cards[0].actions.join(", ")})`);
      };
      const expireAutoArchiveCache = () => page.evaluate(() => {
        const wallNow = Date.now;
        Date.now = () => wallNow() + 31_000;
      });
      await page.goto(baseURL + "/example/");
      const codexTab = page.locator("#session-tab-codex");
      const waitingIndicator = page.locator("#codex-waiting-indicator");
      await expect(waitingIndicator).toBeVisible();
      await expect(codexTab).toHaveAttribute("aria-label", "Codex: Idle · waiting for instructions");
      await width(250);
      await page.getByRole("tab", {name: /^Repositories(?: \(\d+\))?$/}).click();
      await expect(waitingIndicator).toBeVisible();
      await width(250);
      const repositoryCards = page.locator("#repositories .repo-card");
      await expect(repositoryCards).toHaveCount(2);
      const firstCard = repositoryCards.first(), history = firstCard.locator(".repository-history");
      const workflows = firstCard.locator("[data-repository-workflows]");
      await expect(history).not.toHaveAttribute("open");
      await expect(workflows).not.toHaveAttribute("open");
      await expect(history.locator("[data-repository-history-summary]")).toContainText("0 commits · 0 changed files");
      await expect(workflows.locator("summary [role='group']")).toHaveCount(5);
      await expect(firstCard.locator("[data-review-branch]")).toHaveAttribute("href", /review=frozen/);
      for (const label of ["Compare", "Staged changes", "Unstaged changes", "Refresh commits"]) {
        await expect(firstCard.locator(".repository-review-actions").getByText(label, {exact: true})).toBeVisible();
      }
      assertRepositoryLayout(await repositoryLayout(), true);
      reviewStyleHeld = false; releaseReviewStyle();
      await expect.poll(() => page.locator('link[data-repository-review-styles]').evaluate(link => Boolean(link.sheet))).toBe(true);
      assertRepositoryLayout(await repositoryLayout(), true);
      await page.setViewportSize({width: 600, height: 720});
      assertRepositoryLayout(await repositoryLayout(), false);
      await page.setViewportSize({width: 1440, height: 720});
      failCapture = true;
      await firstCard.locator('[data-review-worktree="staged"]').click();
      await expect(firstCard.locator("[data-worktree-capture-error]")).toHaveText("Fixture capture failed");
      await expect(firstCard.locator("[data-worktree-capture-error]")).toBeVisible();
      await expect(history).not.toHaveAttribute("open");
      failCapture = false;
      await firstCard.locator('[data-review-worktree="staged"]').click();
      await expect(page.locator("#repositories .repository-file-scroll > .empty")).toHaveText("No changes in this snapshot.");
      await expect(firstCard.locator("[data-worktree-capture-error]")).toHaveCount(0);
      await page.getByRole("button", {name: "← Repositories", exact: true}).click();
      await history.locator("summary").click();
      await expect(history).toHaveAttribute("open", "");
      await workflows.locator("summary").focus();
      await page.keyboard.press("Enter");
      await expect(workflows).toHaveAttribute("open", "");
      await page.keyboard.press("Space");
      await expect(workflows).not.toHaveAttribute("open");
      await page.keyboard.press("Space");
      await expect(workflows).toHaveAttribute("open", "");
      await page.evaluate(() => {
        window.retainedWorkflow = document.querySelector("#repositories [data-repository-workflows]");
        window.retainedWorkflowSummary = window.retainedWorkflow.querySelector("summary");
      });
      repositoryVersion++; workflowBodyVersion++;
      await expect.poll(async () => {
        await page.evaluate(() => dispatchEvent(new Event("focus")));
        return firstCard.locator("[data-repository-status]").textContent();
      }).toContain("project 1");
      await expect(history).toHaveAttribute("open", "");
      await expect(workflows).toHaveAttribute("open", "");
      await expect(workflows.locator("summary")).toBeFocused();
      await expect(workflows.locator('[aria-label="Total workflow runs: 2"]')).toBeVisible();
      assert(await page.evaluate(() => window.retainedWorkflow === document.querySelector("#repositories [data-repository-workflows]") &&
        window.retainedWorkflowSummary === window.retainedWorkflow.querySelector("summary")), "status refresh replaced workflow disclosure");
      const runLink = workflows.locator("[data-repository-workflow-runs] a");
      await runLink.focus();
      await expect(runLink).toBeFocused();
      await page.evaluate(() => {
        window.retainedRunLink = document.querySelector("#repositories [data-repository-workflow-runs] a");
      });
      repositoryVersion++;
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect(firstCard.locator("[data-repository-status]")).toContainText("project 2");
      assert(await page.evaluate(() => window.retainedRunLink === document.querySelector("#repositories [data-repository-workflow-runs] a") &&
        document.activeElement === window.retainedRunLink), "unchanged workflow run link lost identity or focus");
      repositoryVersion++; workflowBodyVersion++;
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect(firstCard.locator("[data-repository-status]")).toContainText("project 3");
      assert(await page.evaluate(() => {
        const refreshed = document.querySelector("#repositories [data-repository-workflow-runs] a");
        window.refreshedRunLink = refreshed;
        return refreshed !== window.retainedRunLink && refreshed.getAttribute("href") === window.retainedRunLink.getAttribute("href") &&
          document.activeElement === refreshed;
      }), "changed workflow run link did not restore focus by URL");
      repositoryVersion++; workflowBodyVersion++;
      workflowRunURL = "https://github.com/example/project/actions/runs/2";
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect(firstCard.locator("[data-repository-status]")).toContainText("project 4");
      assert(await page.evaluate(() => window.refreshedRunLink !== document.querySelector("#repositories [data-repository-workflow-runs] a") &&
        document.activeElement === window.retainedWorkflowSummary), "removed workflow run link did not focus the summary");
      await expect(workflows).toHaveAttribute("open", "");
      workflowMode = "unavailable";
      repositoryVersion++;
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect(firstCard.locator("[data-repository-status]")).toContainText("project 5");
      await expect(firstCard.locator(".repository-workflows-compact")).toHaveText("Workflows · unavailable");
      await expect(workflows).toHaveCount(0);
      await expect(history).toHaveAttribute("open", "");
      workflowMode = "zero";
      repositoryVersion++;
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect(firstCard.locator("[data-repository-status]")).toContainText("project 6");
      await expect(firstCard.locator(".repository-workflows-compact")).toHaveText("Workflows · 0 total");
      await expect(workflows).toHaveCount(0);
      workflowMode = "runs";
      repositoryVersion++;
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect(firstCard.locator("[data-repository-status]")).toContainText("project 7");
      await expect(workflows).not.toHaveAttribute("open");
      assert(await page.evaluate(() => window.retainedWorkflow !== document.querySelector("#repositories [data-repository-workflows]")),
        "workflow disclosure persisted through compact states");
      await expect(history).toHaveAttribute("open", "");
      await page.reload();
      await expect(page.locator("#repositories .repository-history").first()).not.toHaveAttribute("open");
      await expect(page.locator("#repositories [data-repository-workflows]").first()).not.toHaveAttribute("open");
      await page.evaluate(() => dispatchEvent(new Event("pagehide")));
      await expect(waitingIndicator).toBeHidden();
      await page.reload();
      threadStatus = "idle"; activityState = "waiting"; automaticWait = "sleep";
      await page.reload(); await expect(waitingIndicator).toBeHidden();
      await expect(page.locator("#codex-work-label")).toHaveText("Sleeping · wakes automatically");
      automaticWait = "subagents"; await page.reload();
      await expect(waitingIndicator).toBeHidden();
      await expect(page.locator("#codex-work-label")).toHaveText("Waiting for team members · resumes automatically");
      automaticWait = ""; threadStatus = "active"; activityState = "waiting"; pending = [blockingPrompt];
      await page.reload();
      await expect(waitingIndicator).toBeVisible();
      await expect(codexTab).toHaveAttribute("aria-label", "Codex: Waiting for your answer");
      await codexTab.click();
      await page.locator(".wizard-option").filter({hasText: "Continue"}).click();
      await page.getByRole("button", {name: "Submit answers"}).click();
      await expect(waitingIndicator).toBeHidden();
      await expect(codexTab).toHaveAttribute("aria-label", "Codex");
      // The pending endpoint intentionally remains stale. A confirmed response
      // must still remove the attention signal in this page immediately.
      await expect(page.locator('input[type="radio"][value="Continue"]')).toHaveCount(0);
      failActivity = true;
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect(waitingIndicator).toBeHidden();
      await expect(codexTab).toHaveAttribute("aria-label", "Codex");
      failActivity = false;
      pending = [{...blockingPrompt, isBlocking: false}];
      await page.reload();
      await expect(waitingIndicator).toBeHidden();
      await expect(codexTab).toHaveAttribute("aria-label", "Codex");
      activityState = "working"; pending = [];
      await page.reload();
      await expect(waitingIndicator).toBeHidden();
      await expect(codexTab).toHaveAttribute("aria-label", "Codex");
      await page.getByRole("tab", {name: /^Repositories(?: \(\d+\))?$/}).click();
      await page.locator("[data-review-branch]").first().click();
      await expect(page.locator(".repository-review-heading")).toBeVisible();
      await width(58);
      const limits = page.locator(".codex-limits-toggle");
      await limits.focus(); await page.keyboard.press("Enter");
      await expect(limits).toHaveAttribute("aria-expanded", "true");
      await page.keyboard.press("Escape");
      await expect(limits).toHaveAttribute("aria-expanded", "false");
      for (const name of ["Workspace", "Delete session"]) await expect(sidebar.getByRole(name === "Workspace" ? "link" : "button", {name, exact: true})).toBeVisible();
      await page.getByRole("tab", {name: "Settings", exact: true}).click();
      await width(250);
      await page.getByRole("tab", {name: /^Repositories(?: \(\d+\))?$/}).click();
      await width(58);
      await page.getByRole("button", {name: "← Repositories", exact: true}).click();
      await width(250);
      await page.goBack(); await width(58);
      await page.goForward(); await width(250);
      await page.setViewportSize({width: 600, height: 720}); await width(58);
      await page.getByRole("tab", {name: "Codex", exact: true}).click(); await width(58);
      await page.setViewportSize({width: 1440, height: 450}); await width(250);
      await page.goto(baseURL + "/example/?tab=repositories&repository=project&review=frozen&view=file");
      await width(58);
      await page.getByRole("tab", {name: /^Repositories(?: \(\d+\))?$/}).focus();
      await page.keyboard.press("End");
      await expect(page.getByRole("tab", {name: "Settings", exact: true})).toBeFocused();
      const historyLength = await page.evaluate(() => history.length);
      await page.keyboard.press("End");
      assert.equal(await page.evaluate(() => history.length), historyLength, "same-tab key added a history entry");
      await width(250);
      await expect(page.locator("#auto-archive-details dl")).toContainText("once all registered branches are merged");
      await expect(page.locator("#auto-archive-values")).toHaveText("The session has uncommitted worktree changes. Codex has an active turn.");
      const technical = page.locator("#auto-archive-details");
      await expect(technical).not.toHaveAttribute("open");
      await expect(technical.locator("pre")).toBeHidden();
      await technical.locator("summary").click();
      await expect(technical.locator("pre")).toHaveText(diagnostic);
      failArchive = true;
      await expireAutoArchiveCache();
      await page.getByRole("tab", {name: "Codex", exact: true}).click();
      await page.getByRole("tab", {name: "Settings", exact: true}).click();
      await expect(page.locator("#auto-archive-status")).toContainText("Showing the last available settings");
      await expect(page.locator("#auto-archive-details dl")).toContainText("Not before");
      await expect(technical.locator("pre")).toContainText("Fixture read failure");
      failArchive = false;
      await page.getByRole("checkbox", {name: "Keep open", exact: true}).check();
      await expect(page.getByRole("checkbox", {name: "Keep open", exact: true})).toBeEnabled();
      await expect(page.locator("#auto-archive-details dl")).not.toContainText("Not before");
      await expect(technical.locator("pre")).toHaveText("");
      const hold = page.getByRole("checkbox", {name: "Keep open", exact: true});
      await expect(hold).toBeChecked();
      failHold = true;
      const [failedHoldResponse] = await Promise.all([
        page.waitForResponse(response => new URL(response.url()).pathname === "/api/sessions/example/auto-archive" &&
          response.request().method() === "POST"),
        hold.click(),
      ]);
      assert.equal(failedHoldResponse.request().postDataJSON().hold, false);
      assert.equal(failedHoldResponse.status(), 503);
      await expect(hold).toBeChecked();
      await expect(page.locator("#auto-archive-status")).toContainText("Could not confirm the Keep open change");
      await expect(technical.locator("pre")).toContainText("Fixture hold failure");
      for (const fixture of [{enabled: false, eligible_at: "2026-09-21T18:01:59Z"}, {}, {enabled: true, tier: "complete", result: "deferred", blockers: ["unknown <script>diagnostic</script>"]}]) {
        archive = fixture;
        await expireAutoArchiveCache();
        await page.getByRole("tab", {name: "Codex", exact: true}).click();
        await page.getByRole("tab", {name: "Settings", exact: true}).click();
        await expect(page.locator("#auto-archive-details dl")).toContainText("Waiting for the first scan");
        await expect(page.locator("#auto-archive-details dl")).not.toContainText("Not before");
      }
      await expect(technical.locator("pre")).toHaveText("unknown <script>diagnostic</script>");
      await expect(page.locator("#auto-archive-values")).toContainText("An archival check could not be completed.");
      // An allocated journal ID is still pre-journal. A changed target must
      // reopen fresh confirmation in this document, without an automatic replay.
      const oldTarget = await page.locator("body").getAttribute("data-lifecycle-target-id");
      const receiptId = "a".repeat(64), allocatedJournalId = "b".repeat(64);
      const currentTarget = {targetIdentityVersion: 2, targetId: "c".repeat(64), slug: "example", threadId: "replacement-root", lifecycle: "active", archived: false};
      const confirmations = [];
      await page.route("**/api/sessions/example/operation", route => route.fulfill({json: confirmations.length ? {
        slug: "example", kind: "archive", state: "failed", receiptId, phase: "starting",
        code: confirmations.length === 1 ? "target_changed" : undefined,
        error: "Fixture refusal", currentTarget,
        options: {mode: "complete", journalId: allocatedJournalId, journalExpected: false, targetId: currentTarget.targetId},
      } : {state: "idle"}}));
      await page.route("**/api/sessions/example/archive", route => {
        confirmations.push(route.request().postDataJSON());
        return route.fulfill(confirmations.length === 1 ? {status: 409, json: {error: "Review the current session", code: "target_changed", currentTarget, receiptId}} :
          {status: 503, json: {error: "Fixture refusal after explicit confirmation"}});
      });
      await page.locator("#archive-session-open").click();
      await page.locator('#archive-session-form button[value="complete"]').click();
      await expect(page.locator("#archive-session-dialog")).toBeVisible();
      await expect(page.locator("[data-lifecycle-target-summary]")).toContainText("replacement-root");
      assert.equal(confirmations.length, 1, "changed pre-journal request replayed without confirmation");
      assert.equal(confirmations[0].targetId, oldTarget);
      await page.locator('#archive-session-form button[value="complete"]').click();
      await expect.poll(() => confirmations.length).toBe(2);
      assert.equal(confirmations[1].targetId, currentTarget.targetId);
      assert.equal(confirmations[1].receiptId, receiptId);
      await expect(page.locator("#archive-session-dialog")).not.toBeVisible();
      await page.goto(baseURL + "/");
      await width(310);
      await page.setViewportSize({width: 600, height: 720}); await width(180);
      await limits.focus(); await page.keyboard.press("Enter");
      await expect(limits).toHaveAttribute("aria-expanded", "true");
      const limitsBox = await page.locator("#codex-limits-dialog").boundingBox();
      assert(limitsBox.x >= 0 && limitsBox.x + limitsBox.width <= 600, "account limits dialog escaped the viewport");
      await page.keyboard.press("Escape");
      await expect(limits).toHaveAttribute("aria-expanded", "false");
      await page.setViewportSize({width: 1440, height: 720}); await width(310);
      await expect(page.locator("#codex-limits-panel")).toBeVisible();
      // Even fast operation polling on the index never reads the overview.
      await page.route(/\/api\/index-status(?:\?.*)?$/, route => {
        if (new URL(route.request().url()).searchParams.get("progress") === "1") indexProgressReads++; else indexReads++;
        // A cached timestamp older than the page must not trigger fast full scans.
        return route.fulfill({json: {sessions: [], authoritative: true, generatedAt: "2000-01-01T00:00:00Z",
          operations: [{slug: "example", kind: "archive", state: "running", phase: "prepared"}]}});
      });
      await page.clock.install();
      await page.goto(baseURL + "/");
      await expect.poll(() => indexReads).toBeGreaterThan(0);
      const firstIndexReads = indexReads;
      await page.clock.runFor(1500);
      await expect.poll(() => indexProgressReads).toBeGreaterThan(0);
      assert.equal(indexReads, firstIndexReads, "fast progress started a full status request");
      await page.clock.runFor(14_000);
      await expect.poll(() => indexReads).toBeGreaterThan(firstIndexReads);
      const visibleIndexReads = [indexReads, indexProgressReads];
      await page.evaluate(() => { Object.defineProperty(document, "hidden", {configurable: true, value: true}); document.dispatchEvent(new Event("visibilitychange")); });
      await page.clock.runFor(20_000);
      assert.deepEqual([indexReads, indexProgressReads], visibleIndexReads, "hidden index kept polling");
      await page.evaluate(() => { Object.defineProperty(document, "hidden", {configurable: true, value: false}); document.dispatchEvent(new Event("visibilitychange")); });
      assert.equal(workspaceArchiveReads, 0);
      await expect(page.locator("#workspace-auto-archive")).toHaveCount(0);
      await page.getByRole("button", {name: "Workspace menu", exact: true}).click();
      await page.getByRole("link", {name: "Automatic archival", exact: true}).click();
      await page.waitForURL("**/automatic-archival");
      await expect(page.locator("#workspace-auto-archive-status")).toContainText("disabled");
      await expect(page.locator("#workspace-auto-archive-status")).toContainText("Last scan:");
      await expect(page.locator("#workspace-auto-archive-rows a")).toHaveText(["abandoned", "broken", "unknown"]);
      await expect(page.locator("#workspace-auto-archive-rows p")).toContainText([
        "Manual archive required", "Needs metadata repair", "Activity not verified",
      ]);
      await expect(page.getByRole("link", {name: "broken", exact: true})).toHaveAttribute("href", "/broken/#settings");
      assert.equal(workspaceArchiveReads, 1);
      const overviewIndexReads = indexReads;
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      assert.equal(workspaceArchiveReads, 1);
      failWorkspaceArchive = true;
      await page.clock.runFor(30_000);
      await expect.poll(() => workspaceArchiveReads).toBeGreaterThan(1);
      await expect(page.locator("#workspace-auto-archive-status")).not.toContainText("unavailable");
      await page.clock.runFor(30_001);
      await expect(page.locator("#workspace-auto-archive-status")).toContainText("unavailable");
      await expect(page.locator("#workspace-auto-archive-rows a")).toHaveCount(3);
      assert.equal(indexReads, overviewIndexReads);
      const visibleReads = workspaceArchiveReads;
      await page.evaluate(() => {
        Object.defineProperty(document, "hidden", {configurable: true, value: true});
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await page.clock.runFor(30_000);
      assert.equal(workspaceArchiveReads, visibleReads);
      failWorkspaceArchive = false;
      await page.evaluate(() => {
        Object.defineProperty(document, "hidden", {configurable: true, value: false});
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await expect(page.locator("#workspace-auto-archive-status")).toContainText("disabled");
      await page.getByRole("link", {name: "Back to workspace", exact: true}).click();
      await page.waitForURL(baseURL + "/");
      const afterOverviewReads = workspaceArchiveReads;
      await page.clock.runFor(1500);
      assert.equal(workspaceArchiveReads, afterOverviewReads);
      assert.deepEqual(errors, []);
      console.log(engine.name() + ": comparison-only compact sidebar, limits, keyboard navigation and archival presentation passed");
    } finally { releaseReviewStyle(); await browser.close(); }
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
