"use strict";
const assert = require("node:assert/strict");
const {chromium, expect} = require("@playwright/test");
const baseURL = process.argv[2];
const scope = "pi-workflow-v1:" + "a".repeat(64);
const slug = "2026-11-14-transcript-fixture";
const original = "CREATION_PI_ORIGINAL_pi-solo", reply = "CREATION_REPLY_pi";
const escape = text => text.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;").replaceAll('"', "&#34;").replaceAll("'", "&#39;");
const timestamp = "2026-11-14T12:34:56Z";
const item = (kind, itemId, text, html = escape(text)) => ({kind, itemId, turnId: "fixture-turn", text, html, timestamp, turnStatus: "completed"});
(async () => {
  console.log("FIXTURE_ORIGIN " + baseURL);
  const browser = await chromium.launch({headless: true, channel: "chromium"});
  const totals = {mutations: 0, reconciliations: {pi: 0, codex: 0}, requests: {}, pages: 0};
  try {
    const run = async ({pi = true, entries, check}) => {
      const page = await browser.newPage({ignoreHTTPSErrors: true});
      const errors = [], unexpected = [];
      let reads = 0, requestEvidenceCount = 0;
      assert.equal(page.context().serviceWorkers().length, 0, "fresh nonpersistent context");
      const payload = () => ({threadId: pi ? "fixture-native-thread" : "thread-1", latestTurnId: "fixture-turn", status: "idle",
        collaborationMode: "default", model: "gpt-4o-mini", reasoningEffort: "off", entries, hasOlder: false, olderCursor: null});
      await page.addInitScript(({pi, slug, scope}) => {
        window.fixtureFreshState = {localStorageKeys: Object.keys(localStorage), notificationPermission: globalThis.Notification?.permission};
        window.fixtureEvents = [];
        window.EventSource = class {
          constructor(url) {
            this.url = url; this.listeners = {};
            const parsed = new URL(url, location.origin);
            if (pi && (parsed.pathname !== `/codex/conversations/${slug}/events` || parsed.searchParams.getAll("conversationScope").join(",") !== scope)) throw new Error("fixture SSE scope contract: " + parsed.pathname);
            window.fixtureEvents.push(this); queueMicrotask(() => this.onopen?.());
          }
          addEventListener(name, fn) { (this.listeners[name] ||= []).push(fn); }
          removeEventListener() {}
          close() {}
        };
      }, {pi, slug, scope});
      page.on("pageerror", error => errors.push(error.message));
      page.on("console", message => { if (message.type() === "error") errors.push(message.text()); });
      await page.route("**/*", async route => {
        const request = route.request(), url = new URL(request.url());
        const path = url.pathname;
        const header = request.headers()["x-workspace-conversation-scope"];
        if (requestEvidenceCount++ < 80) console.log("FIXTURE_REQUEST " + JSON.stringify({pi, path, method: request.method(), scopePresent: header !== undefined, scopeMatches: header === scope}));
        if (url.origin !== new URL(baseURL).origin) { unexpected.push("external:" + url.origin); return route.abort(); }
        if (path === "/favicon.ico") return route.fulfill({status: 204});
        const sessionBase = `/api/sessions/${pi ? slug : "example"}`;
        const conversationBase = pi ? `/codex/conversations/${slug}` : sessionBase;
        // The pinned client POSTs an empty queue reconciliation at startup.
        // This response is a controlled fixture protocol, not a production read.
        if (request.method() === "POST" && path === `${conversationBase}/queue/reconcile`) {
          totals.reconciliations[pi ? "pi" : "codex"]++;
          try {
            assert.equal(header, pi ? scope : undefined, "reconciliation scope: " + path);
            assert.equal(request.postData(), "{}", "pinned reconciliation body");
            assert.equal(request.headers()["content-type"], "application/json", "pinned reconciliation content type");
            assert.equal(url.search, "", "pinned reconciliation path");
            return route.fulfill({json: {ok: true}});
          } catch (error) {
            unexpected.push(path + ": " + error.message);
            return route.fulfill({status: 500, json: {error: "fixture reconciliation contract"}});
          }
        }
        if (request.method() !== "GET") { totals.mutations++; unexpected.push(request.method() + " " + path); return route.fulfill({status: 405, json: {error: "fixture rejects mutations"}}); }
        if (path.startsWith("/static/") || path.startsWith("/codex/assets/") || path === "/fixture/pi-transcript" || path === "/example/") return route.continue();
        totals.requests[path] = (totals.requests[path] || 0) + 1;
        try {
          const relative = path.startsWith(sessionBase + "/") ? path.slice(sessionBase.length + 1) : null;
          const conversationOperation = path.startsWith(conversationBase + "/") ? path.slice(conversationBase.length + 1) : null;
          const conversationReads = ["thread", "thread/page", "pending", "queue", "activity", "retry/activity", "models", "collaboration-modes"];
          const workflowRead = relative === "pi-questions" || relative === "pi-plan";
          const scopedSessionRead = ["details", "auto-archive", "directory", "file", "artifact-preview", "operation"].includes(relative);
          const genericSessionRead = ["attention", "coordination", "communication", "progress", "retry-policy"].includes(relative);
          const conversationRead = conversationReads.includes(conversationOperation);
          const scoped = pi && (conversationRead || workflowRead || scopedSessionRead || path === "/api/collaboration-modes");
          const genericGlobalRead = ["/api/models", "/codex/models", "/api/codex-limits", "/api/browser-notifications", "/api/codex-providers"].includes(path);
          const uploadListing = !pi && path === "/uploads/s-example";
          assert.ok(conversationRead || workflowRead || scopedSessionRead || genericSessionRead || genericGlobalRead || uploadListing || path === "/api/collaboration-modes", "unaccounted path: " + path);
          assert.equal(header, scoped ? scope : undefined, "scope contract: " + path);
          if (pi && conversationRead) {
            assert.equal(decodeURIComponent(path.split("/")[3]), slug, "actual selected conversation URL");
            assert.equal(url.searchParams.has("conversationScope"), false, "fetch scope belongs in header");
          }
          if (pi && (workflowRead || relative === "details")) assert.deepEqual(url.searchParams.getAll("member"), ["lead"], "selected member: " + path);
          const json = data => route.fulfill({json: data});
          if (conversationOperation === "thread" || conversationOperation === "thread/page") { reads++; return json(payload()); }
          if (conversationOperation === "pending" || conversationOperation === "queue") return json([]);
          if (relative === "pi-questions") return json({revision: 0, questions: []});
          if (relative === "pi-plan") return json({revision: 0, proposals: [], acceptances: []});
          if (relative === "details") return json({repositoriesHTML: "", artifactsHTML: "", sessionBackend: pi ? "pi" : "codex", conversationScope: pi ? scope : "", conversationInteractive: true, conversationHeld: false, readyMembers: []});
          if (conversationOperation === "retry/activity") return json({serverNowMs: Date.now(), activity: null});
          if (conversationOperation === "activity") return json({currentState: "idle", observedAtMs: Date.now(), stateSinceMs: Date.now(), coverageComplete: true});
          if (path === "/api/collaboration-modes" || conversationOperation === "collaboration-modes") return json([{mode: "default", name: "Default"}, {mode: "plan", name: "Plan"}]);
          if (path === "/api/models" || path === "/codex/models" || conversationOperation === "models") return json([{id: "gpt-4o-mini", model: "gpt-4o-mini", displayName: "gpt-4o-mini", supportedReasoningEfforts: [{reasoningEffort: "off"}], defaultReasoningEffort: "off"}]);
          if (uploadListing) return json({files: [], limits: {chunkBytes: 1024, fileBytes: 1024, promptBytes: 2048, files: 1}});
          if (relative === "retry-policy") return json({revision: "fixture", capacity: false, cybersecurity: false});
          if (relative === "auto-archive") return json({enabled: false, hold: true, blockers: []});
          if (relative === "progress") return json({available: false});
          if (path === "/api/codex-providers" || path === "/api/codex-limits") {
            assert.equal(request.postData(), null, "catalog read has no body");
            const query = [...url.searchParams];
            const sessionQuery = JSON.stringify(query) === JSON.stringify([["session", "example"]]);
            const providerQuery = JSON.stringify(query) === JSON.stringify([["provider", "openai"]]);
            assert.ok(query.length === 0 || sessionQuery || path === "/api/codex-limits" && providerQuery, "catalog query: " + path);
            const provider = {id: "openai", label: "Subscription", kind: "subscription"};
            if (path === "/api/codex-providers") return json({providers: [provider], active: "openai", default: "openai", inherited: true, ...(sessionQuery ? {session: "example"} : {})});
            return json({provider, windows: [], updatedAt: timestamp, canReset: false});
          }
          if (path === "/api/browser-notifications") {
            assert.deepEqual(url.searchParams.getAll("registrationId"), [""], "fresh notification registration");
            return json({version: 1, deliveryEpoch: 1, publicKey: "", registered: false, result: "", available: false});
          }
          if (relative === "operation") return json({state: "idle", options: {}});
          if (relative === "attention") return json({version: 1, identity: await page.locator("body").getAttribute("data-session-observation"), rootThread: pi ? "" : "thread-1", enabled: false, revision: 0, event: null, fresh: true});
          if (relative === "coordination") return json({workspaces: [], reservationHolders: [], jobs: []});
          if (relative === "communication") return json({candidates: [], grants: [], messages: []});
          throw new Error("unhandled fixture response: " + path);
        } catch (error) {
          unexpected.push(path + ": " + error.message);
          console.error("FIXTURE_ROUTE_ERROR " + JSON.stringify({path, error: error.message}));
          return route.fulfill({status: 500, json: {error: "fixture contract failure"}});
        }
      });
      try {
        totals.pages++;
        await page.goto(baseURL + (pi ? "/fixture/pi-transcript" : "/example/"));
        const fresh = await page.evaluate(async () => ({...window.fixtureFreshState, registrations: navigator.serviceWorker ? (await navigator.serviceWorker.getRegistrations()).length : 0}));
        assert.deepEqual(fresh.localStorageKeys, []);
        assert.notEqual(fresh.notificationPermission, "granted");
        assert.equal(fresh.registrations, 0);
        await expect(page.locator("#transcript .message.user")).toHaveCount(entries.filter(e => e.kind === "userMessage").length, {timeout: 5000});
        await check(page, {errors, unexpected, payload, refresh: async () => {
          const before = reads;
          // An explicit transport invalidation uses the actual sync module's registered listeners.
          await page.evaluate(() => { for (const source of window.fixtureEvents) { const event = {data: JSON.stringify({type: "changed"})}; source.onmessage?.(event); } });
          await expect.poll(() => reads, {timeout: 5000}).toBeGreaterThan(before);
        }});
        assert.deepEqual(errors, [], "application errors");
        assert.deepEqual(unexpected, [], "unaccounted network");
        assert.equal(totals.mutations, 0);
      } finally { await page.close(); }
    };
    await run({entries: [item("userMessage", "fixture-user", original), item("agentMessage", "fixture-agent", reply)], check: async (page, context) => {
      const body = page.locator("body");
      await expect(body).toHaveAttribute("data-session-backend", "pi");
      await expect(body).toHaveAttribute("data-interactive", "true");
      await expect(body).toHaveAttribute("data-thread-id", "");
      await expect(body).toHaveAttribute("data-root-thread-id", "");
      await expect(body).toHaveAttribute("data-conversation-scope", scope);
      const row = page.locator("#transcript .message.user");
      await expect(row).toBeVisible({timeout: 5000});
      await expect(row).toHaveAttribute("data-transcript-entry-key", /fixture-user/);
      await expect(row.locator(".message-time")).toBeVisible({timeout: 5000});
      assert.ok((await row.locator(".message-time").innerText()).trim());
      assert.deepEqual(context.errors, []); assert.deepEqual(context.unexpected, []);
      const evidence = await row.evaluate(node => ({outerHTML: node.outerHTML.slice(0, 4096), children: [...node.childNodes].map(n => ({type: n.nodeType, text: n.textContent.slice(0, 256)})), footer: node.querySelector(".message-footer").textContent, backend: document.body.dataset.sessionBackend, scope: document.body.dataset.conversationScope}));
      evidence.exactCount = await page.locator("#transcript").getByText(original, {exact: true}).count();
      evidence.mutations = totals.mutations;
      evidence.payload = {threadId: context.payload().threadId, model: context.payload().model, reasoningEffort: context.payload().reasoningEffort};
      console.log("PRE_EXACT_ASSERTION " + JSON.stringify(evidence));
      // This is the unchanged acceptance locator; body and timestamp must already exist.
      await expect(page.locator("#transcript").getByText(original, {exact: true})).toBeVisible({timeout: 5000});
      for (const [text, role] of [[original, "user"], [reply, "agent"]]) {
        const exact = page.locator("#transcript").getByText(text, {exact: true});
        await expect(exact).toHaveCount(1);
        await expect(exact).toBeVisible();
        assert.equal(await exact.evaluate((n, expectedRole) => n.closest(".message").classList.contains(expectedRole), role), true);
        assert.equal(await exact.textContent(), text);
        await expect(exact).toHaveClass("message-text");
      }
      await expect(row.locator(".message-footer button")).toHaveCount(1);
      const footer = await row.locator(".message-footer").textContent();
      await context.refresh();
      await expect(row.locator(".message-text")).toHaveCount(1);
      await expect(row.locator(".message-text .message-text")).toHaveCount(0);
      assert.equal(await row.locator(".message-footer").textContent(), footer);
      await expect(page.locator("#transcript .message.user")).toHaveCount(1);
    }});
    await run({entries: [item("userMessage", "fallback-user", "Fallback original", ""), item("agentMessage", "fallback-agent", "Fallback reply", "")], check: async page => {
      for (const [text, role] of [["Fallback original", "user"], ["Fallback reply", "agent"]]) {
        const exact = page.locator("#transcript").getByText(text, {exact: true});
        await expect(exact).toHaveCount(1); await expect(exact).toBeVisible();
        assert.equal(await exact.textContent(), text);
        assert.equal(await exact.evaluate((n, expectedRole) => n.closest(".message").classList.contains(expectedRole), role), true);
        await expect(page.locator(`#transcript .message.${role} .message-time`)).toBeVisible();
      }
    }});
    const refreshed = [item("userMessage", "refresh-user", "Refresh original"), item("agentMessage", "refresh-agent", "Refresh reply")];
    await run({entries: refreshed, check: async (page, context) => {
      const row = page.locator("#transcript .message.user");
      const before = await row.locator(".message-time").getAttribute("datetime");
      refreshed[0].timestamp = "2026-11-14T12:35:56Z";
      await context.refresh();
      await expect(row.locator(".message-time")).not.toHaveAttribute("datetime", before);
      await expect(row.locator(".message-text")).toHaveCount(1);
      await expect(row.locator(".message-text .message-text")).toHaveCount(0);
      await expect(row.locator(".message-text")).toHaveText("Refresh original");
      await expect(page.locator("#transcript .message.user")).toHaveCount(1);
    }});
    await run({entries: [item("userMessage", "hidden-user", original)], check: async page => {
      const exact = page.locator("#transcript").getByText(original, {exact: true});
      await expect(exact).toHaveCount(1); await expect(exact).toBeVisible();
      await exact.evaluate(node => { node.closest(".message").hidden = true; });
      await expect(exact).not.toBeVisible();
    }});
    const literal = '<img src=x onerror="window.payloadExecuted=true">&';
    const whitespace = "  Příliš 🐈\n\tUnicode & literal  ";
    await run({entries: [item("userMessage", "literal-user", literal), item("agentMessage", "space-agent", whitespace, "")], check: async page => {
      assert.equal(await page.locator("#transcript .message.user .message-text").textContent(), literal);
      assert.equal(await page.locator("#transcript .message.agent .message-text").textContent(), whitespace);
      await expect(page.locator("#transcript img, #transcript script, #transcript [onerror]")).toHaveCount(0);
      assert.equal(await page.evaluate(() => window.payloadExecuted), undefined);
      await expect(page.locator("#transcript .message-time")).toHaveCount(2);
    }});
    await run({pi: false, entries: [item("userMessage", "rich-user", "Rich text", '<p>Rich <a href="#fixture">text</a></p><details><summary>More</summary><pre>detail</pre></details><table><tbody><tr><td>cell</td></tr></tbody></table>'), item("agentMessage", "rich-agent", "Rich reply", "<p>Rich reply</p>")], check: async page => {
      const row = page.locator("#transcript .message.user");
      await expect(row.locator(".message-text")).toHaveCount(0);
      await expect(row.locator(":scope > p > a")).toHaveText("text");
      await expect(row.locator(":scope > details > summary")).toHaveText("More");
      await expect(row.locator("table td")).toHaveText("cell");
      await expect(page.locator("#transcript").getByText("Rich reply", {exact: true})).toBeVisible();
      await expect(row.locator(".message-time")).toBeVisible();
    }});
    await run({entries: [item("userMessage", "prefix-user", "prefix " + original), item("agentMessage", "suffix-agent", original + " suffix")], check: async page => {
      await expect(page.locator("#transcript").getByText(original, {exact: true})).toHaveCount(0);
      const row = page.locator("#transcript .message.user");
      await row.evaluate(node => { node.hidden = true; });
      await expect(page.locator("#transcript").getByText("prefix " + original, {exact: true})).not.toBeVisible();
    }});
    await run({entries: [item("userMessage", "duplicate-a", original), item("userMessage", "duplicate-b", original)], check: async page => {
      await expect(page.locator("#transcript").getByText(original, {exact: true})).toHaveCount(2);
      // Unique-count assertions deliberately reject this input; no first() hides it.
      assert.notEqual(await page.locator("#transcript").getByText(original, {exact: true}).count(), 1);
    }});
    console.log("EXACT_VISIBILITY_GREEN " + JSON.stringify(totals));
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
