"use strict";
const assert = require("node:assert/strict");
const {chromium, expect} = require("@playwright/test");
const baseURL = process.argv[2];
const fs = require("node:fs/promises");
(async () => {
  // Chromium's download manager also needs to trust the disposable TLS server.
  const browser = await chromium.launch({headless: true, channel: "chromium", args: ["--ignore-certificate-errors"]});
  try {
    const context = await browser.newContext({ignoreHTTPSErrors: true, permissions: ["clipboard-read", "clipboard-write"], viewport: {width: 1440, height: 850}});
    const page = await context.newPage(), errors = [], dialogs = [], sends = [], additions = [];
    page.on("pageerror", error => errors.push(error.message));
    page.on("dialog", dialog => { dialogs.push(dialog.message()); void dialog.dismiss(); });
    let removed = false, detailsReads = 0, previewVersion = 0, releasePreview, releaseAdd, turns = 0;
    const transcript = await (await context.request.get(baseURL + "/fixture/presentation")).json();
    const fullTree = await (await context.request.get(baseURL + "/fixture/artifact-tree")).text();
    const smallerTree = await (await context.request.get(baseURL + "/fixture/artifact-tree?remove=1")).text();
    const teamHTML = (await (await context.request.get(baseURL + "/fixture/team")).text()).replace('<div id="team-role-defaults" hidden>', '<div id="team-role-defaults" hidden><span data-role="implementer" data-model="model-1" data-effort="high"></span>');
    await page.route("**/api/models**", route => route.fulfill({json: [{model: "model-1", displayName: "Fixture model", isDefault: true, defaultReasoningEffort: "low", supportedReasoningEfforts: [{reasoningEffort: "low"}, {reasoningEffort: "high"}]}]}));
    await page.route("**/api/collaboration-modes", route => route.fulfill({json: [{mode:"default", name:"Default"}]}));
    await page.route("**/api/sessions/example/**", async route => {
      const url = new URL(route.request().url()), operation = url.pathname.split("/").at(-1);
      if (["thread", "page"].includes(operation)) return route.fulfill({json: transcript});
      if (operation === "details") { detailsReads++; return route.fulfill({json: {artifactsHTML: (removed ? smallerTree : fullTree)+`<!--refresh ${detailsReads}-->`, artifactCount: removed ? 499 : 500, repositoriesHTML:"", teamHTML}}); }
      if (operation === "artifact-preview") {
        const version = ++previewVersion;
        if (url.searchParams.get("path").endsWith("499.md") && version === 2) await new Promise(resolve => {releasePreview = resolve;});
        return route.fulfill({json: {kind:"markdown", text: `# Raw ${version}\n`, html:`<h1>Rendered ${version}</h1>`}});
      }
      if (operation === "team" && route.request().method() === "POST") {
        additions.push(route.request().postDataJSON());
        if (additions.length === 1) await new Promise(resolve => {releaseAdd = resolve;});
        return route.fulfill({json:{ok:true}});
      }
      if (["queue", "pending"].includes(operation)) return route.fulfill({json:[]});
      if (operation === "team-stats") return route.fulfill({json:{members:[]}});
      return route.continue();
    });
    await page.goto(baseURL + "/example/");
    await expect(page.locator("#transcript h1")).toHaveText("User heading");
    await expect(page.locator("#transcript")).toContainText("fixture.txt");
    await expect(page.locator("#transcript")).not.toContainText("Generated attachment envelope");
    await expect(page.locator("#transcript")).not.toContainText("Internal work.");
    const citation = page.locator('#transcript a[href="https://example.test/source"]');
    await expect(citation).toHaveText("[1]");
    await expect(citation).toHaveAttribute("title", 'Český "title"');
    await expect(page.locator("#transcript code")).toContainText("\ue200cite\ue202ref\ue201");
    await citation.locator('xpath=ancestor::*[contains(concat(" ", normalize-space(@class), " "), " message ")][1]').getByRole("button", {name: "Copy message", exact: true}).click();
    assert((await page.evaluate(() => navigator.clipboard.readText())).includes("https://example.test/source"));
    await page.locator('[data-transcript-filter="team"]').click();
    await expect(page.locator("#transcript .message.team")).toContainText("lead → implementer0");
    await expect(page.locator("#transcript")).toContainText("Internal work.");
    await page.locator('[data-transcript-filter="messages"]').click();
    const link = page.locator('#transcript a').filter({hasText:"Plan"});
    await expect(link).toHaveAttribute("target", "_blank");
    await expect(link).toHaveAttribute("rel", /noopener/);
    const [popup] = await Promise.all([context.waitForEvent("page"), link.click({button:"middle"})]);
    await popup.waitForLoadState();
    await expect(popup.locator("#source-file-copy-content")).toBeEnabled();
    await popup.locator("#source-file-copy-content").click();
    const source = await (await context.request.get(baseURL + "/api/sessions/example/file?artifact=plan.md")).json();
    assert.equal(await popup.evaluate(() => navigator.clipboard.readText()), source.content.text);
    const [download] = await Promise.all([popup.waitForEvent("download"), popup.locator("#source-file-download").click()]);
    assert.equal(download.suggestedFilename(), "plan.md");
    assert.equal(await fs.readFile(await download.path(), "utf8"), source.content.text);
    await popup.close(); await page.bringToFront();

    await page.getByRole("tab", {name:/Artifacts/}).click();
    await expect(page.locator('[data-artifact-path]')).toHaveCount(500);
    const bottom = page.locator('[data-artifact-path="reports/deep/499.md"]');
    await bottom.click(); await expect.poll(() => Boolean(releasePreview)).toBe(true);
    await page.locator('[data-artifact-path="reports/deep/000.md"]').click();
    await bottom.click();
    await expect(page.locator("#artifact-preview h1")).toHaveText("Rendered 4");
    releasePreview(); await expect(page.locator("#artifact-preview h1")).toHaveText("Rendered 4");
    await page.locator("#artifact-copy").click();
    assert.equal(await page.evaluate(() => navigator.clipboard.readText()), "# Raw 4\n");
    const [artifactDownload] = await Promise.all([page.waitForEvent("download"), page.locator("#artifact-download").click()]);
    assert.equal(artifactDownload.url(), baseURL + "/artifacts/example/reports/deep/499.md");
    assert.equal(artifactDownload.suggestedFilename(), "499.md");
    assert.equal(await artifactDownload.failure(), null, `Artifact download failed: ${artifactDownload.url()}`);
    assert.equal(await fs.readFile(await artifactDownload.path(), "utf8"), "# Original artifact bytes\r\n");
    await page.locator('[data-artifact-directory="reports/deep"]').evaluate(node => {node.open = false;});
    await page.locator('[data-artifact-directory="reports/deep"]').evaluate(node => {node.open = true;});
    // Collapsing the only file directory can clamp navigation to zero. Set up
    // the position whose retention across status refresh is under test.
    await bottom.scrollIntoViewIfNeeded();
    const panePositions = () => page.evaluate(() => {
      const nav = document.querySelector(".artifact-list"), preview = document.querySelector(".artifact-view");
      return {nav:nav.scrollTop, navHeight:nav.clientHeight, navContent:nav.scrollHeight,
        preview:preview.scrollTop, panel:document.getElementById("artifacts").scrollTop, page:scrollY};
    });
    const beforeScroll = await panePositions();
    assert(beforeScroll.navContent > beforeScroll.navHeight, JSON.stringify(beforeScroll));
    await page.locator(".artifact-list").evaluate(node => {node.scrollTop -= 64;});
    const afterScroll = await panePositions(), scrollBefore = afterScroll.nav;
    assert(scrollBefore > 0 && scrollBefore < beforeScroll.nav, JSON.stringify({beforeScroll, afterScroll}));
    for (const key of ["preview", "panel", "page"]) assert.equal(afterScroll[key], beforeScroll[key], JSON.stringify({beforeScroll, afterScroll}));
    const directoriesBefore = await page.locator("[data-artifact-directory]").evaluateAll(nodes => nodes.map(node => [node.dataset.artifactDirectory, node.open]));
    const reads = detailsReads; await page.evaluate(() => dispatchEvent(new Event("focus")));
    await expect.poll(() => detailsReads).toBeGreaterThan(reads);
    await expect.poll(() => page.locator(".artifact-list").evaluate(node => Number(/<!--refresh (\d+)-->/.exec(node.innerHTML)?.[1] || 0))).toBeGreaterThan(reads);
    const afterRefresh = await panePositions();
    assert.equal(afterRefresh.nav, scrollBefore, JSON.stringify({afterScroll, afterRefresh}));
    for (const key of ["preview", "panel", "page"]) assert.equal(afterRefresh[key], afterScroll[key], JSON.stringify({afterScroll, afterRefresh}));
    assert.deepEqual(await page.locator("[data-artifact-directory]").evaluateAll(nodes => nodes.map(node => [node.dataset.artifactDirectory, node.open])), directoriesBefore);
    await expect(bottom).toHaveClass(/active/);
    const geometry = await page.evaluate(() => ({nav:getComputedStyle(document.querySelector('.artifact-list')).overflowY, preview:getComputedStyle(document.querySelector('.artifact-view')).overflowY}));
    assert.equal(geometry.nav, "auto"); assert.equal(geometry.preview, "auto");
    removed = true; await page.evaluate(() => dispatchEvent(new Event("focus")));
    await expect(page.locator('[data-artifact-path]')).toHaveCount(499);
    await expect(page.locator("#artifact-download")).toBeHidden(); await expect(page.locator("#artifact-copy")).toBeDisabled();
    await page.setViewportSize({width:390, height:844});
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    await page.setViewportSize({width:1440, height:850});

    await page.locator('[data-session-tab="team"]').click();
    const form = page.locator('[data-direct-team-form][data-action="add"]');
    await form.locator('[name="role"]').selectOption("implementer");
    await expect(form.locator('[name="effort"]')).toHaveValue("high");
    await form.evaluate(form => {form.requestSubmit(); form.requestSubmit();});
    await expect.poll(() => additions.length).toBe(1);
    await expect(form.getByRole("button", {name:"Add member"})).toBeDisabled();
    assert.deepEqual(additions[0], {action:"add", role:"implementer", model:"model-1", reasoningEffort:"high"});
    releaseAdd();
    await expect(form.getByRole("button", {name:"Add member"})).toBeEnabled();
    await expect(form.locator('[name="effort"]')).toHaveValue("high");
    await form.getByRole("button", {name:"Add member"}).click();
    await expect.poll(() => additions.length).toBe(2); assert.deepEqual(additions[1], additions[0]);
    await expect(form.locator('[name="effort"]')).toHaveValue("high");
    assert.deepEqual(dialogs, []); assert.deepEqual(errors, []);
  } finally { await browser.close(); }
})().catch(error => {console.error(error); process.exitCode = 1;});
