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
    await page.route("**/api/codex-providers*", route => route.fulfill({json: {default:"openai",active:"openai",providers:[
      {id:"openai",label:"Subscription",kind:"subscription"},{id:"pool",label:"Pool",kind:"codex-lb"}]}}));
    await page.route("**/api/models*", async route => {
      modelReads++;
      if (holdModels) await new Promise(resolve => { releaseModels = resolve; });
      if (failModels) return route.fulfill({status: 503, json: {error: "Models unavailable"}});
      return route.fulfill({json: [
        {model: "model-lead", displayName: "Lead model", isDefault: false, defaultReasoningEffort: "high",
          supportedReasoningEfforts: [{reasoningEffort: "high"}, {reasoningEffort: "xhigh"}]},
        {model: "model-solo", displayName: "Solo model", isDefault: true, defaultReasoningEffort: "medium",
          supportedReasoningEfforts: [{reasoningEffort: "medium"}, {reasoningEffort: "high"}]},
        ...(new URL(route.request().url()).searchParams.get("provider")==="pool" ? [{model:"pool-marker",displayName:"Pool marker",defaultReasoningEffort:"high",supportedReasoningEfforts:[{reasoningEffort:"high"}]}] : []),
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
          beforeOptions: Boolean(label.nextElementSibling?.querySelector("[name=browserNotificationsEnabled]")) && label.nextElementSibling?.nextElementSibling?.tagName === "DETAILS"};
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
    // The actual provider store must retain edits when async model loading and
    // draft restoration run, including across reload and equal-default pins.
    const provider=form.locator("[name=provider]");
    await expect(provider.locator("option[value='']")).toHaveText("Workspace default (Subscription)");
    await provider.selectOption("pool");
    await expect(model.locator("option[value=pool-marker]")).toHaveCount(1);
    await expect(provider).toHaveValue("pool");
    await expect.poll(()=>page.evaluate(key=>JSON.parse(sessionStorage.getItem(key)).provider,draftKey)).toBe("pool");
    await page.evaluate(()=>document.dispatchEvent(new CustomEvent("workspace-provider-changed")));
    await expect(model.locator("option[value=pool-marker]")).toHaveCount(1);
    await expect(provider).toHaveValue("pool");
    await page.reload({waitUntil:"domcontentloaded"});
    await form.locator("summary").click();
    await expect(provider).toHaveValue("pool");
    await expect(model.locator("option[value=pool-marker]")).toHaveCount(1);
    await provider.selectOption("openai");
    await expect.poll(()=>page.evaluate(key=>JSON.parse(sessionStorage.getItem(key)).provider,draftKey)).toBe("openai");
    await expect(model.locator("option[value=pool-marker]")).toHaveCount(0);
    await expect(provider).toHaveValue("openai");
    await provider.selectOption("");
    await expect.poll(()=>page.evaluate(key=>JSON.parse(sessionStorage.getItem(key)).provider,draftKey)).toBe("");
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
        model: "", effort: "", uploadScope: draft.scope?.id || "",
        ...(draft.sensitive === "true" ? {sensitive: "true"} : {})}).toString();
      sessionStorage.setItem(key, JSON.stringify(draft));
      return draft.body;
    }, draftKey);
    await page.reload({waitUntil: "domcontentloaded"});
    await expect.poll(() => retryBody).toBe(frozenBody);
    await expect(team).toBeDisabled();
    assert.equal(await page.evaluate(key => JSON.parse(sessionStorage.getItem(key)).body, draftKey), frozenBody);
    // A retained, rejected stale-catalog attempt becomes editable only after
    // the exact POST rejection, then preserves choices for explicit review.
    const catalogBodies = [];
    await page.route("**/sessions", route => {
      const body = route.request().postData(); catalogBodies.push(body);
      const fields = new URLSearchParams(body);
      if (catalogBodies.length === 1) return route.fulfill({status:409,json:{code:"preparation_catalog_stale",
        requestId:fields.get("clientRequestId"),catalogDigest:fields.get("catalogDigest"),error:"Catalog changed"}});
      return route.fulfill({status:503,json:{error:"Fixture keeps fresh request recoverable"}});
    });
    await page.evaluate(key => {
      const draft=JSON.parse(sessionStorage.getItem(key));
      Object.assign(draft,{name:"retained-choice",goal:" literal saved request ",team:"solo",model:"model-lead",effort:"xhigh",
        catalogDigest:"b".repeat(64),sensitive:"true",browserNotificationsEnabled:false,provider:"pool"});
      draft.body=new URLSearchParams({clientRequestId:draft.requestId,name:draft.name,goal:draft.goal,creation_date:draft.date,
        team:draft.team,catalogDigest:draft.catalogDigest,model:draft.model,effort:draft.effort,sensitive:"true",
        browserNotificationsEnabled:"false",provider:draft.provider,uploadScope:draft.scope?.id || ""}).toString();
      sessionStorage.setItem(key,JSON.stringify(draft));
    },draftKey);
    await page.reload({waitUntil:"domcontentloaded"});
    await expect.poll(()=>catalogBodies.length).toBe(1);
    await expect(form.locator("[data-team-catalog-changed]")).toBeVisible();
    await expect(team).toBeEnabled();await expect(team).toHaveValue("solo");
    await pair("model-lead","xhigh");await expect(sensitive).toBeChecked();
    await expect(form.locator("[name=browserNotificationsEnabled]")).not.toBeChecked();
    await expect(form.locator("[name=goal]")).toHaveValue(" literal saved request ");
    await expect(form.locator("[name=name]")).toHaveValue("retained-choice");
    const rejected=await page.evaluate(key=>JSON.parse(sessionStorage.getItem(key)),draftKey);
    assert.equal(rejected.body,null);assert.equal(rejected.catalogDigest,"b".repeat(64));
    await expect(form.locator("[type=submit]")).toBeDisabled();
    await form.locator("[data-team-catalog-acknowledge]").check();
    await form.locator("[type=submit]").click();
    await expect.poll(()=>catalogBodies.length).toBe(2);
    const retried=new URLSearchParams(catalogBodies[1]);
    assert.notEqual(retried.get("catalogDigest"),"b".repeat(64));
    assert.equal(retried.get("clientRequestId"),rejected.requestId);
    assert.equal(retried.get("provider"),"pool");
    await expect(provider).toHaveValue("pool");
    assert.equal(retried.get("team"),"solo");assert.equal(retried.get("model"),"model-lead");
    assert.equal(retried.get("effort"),"xhigh");assert.equal(retried.get("goal"),rejected.goal);
    // A removed saved team/model stays visible, but confirming new policy
    // cannot freeze invalid choices. The user must choose a supported team.
    catalogBodies.length=0;
    await page.evaluate(key=>{
      const draft=JSON.parse(sessionStorage.getItem(key));
      const fields=new URLSearchParams(draft.body);
      draft.catalogDigest="b".repeat(64);draft.team="removed-team";draft.model="removed-model";
      fields.set("catalogDigest",draft.catalogDigest);fields.set("team",draft.team);fields.set("model",draft.model);
      draft.body=fields.toString();sessionStorage.setItem(key,JSON.stringify(draft));
    },draftKey);
    await page.reload({waitUntil:"domcontentloaded"});
    await expect.poll(()=>catalogBodies.length).toBe(1);
    await expect(form.locator("[data-team-catalog-changed]")).toBeVisible();
    await expect(team).toHaveValue("removed-team");await expect(model).toHaveValue("removed-model");
    await form.locator("[data-team-catalog-acknowledge]").check();
    await expect(form.locator("[type=submit]")).toBeDisabled();
    assert.equal(catalogBodies.length,1);
    assert.equal(await page.evaluate(key=>JSON.parse(sessionStorage.getItem(key)).body,draftKey),null);
    await team.selectOption("solo");await pair("model-solo","high");
    await expect(form.locator("[type=submit]")).toBeEnabled();
    await form.locator("[type=submit]").click();
    await expect.poll(()=>catalogBodies.length).toBe(2);
    assert.equal(new URLSearchParams(catalogBodies[1]).get("team"),"solo");
    // Missing live discovery does not invalidate an exact saved override.
    catalogBodies.length=0;failModels=true;
    await page.evaluate(key=>{
      const draft=JSON.parse(sessionStorage.getItem(key));const fields=new URLSearchParams(draft.body);
      draft.catalogDigest="b".repeat(64);draft.team="solo";draft.model="exact-custom-model";draft.effort="xhigh";
      fields.set("catalogDigest",draft.catalogDigest);fields.set("team",draft.team);fields.set("model",draft.model);fields.set("effort",draft.effort);
      draft.body=fields.toString();sessionStorage.setItem(key,JSON.stringify(draft));
    },draftKey);
    await page.reload({waitUntil:"domcontentloaded"});
    await expect.poll(()=>catalogBodies.length).toBe(1);
    await expect(form.locator("[data-team-catalog-changed]")).toBeVisible();
    await pair("exact-custom-model","xhigh");await expect(team).toHaveValue("solo");
    await form.locator("[data-team-catalog-acknowledge]").check();
    await expect(form.locator("[type=submit]")).toBeEnabled();await form.locator("[type=submit]").click();
    await expect.poll(()=>catalogBodies.length).toBe(2);
    assert.equal(new URLSearchParams(catalogBodies[1]).get("model"),"exact-custom-model");
    assert.equal(new URLSearchParams(catalogBodies[1]).get("effort"),"xhigh");
    for (const width of [1280, 375]) {
      await page.setViewportSize({width, height: 900});
      for (const item of sessions) {
        await page.goto(`${baseURL}/fixture/session?sensitive=${item.sensitive}&archived=${item.archived}#settings`);
        await expect(page.locator("[data-session-sensitivity]")).toBeVisible();
        await expect(page.locator("[data-session-sensitivity]")).toHaveText(item.sensitive ? "Yes" : "No");
        assert.equal(await page.locator("#settings [name=sensitive]").count(), 0);
      }
    }
    // These pages have no /sessions route interception: submit the real form
    // through the production parser, validation and durable admission handler.
    for (const value of ["", "pool", "openai"]) {
      const actual=await browser.newPage({ignoreHTTPSErrors:true});
      actual.on("pageerror",error=>errors.push(error.message));
      await actual.route("**/api/models*",route=>route.fulfill({json:[{model:"model-solo",displayName:"Solo model",
        defaultReasoningEffort:"high",supportedReasoningEfforts:[{reasoningEffort:"high"}]}]}));
      await actual.goto(baseURL,{waitUntil:"domcontentloaded"});
      const realForm=actual.locator("#new-session-form");
      await realForm.locator("[name=team]").selectOption("solo");
      const realProvider=realForm.locator("[name=provider]");
      await expect(realProvider.locator("option[value=pool]")).toHaveCount(1);
      await realProvider.selectOption(value);
      await realForm.locator("[name=goal]").fill(`Unmocked provider admission: ${value || "inherited"}`);
      await expect(realForm.locator("[type=submit]")).toBeEnabled();
      const submitted=actual.waitForResponse(response=>new URL(response.url()).pathname==="/sessions" && response.request().method()==="POST");
      await realForm.locator("[type=submit]").click();
      const response=await submitted;
      assert.equal(response.status(),202);
      // Navigation can retire Chromium's POST response body before CDP reads
      // it. Recover the authoritative receipt using the actual submitted ID.
      const requestID=new URLSearchParams(response.request().postData()).get("clientRequestId");
      const persisted=await actual.request.get(`${baseURL}/api/session-creations/${requestID}`);
      assert.equal(persisted.status(),200);
      const status=await persisted.json();
      assert.equal(status.requestId,requestID);
      assert.equal(new URLSearchParams(response.request().postData()).get("provider"),value || null);
      await expect(actual).toHaveURL(`${baseURL}/creations/${status.requestId}/`);
      await actual.reload({waitUntil:"domcontentloaded"});
      await expect(actual.locator("body")).not.toContainText("unknown session request field");
      // Exercise the shipped renderer while real browser text is selected.
      // Identical status polls must preserve the selected text node.
      const failure="initialize session: error: direct team creation request is no longer current; start a new request";
      let polls=0;
      await actual.route(`**/api/session-creations/${status.requestId}`,route=>{
        polls++;
        return route.fulfill({json:{...status,state:"failed",phase:"initializing",error:failure}});
      });
      await actual.reload({waitUntil:"domcontentloaded"});
      await expect(actual.locator("#creation-error")).toHaveText(failure);
      await actual.evaluate(()=>{
        const node=document.getElementById("creation-error").firstChild;
        window.selectedErrorNode=node;
        const range=document.createRange();range.selectNodeContents(node);
        const selection=window.getSelection();selection.removeAllRanges();selection.addRange(range);
      });
      const initialPolls=polls;
      await expect.poll(()=>polls).toBeGreaterThanOrEqual(initialPolls+2);
      assert.equal(await actual.evaluate(()=>window.getSelection().toString()),failure);
      assert.equal(await actual.evaluate(()=>window.selectedErrorNode===document.getElementById("creation-error").firstChild),true);
      await actual.context().grantPermissions(["clipboard-read","clipboard-write"]);
      await actual.getByRole("button",{name:"Copy error",exact:true}).click();
      assert.equal(await actual.evaluate(()=>navigator.clipboard.readText()),failure);
      await actual.close();
    }
    assert.deepEqual(errors, []);
    console.log("Concrete creation settings, drafts and recovery passed");
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
