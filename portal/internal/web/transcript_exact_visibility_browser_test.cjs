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
    const run = async ({pi = true, entries, readPage, check}) => {
      const page = await browser.newPage({ignoreHTTPSErrors: true});
      const errors = [], unexpected = [];
      let reads = 0, requestEvidenceCount = 0, expectedReadErrors = [];
      assert.equal(page.context().serviceWorkers().length, 0, "fresh nonpersistent context");
      const payload = cursor => ({threadId: pi ? "fixture-native-thread" : "thread-1", latestTurnId: "fixture-turn", status: "idle",
        collaborationMode: "default", model: "gpt-4o-mini", reasoningEffort: "off", entries, hasOlder: false, olderCursor: null, ...(readPage?.(cursor) || {})});
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
      page.on("pageerror", error => {errors.push(error.message); console.error("FIXTURE_PAGE_ERROR " + JSON.stringify({message:error.message}));});
      page.on("console", message => {
        if (message.type() !== "error") return;
        const status=/Failed to load resource:.*status of (\d+)/.exec(message.text())?.[1];
        const expected=status ? expectedReadErrors.indexOf(Number(status)) : -1;
        if (expected >= 0) { expectedReadErrors.splice(expected,1); return; }
        errors.push(message.text());
      });
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
          const conversationReads = ["thread", "thread/page", "pending", "queue", "activity", "retry/activity", "models", "collaboration-modes", "provider"];
          const workflowRead = relative === "pi-questions" || relative === "pi-plan";
          const scopedSessionRead = ["details", "auto-archive", "directory", "file", "artifact-preview", "operation"].includes(relative);
          const genericSessionRead = ["attention", "coordination", "communication", "progress", "retry-policy"].includes(relative);
          const conversationRead = conversationReads.includes(conversationOperation);
          const uploadListing = path === `/uploads/s-${pi ? slug : "example"}`;
          const scoped = pi && (conversationRead || workflowRead || scopedSessionRead || uploadListing || path === "/api/collaboration-modes");
          const genericGlobalRead = ["/api/models", "/codex/models", "/api/codex-limits", "/api/browser-notifications", "/api/codex-providers"].includes(path);
          assert.ok(conversationRead || workflowRead || scopedSessionRead || genericSessionRead || genericGlobalRead || uploadListing || path === "/api/collaboration-modes", "unaccounted path: " + path);
          const expectedScope = relative === "communication" ? (pi ? scope : await page.locator("body").getAttribute("data-session-observation")) : scoped ? scope : undefined;
          assert.equal(header, expectedScope, "scope contract: " + path);
          if (pi && conversationRead) {
            assert.equal(decodeURIComponent(path.split("/")[3]), slug, "actual selected conversation URL");
            assert.equal(url.searchParams.has("conversationScope"), false, "fetch scope belongs in header");
          }
          if (pi && (workflowRead || relative === "details")) assert.deepEqual(url.searchParams.getAll("member"), ["lead"], "selected member: " + path);
          if (relative === "details") assert.equal(request.headers()["x-session-observation"], await page.locator("body").getAttribute("data-session-observation"), "details observation");
          const json = data => route.fulfill({json: data});
          if (conversationOperation === "thread" || conversationOperation === "thread/page") {
            reads++; const value=payload(url.searchParams.get("cursor"));
            if(value.fixtureReset){expectedReadErrors.push(409);return route.fulfill({status:409,json:{error:"History changed",code:"transcript_reset_required"}});}
            if(value.fixtureReadFailure){expectedReadErrors.push(503);return route.fulfill({status:503,json:{error:"Read unavailable"}});}
            return json(value);
          }
          if (conversationOperation === "provider") return json({threadId: pi ? "fixture-native-thread" : "thread-1",
            generation: "fixture-generation", providerId: "fixture", providerLabel: "Fixture",
            observedAt: timestamp, usage: {state: "unavailable", reason: "fixture"}});
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
        await page.goto(baseURL + (pi ? "/fixture/pi-transcript?initialThread=empty" : "/example/"));
        const fresh = await page.evaluate(async () => ({...window.fixtureFreshState, registrations: navigator.serviceWorker ? (await navigator.serviceWorker.getRegistrations()).length : 0}));
        assert.deepEqual(fresh.localStorageKeys, []);
        assert.notEqual(fresh.notificationPermission, "granted");
        assert.equal(fresh.registrations, 0);
        try {
          await expect(page.locator("#transcript .message.user")).toHaveCount(entries.filter(e => e.kind === "userMessage").length, {timeout: 5000});
        } catch (error) {
          console.error("FIXTURE_INITIAL_RENDER " + JSON.stringify({errors,unexpected,reads,
            state:await page.evaluate(()=>({initialThread:document.body.dataset.threadId,scope:document.body.dataset.conversationScope,
              connection:document.getElementById("conversation-connection")?.textContent,rows:document.querySelectorAll("#transcript-entries .message").length}))}));
          throw error;
        }
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
    // Source-shaped rows from the Pi adapter's native activity projection.
    const activityRows = [
      item("userMessage", "activity-user", "Exact user wire"),
      {...item("thinking", "read-call:pi-block:0", ""), summary: "Thinking", details: "Public reasoning", activity: {action: "thinking"}},
      {...item("toolCall", "read-call:pi-block:1", ""), summary: "Read /fixture/source.go", details: '{"path":"/fixture/source.go"}', activity: {action: "toolCall", status: "Requested"}},
      {...item("toolResult", "read-result", ""), summary: "Read /fixture/source.go · Result", details: '<script>unsafe</script>\n[Image omitted from transcript display]', activity: {action: "toolResult", status: "completed"}},
      item("agentMessage", "mixed-agent", "Exact answer continued"),
      {...item("toolResult", "failed-result", ""), summary: "Run echo second · Result", details: "second failed", activity: {action: "toolResult", status: "failed"}},
      {...item("assistantActivity", "empty-envelope", ""), summary: "Assistant activity · pending", activity: {action: "assistantActivity"}, timestamp: undefined},
      item("error", "native-error", "literal failure"),
      {...item("agentMessage", "pi-stream", "Live reply"), timestamp: "2025-11-14T12:34:56.789Z", turnStatus: "inProgress"},
      {...item("toolCall", "pi-stream:pi-block:1", ""), summary: "Read /fixture/live.go", activity: {action: "toolCall", status: "Preparing"}, timestamp: "2025-11-14T12:34:56.789Z", turnStatus: "inProgress"},
    ];
    await run({entries: activityRows, check: async (page, context) => {
      const transcript = page.locator("#transcript");
      const select = async filter => { await page.locator(`[data-transcript-filter="${filter}"]`).click(); };
      await select("messages");
      await expect(transcript.locator(".message")).toHaveCount(4);
      await expect(transcript.getByText("Exact user wire", {exact: true})).toBeVisible();
      await expect(transcript.getByText("Exact answer continued", {exact: true})).toBeVisible();
      await expect(transcript.getByText("Live reply", {exact: true})).toBeVisible();
      await expect(transcript.locator(".message.event")).toHaveCount(0);
      assert.equal(await transcript.getByText("Codex event", {exact: true}).count(), 0);
      await select("all");
      await expect(transcript.locator(".message")).toHaveCount(activityRows.length);
      for (const text of ["Thinking", "Read /fixture/source.go", "Read /fixture/source.go · Result", "Run echo second · Result", "Assistant activity · pending", "Preparing", "Completed", "Failed"]) {
        await expect(transcript.getByText(text, {exact: true})).toBeVisible();
      }
      const result = transcript.locator('[data-transcript-entry-key*="read-result"]');
      await result.locator("summary").click();
      await expect(result.locator("pre")).toHaveText('<script>unsafe</script>\n[Image omitted from transcript display]');
      await expect(result.locator("script, img")).toHaveCount(0);
      const live = transcript.locator(".message.agent").filter({hasText: "Live reply"});
      await expect(live.locator(".message-time")).toHaveAttribute("datetime", "2025-11-14T12:34:56.789Z");
      const empty = transcript.locator('[data-transcript-entry-key*="empty-envelope"]');
      await expect(empty.locator(".message-time")).toHaveText("Time unavailable");
      assert.equal(await empty.locator(".message-time").getAttribute("datetime"), null);
      await select("activity");
      await expect(transcript.locator(".message.user, .message.agent")).toHaveCount(0);
      await expect(transcript.getByText("Read /fixture/source.go · Result", {exact: true})).toBeVisible();
      await select("all");
      activityRows.splice(-2, 2, {...item("agentMessage", "persisted-live", "Live reply"), timestamp: "2025-11-14T12:34:56.789Z"});
      await context.refresh();
      await expect(transcript.locator('[data-transcript-entry-key*="pi-stream"]')).toHaveCount(0);
      await expect(transcript.getByText("Live reply", {exact: true})).toHaveCount(1);
      await page.reload();
      await expect(transcript.locator(".message.user")).toHaveCount(1);
      await select("all");
      await expect(transcript.getByText("Read /fixture/source.go · Result", {exact: true})).toBeVisible();
      await expect(transcript.getByText("Live reply", {exact: true})).toHaveCount(1);
    }});
    const outputRows = [item("userMessage", "output-user", "Short prompt")];
    await run({entries: outputRows, check: async (page, context) => {
      const transcript = page.locator("#transcript");
      const newOutput = page.locator("#new-output");
      const remaining = () => transcript.evaluate(node => node.scrollHeight - node.clientHeight - node.scrollTop);
      const refresh = async () => {
        await context.refresh();
        await expect(transcript.locator(".message")).toHaveCount(outputRows.length);
      };
      const append = text => outputRows.push(item("agentMessage", `output-${outputRows.length}`, text));
      await expect(transcript).toBeVisible();
      assert.ok(await remaining() <= 0, "short transcript fits before output");
      // An upward input can pause follow even when the view cannot scroll.
      await transcript.dispatchEvent("wheel", {deltaY: -120});
      append("Short reply that still fits");
      await refresh();
      assert.ok(await remaining() <= 0, "new reply fits too");
      await expect(newOutput).toBeHidden();

      for (let n = 0; n < 20; n++) append(`Overflow message ${n}`);
      await refresh();
      assert.ok(await remaining() > 48, "paused transcript leaves output below the viewport");
      await expect(newOutput).toBeVisible();
      await page.locator('[data-transcript-filter="activity"]').click();
      await expect(newOutput).toBeHidden();
      await page.locator('[data-transcript-filter="messages"]').click();
      await expect(newOutput).toBeVisible();
      assert.ok(await remaining() > 48, "switching filters preserves unread output below");

      const viewport = page.viewportSize();
      await page.setViewportSize({...viewport, height: 2600});
      await expect.poll(remaining).toBeLessThanOrEqual(0);
      await expect(newOutput).toBeHidden();
      await page.setViewportSize(viewport);
      await expect.poll(remaining).toBeGreaterThan(48);
      await expect(newOutput).toBeHidden();

      append("New output below after resize");
      await refresh();
      await expect(newOutput).toBeVisible();
      await newOutput.click();
      await expect(newOutput).toBeHidden();
      await expect.poll(remaining).toBeLessThanOrEqual(1);
      append("Following the tail again");
      await refresh();
      await expect.poll(remaining).toBeLessThanOrEqual(1);
      await expect(newOutput).toBeHidden();
    }});
    // Sliding native windows change workflow grouping, never display identity.
    // More than50 saved entries exercise retention rather than legacy replacement.
    const saved = Array.from({length:275}, (_, n) => ({...item(n % 50 === 0 ? "userMessage" : "agentMessage", `saved-${n}`,
      n === 3 || n === 4 ? "Legitimate equal text" : `Saved ${n}`), timestamp:new Date(1763121600000+n*1000).toISOString()}));
    saved[175] = {...saved[175],text:"Second live response",html:"Second live response"};
    const savedPage = (start,end) => {
      let turnId="";
      return saved.slice(start,end).flatMap(entry=>{
        if(entry.kind==="userMessage")turnId=entry.itemId;
        const row={...entry,turnId};
        return Number(entry.itemId.slice(6)) % 17 === 1 ? [row,{...row,kind:"thinking",itemId:entry.itemId+":pi-block:0",text:"",html:"",summary:"Thinking",details:"Saved activity"}] : [row];
      });
    };
    let newest=[0,50],liveText="First live response",resetOnce=false,failReads=false,changedIdentity=false;
    const live = text => [{...item("agentMessage","pi-stream",text),turnStatus:"inProgress"},
      {...item("thinking","pi-stream:pi-block:0",""),summary:"Thinking",details:text,turnStatus:"inProgress"}];
    const readPage = cursor => {
      if(failReads)return {fixtureReadFailure:true};
      if(cursor && resetOnce){resetOnce=false;return {fixtureReset:true};}
      const end=cursor?Number(cursor.slice(7)):newest[1],start=cursor?Math.max(0,end-50):newest[0];
      return {...(changedIdentity?{threadId:"different-native-thread"}:{}),entries:[...savedPage(start,end),...(cursor?live("Older-page live must stay absent").map(e=>({...e,timestamp:"2020-01-01T00:00:00Z"})):liveText?live(liveText):[])],
        hasOlder:start>0,olderCursor:start>0?`before-${start}`:null};
    };
    await run({entries:savedPage(0,50),readPage,check:async(page,context)=>{
      const rows=()=>page.locator("#transcript-entries .message").evaluateAll(nodes=>nodes.map(n=>({
        id:JSON.parse(n.dataset.transcriptEntryKey)[1],time:n.querySelector(".message-time")?.dateTime||""})));
      const verify=async(end,currentLive)=>{
        const retained=savedPage(0,end),expected=[...retained,...(currentLive?live(currentLive):[])];
        await page.locator('[data-transcript-filter="all"]').click();
        await expect.poll(async()=> (await rows()).map(r=>r.id)).toEqual(expected.map(e=>e.itemId));
        assert.deepEqual((await rows()).map(r=>r.time),expected.map(e=>new Date(e.timestamp).toISOString()));
        assert.equal(new Set((await rows()).map(r=>r.id)).size,expected.length);
        await expect(page.locator("#transcript .message.user")).toHaveCount(retained.filter(e=>e.kind==="userMessage").length);
        await page.locator('[data-transcript-filter="messages"]').click();
        await expect.poll(async()=> (await rows()).map(r=>r.id)).toEqual(expected.filter(e=>["userMessage","agentMessage"].includes(e.kind)).map(e=>e.itemId));
        await expect(page.locator("#transcript .message.user")).toHaveCount(retained.filter(e=>e.kind==="userMessage").length);
        await page.locator('[data-transcript-filter="activity"]').click();
        await expect(page.locator("#transcript .message.user, #transcript .message.agent")).toHaveCount(0);
        await page.locator('[data-transcript-filter="all"]').click();
        await expect.poll(async()=> (await rows()).map(r=>r.id)).toEqual(expected.map(e=>e.itemId));
      };
      await verify(50,liveText);
      await expect(page.locator("#transcript").getByText("Legitimate equal text",{exact:true})).toHaveCount(2);
      newest=[25,75];await context.refresh();await verify(75,liveText);
      newest=[125,175];liveText="Second live response";await context.refresh();await verify(175,liveText);
      await expect(page.locator("#transcript").getByText("First live response",{exact:true})).toHaveCount(0);
      await expect(page.locator("#transcript").getByText("Older-page live must stay absent",{exact:true})).toHaveCount(0);
      failReads=true;await context.refresh();await verify(175,liveText); // failed reads keep the successful live group
      failReads=false;await context.refresh();await verify(175,liveText); // repeated event hints keep one live group
      newest=[126,176];liveText="";await context.refresh();await verify(176,"");
      await expect(page.locator('[data-transcript-entry-key*="pi-stream"]')).toHaveCount(0);
      await expect(page.locator("#transcript").getByText("Second live response",{exact:true})).toHaveCount(1);
      newest=[225,275];resetOnce=true;await context.refresh();await verify(275,"");
      await page.reload();
      await expect(page.locator("#transcript .message.user")).toHaveCount(1);
      await page.locator('[data-transcript-filter="all"]').click();
      await expect.poll(async()=> (await rows()).map(r=>r.id)).toEqual(savedPage(225,275).map(e=>e.itemId));
      for(const filter of ["messages","all"]){
        await page.locator(`[data-transcript-filter="${filter}"]`).click();
        await expect(page.locator("#transcript .message.user")).toHaveCount(1);
        await expect(page.locator('[data-transcript-entry-key*="saved-250"]')).toHaveCount(1);
      }
      liveText="Identity-bound live";await context.refresh();
      await expect(page.locator("#transcript .message.agent").getByText(liveText,{exact:true})).toHaveCount(1);
      changedIdentity=true;await context.refresh();
      await expect(page.locator('[data-transcript-entry-key*="pi-stream"]')).toHaveCount(0);
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
