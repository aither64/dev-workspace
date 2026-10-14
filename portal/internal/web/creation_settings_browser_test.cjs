"use strict";
const assert = require("node:assert/strict");
const {chromium, expect} = require("@playwright/test");
const {draftKey} = require("./static/preparation.js");
const baseURL = process.argv[2];

(async () => {
  const browser = await chromium.launch({headless: true, channel: "chromium"});
  try {
    const page = await browser.newPage({ignoreHTTPSErrors: true});
    const errors = [];
    let releaseModels, failModels = false, holdModels = true, modelReads = 0;
    page.on("pageerror", error => errors.push(error.message));
    await page.route("**/api/models", async route => {
      modelReads++;
      if (holdModels) await new Promise(resolve => { releaseModels = resolve; });
      if (failModels) return route.fulfill({status: 503, json: {error: "Models unavailable"}});
      return route.fulfill({json: [
        {model: "model-lead", displayName: "Lead model", isDefault: false, defaultReasoningEffort: "high",
          supportedReasoningEfforts: [{reasoningEffort: "high"}, {reasoningEffort: "xhigh"}]},
        {model: "model-solo", displayName: "Solo model", isDefault: true, defaultReasoningEffort: "medium",
          supportedReasoningEfforts: [{reasoningEffort: "medium"}, {reasoningEffort: "high"}]},
      ]});
    });
    await page.route("**/api/upload-drafts", route => route.fulfill({status: 503, json: {error: "Fixture has no files"}}));
    const sessions = [false, true].flatMap(archived => [false, true].map(sensitive => ({
      slug: `${archived ? "archived" : "active"}-${sensitive ? "sensitive" : "ordinary"}`,
      archived, sensitive, updatedAt: new Date().toISOString(), repositoryCount: 0, runningClusters: 0,
    })));
    await page.route("**/api/index-status*", route => route.fulfill({json: {
      sessions, operations: [], authoritative: true, generatedAt: new Date(Date.now() + 60000).toISOString(),
    }}));
    const form = page.locator("#new-session-form");
    const model = form.locator("[name=model]"), effort = form.locator("[name=effort]");
    const team = form.locator("[name=team]");
    const pair = async (m, e) => { await expect(model).toHaveValue(m); await expect(effort).toHaveValue(e); };
    const open = async () => {
      await page.goto(baseURL, {waitUntil: "domcontentloaded"});
      await form.locator("summary").click();
    };
    await open();
    await page.locator(".archive-list > summary").click();
    const sensitive = form.getByRole("checkbox", {name: "Sensitive session (records stay local)", exact: true});
    for (const width of [1280, 375]) {
      await page.setViewportSize({width, height: 900});
      const geometry = await sensitive.evaluate(input => {
        const label = input.closest("label"), range = document.createRange();
        range.selectNodeContents(label.lastChild);
        const box = input.getBoundingClientRect(), text = range.getBoundingClientRect();
        return {width: box.width, centerDelta: Math.abs(box.y + box.height / 2 - text.y - text.height / 2),
          afterTeam: Boolean(document.querySelector("[name=team]").compareDocumentPosition(input) & Node.DOCUMENT_POSITION_FOLLOWING),
          beforeOptions: label.nextElementSibling?.tagName === "DETAILS"};
      });
      assert.ok(geometry.width < 24 && geometry.centerDelta < 2, JSON.stringify(geometry));
      assert.ok(geometry.afterTeam && geometry.beforeOptions, JSON.stringify(geometry));
      for (const item of sessions) {
        const card = page.locator(`[data-session-list='${item.archived ? "archived" : "active"}'] [data-session-slug='${item.slug}']`);
        const badge = card.locator("[data-session-sensitive]");
        if (item.sensitive) {
          await expect(badge).toBeVisible();
          const sizing = await badge.evaluate(element => {
            const style = getComputedStyle(element), range = document.createRange();
            range.selectNodeContents(element);
            const decoration = [style.paddingLeft, style.paddingRight, style.borderLeftWidth, style.borderRightWidth]
              .reduce((total, value) => total + parseFloat(value), 0);
            return {actual: element.getBoundingClientRect().width, intrinsic: range.getBoundingClientRect().width + decoration};
          });
          assert.ok(Math.abs(sizing.actual - sizing.intrinsic) < 2, JSON.stringify(sizing));
        } else await expect(badge).toBeHidden();
      }
    }
    await sensitive.locator("..").click();
    await expect(sensitive).toBeChecked();
    await sensitive.focus();
    await page.keyboard.press("Space");
    await expect(sensitive).not.toBeChecked();
    await page.keyboard.press("Space");
    await expect(sensitive).toBeChecked();
    await expect.poll(() => Boolean(releaseModels)).toBe(true);
    await expect(team).toHaveValue("lead_reviewed");
    await pair("model-lead", "xhigh");
    await team.selectOption("solo");
    await pair("model-solo", "high");
    holdModels = false; releaseModels();
    await expect(model.locator("option")).toHaveCount(2);
    await pair("model-solo", "high");
    await model.selectOption("model-lead");
    await pair("model-lead", "high");
    await effort.selectOption("xhigh");
    await pair("model-lead", "xhigh");
    await page.reload({waitUntil: "domcontentloaded"});
    await form.locator("summary").click();
    await expect(sensitive).toBeChecked();
    await pair("model-lead", "xhigh");
    await expect(team).toHaveValue("solo");
    await team.selectOption("lead_reviewed");
    await pair("model-lead", "xhigh");
    await team.selectOption("solo");
    await pair("model-solo", "high");
    assert.equal(await model.locator("option[value='']").count(), 0);
    assert.equal(await effort.locator("option[value='']").count(), 0);

    // An unsent predecessor draft used an empty pair to mean the team's defaults.
    await page.evaluate(key => {
      const draft = JSON.parse(sessionStorage.getItem(key));
      draft.model = ""; draft.effort = "";
      sessionStorage.setItem(key, JSON.stringify(draft));
    }, draftKey);
    failModels = true;
    const readsBefore = modelReads;
    await page.reload({waitUntil: "domcontentloaded"});
    await form.locator("summary").click();
    await expect.poll(() => modelReads).toBeGreaterThan(readsBefore);
    await pair("model-solo", "high");
    await team.selectOption("lead_reviewed");
    await pair("model-lead", "xhigh");

    // A catalog change still needs acknowledgement before an unsent request.
    await page.evaluate(key => {
      const draft = JSON.parse(sessionStorage.getItem(key));
      draft.catalogDigest = "b".repeat(64);
      sessionStorage.setItem(key, JSON.stringify(draft));
    }, draftKey);
    await page.reload({waitUntil: "domcontentloaded"});
    await expect(form.locator("[data-team-catalog-changed]")).toBeVisible();
    await pair("model-lead", "xhigh");
    await expect(form.locator("[type=submit]")).toBeDisabled();
    await form.locator("[data-team-catalog-acknowledge]").check();

    // Submitted requests retain their exact body, even with empty legacy settings.
    let retryBody;
    await page.route("**/api/session-creations/*", route => route.fulfill({status: 404, json: {error: "Not found"}}));
    await page.route("**/sessions", route => {
      retryBody = route.request().postData();
      return route.fulfill({status: 503, json: {error: "Fixture retry remains pending"}});
    });
    const frozenBody = await page.evaluate(key => {
      const draft = JSON.parse(sessionStorage.getItem(key));
      draft.model = ""; draft.effort = "";
      draft.body = new URLSearchParams({clientRequestId: draft.requestId, name: draft.name,
        goal: draft.goal, creation_date: draft.date, team: draft.team, catalogDigest: draft.catalogDigest,
        model: "", effort: "", uploadScope: draft.scope?.id || ""}).toString();
      sessionStorage.setItem(key, JSON.stringify(draft));
      return draft.body;
    }, draftKey);
    await page.reload({waitUntil: "domcontentloaded"});
    await expect.poll(() => retryBody).toBe(frozenBody);
    await expect(team).toBeDisabled();
    assert.equal(await page.evaluate(key => JSON.parse(sessionStorage.getItem(key)).body, draftKey), frozenBody);
    for (const width of [1280, 375]) {
      await page.setViewportSize({width, height: 900});
      for (const item of sessions) {
        await page.goto(`${baseURL}/fixture/session?sensitive=${item.sensitive}&archived=${item.archived}#settings`);
        await expect(page.locator("[data-session-sensitivity]")).toBeVisible();
        await expect(page.locator("[data-session-sensitivity]")).toHaveText(item.sensitive ? "Yes" : "No");
        assert.equal(await page.locator("#settings [name=sensitive]").count(), 0);
      }
    }
    assert.deepEqual(errors, []);
    console.log("Concrete creation settings, drafts and recovery passed");
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
