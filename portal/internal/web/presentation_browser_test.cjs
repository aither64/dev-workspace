"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
const baseURL = process.argv[2];
const head = "b".repeat(40), base = "a".repeat(40);
const cards = '<div class="repo-grid"><article class="panel repo-card" data-repository-id="project" data-repository-name="project" data-repository-head="' + head + '"><div data-repository-status>project</div><section class="repository-history"><div class="repository-review-actions"><button data-review-branch disabled>Compare</button><button data-review-refresh>Refresh commits</button></div><p class="repository-head-change" hidden></p><div data-repository-commits></div></section></article></div>';
(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: "chromium"} : {})});
    try {
      const page = await browser.newPage({ignoreHTTPSErrors: true, viewport: {width: 1440, height: 720}});
      const errors = [];
      const diagnostic = 'command failed with exit 1: /nix/store/example/bin/workspace-portal thread require-idle\nworkspace-portal: Codex thread thread-1 is not idle (latest turn turn-1 has status "inProgress")';
      let archive = {enabled: true, hold: false, tier: "merged", checked_at: "2026-09-14T18:01:59Z", eligible_at: "2026-09-21T18:01:59Z",
        blockers: ["Session has uncommitted worktree changes.", diagnostic]};
      let failArchive = false, failHold = false, failActivity = false;
      let threadStatus = "idle", activityState = "idle", pending = [];
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
          case "respond": return route.fulfill({json: {ok: true}});
          case "queue": return route.fulfill({json: []});
          case "reconcile": return route.fulfill({json: {ok: true}});
          case "activity": return route.fulfill(failActivity ? {status: 503, json: {error: "Fixture timing failure"}} : {json: {
            currentState: activityState, workingMs: 30000, waitingMs: 10000,
            stateSinceMs: Date.now() - 1000, observedAtMs: Date.now(), coverageComplete: true,
          }});
          case "details": return route.fulfill({json: {repositoriesHTML: cards, artifactsHTML: "", repositoryCount: 1, artifactCount: 0, clusterCount: 0}});
          case "repository-histories": return route.fulfill({json: {repositories: [{repository: "project", pair, review: "frozen", history: {commits: [], page: 0, hasMore: false}}]}});
          case "repository-states": return route.fulfill({json: {repositories: [{repository: "project", head}]}});
          case "repository-comparison": return route.fulfill({json: {pair, review: "frozen", files: [], stats: {files: 0}}});
          default: return route.continue();
        }
      });
      const sidebar = page.locator(".workspace-sidebar");
      const width = async value => expect.poll(async () => Math.round((await sidebar.boundingBox()).width)).toBe(value);
      const expireAutoArchiveCache = () => page.evaluate(() => {
        const wallNow = Date.now;
        Date.now = () => wallNow() + 31_000;
      });
      await page.goto(baseURL + "/example/");
      const codexTab = page.locator("#session-tab-codex");
      const waitingIndicator = page.locator("#codex-waiting-indicator");
      await expect(waitingIndicator).toBeVisible();
      await expect(codexTab).toHaveAttribute("aria-label", "Codex: waiting for instructions");
      await width(250);
      await page.getByRole("tab", {name: /^Repositories(?: \(\d+\))?$/}).click();
      await expect(waitingIndicator).toBeVisible();
      await width(250);
      await page.evaluate(() => dispatchEvent(new Event("pagehide")));
      await expect(waitingIndicator).toBeHidden();
      await page.reload();
      threadStatus = "active"; activityState = "waiting"; pending = [blockingPrompt];
      await page.reload();
      await expect(waitingIndicator).toBeVisible();
      await expect(codexTab).toHaveAttribute("aria-label", "Codex: waiting for instructions");
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
      await page.locator("[data-review-branch]").click();
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
      await expect(page.locator("#auto-archive-values")).toContainText("once all registered branches are merged");
      await expect(page.locator("#auto-archive-values li")).toHaveText(["The session has uncommitted worktree changes.", "Codex has an active turn."]);
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
      await expect(page.locator("#auto-archive-values")).toContainText("Not before");
      await expect(technical.locator("pre")).toContainText("Fixture read failure");
      failArchive = false;
      await page.getByRole("checkbox", {name: "Keep open", exact: true}).check();
      await expect(page.getByRole("checkbox", {name: "Keep open", exact: true})).toBeEnabled();
      await expect(page.locator("#auto-archive-values")).not.toContainText("Not before");
      await expect(technical).toBeHidden();
      failHold = true;
      await page.getByRole("checkbox", {name: "Keep open", exact: true}).uncheck();
      await expect(page.getByRole("checkbox", {name: "Keep open", exact: true})).toBeChecked();
      await expect(page.locator("#auto-archive-status")).toContainText("Could not confirm the Keep open change");
      await expect(technical.locator("pre")).toContainText("Fixture hold failure");
      for (const fixture of [{enabled: false, eligible_at: "2026-09-21T18:01:59Z"}, {}, {enabled: true, tier: "complete", result: "deferred", blockers: ["unknown <script>diagnostic</script>"]}]) {
        archive = fixture;
        await expireAutoArchiveCache();
        await page.getByRole("tab", {name: "Codex", exact: true}).click();
        await page.getByRole("tab", {name: "Settings", exact: true}).click();
        await expect(page.locator("#auto-archive-values")).toContainText("Waiting for the first scan");
        await expect(page.locator("#auto-archive-values")).not.toContainText("Not before");
      }
      await expect(technical.locator("pre")).toHaveText("unknown <script>diagnostic</script>");
      await expect(page.locator("#auto-archive-values")).toContainText("An archival check could not be completed.");
      await page.goto(baseURL + "/");
      await width(310);
      await page.setViewportSize({width: 600, height: 720}); await width(180);
      await limits.focus(); await page.keyboard.press("Enter");
      await expect(limits).toHaveAttribute("aria-expanded", "true");
      const limitsBox = await page.locator("#codex-limits-panel").boundingBox();
      assert(limitsBox.x >= 180 && limitsBox.x + limitsBox.width <= 600, "index limits popover escaped the viewport");
      await page.keyboard.press("Escape");
      await expect(limits).toHaveAttribute("aria-expanded", "false");
      await page.setViewportSize({width: 1440, height: 720}); await width(310);
      await expect(page.locator("#codex-limits-panel")).toBeVisible();
      assert.deepEqual(errors, []);
      console.log(engine.name() + ": comparison-only compact sidebar, limits, keyboard navigation and archival presentation passed");
    } finally { await browser.close(); }
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
