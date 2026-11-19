"use strict";
const assert = require("node:assert/strict");
const {chromium, expect} = require("@playwright/test");
const baseURL = process.argv[2];
const question = (id, isBlocking = true) => ({
  id, token: "offer-" + id, threadId: "thread-1", turnId: "turn-1", itemId: id, method: "item/tool/requestUserInput", kind: "userInput", authorityAvailable: true, isBlocking,
  questions: [{id: "approach", header: "Approach", question: "Which approach should I use?",
    options: [{label: "First", description: "The first approach."}, {label: "Second", description: "The second approach."}], isOther: true}],
});
async function planPresentation(browser) {
  for (const [kind, viewport] of [['desktop', {width: 1280, height: 800}], ['mobile', {width: 390, height: 844}]]) {
    for (const backend of ['codex', 'pi']) {
      const page = await browser.newPage({ignoreHTTPSErrors: true, viewport});
      const errors = [], scopeRequests = []; let captures = 0, lookups = 0, original, holdRead = null, amended = false, continued = false;
      page.on('pageerror', error => errors.push(error.message));
      await page.addInitScript(() => {
        window.fixtureEvents = [];
        window.EventSource = class {
          constructor() {window.fixtureEvents.push(this); queueMicrotask(() => this.onopen?.());}
          addEventListener() {} close() {}
        };
      });
      const slug = `2026-11-19-plan-${kind}`, base = `/api/sessions/${slug}`;
      const prefixes = [base, `/codex/conversations/${encodeURIComponent(slug)}`];
      if (backend === 'pi') await page.route('**/*', async route => {
        const request = route.request(), path = new URL(request.url()).pathname;
        const prefix = prefixes.find(value => path.startsWith(value + '/'));
        if (!prefix) return route.continue();
        const operation = path.slice(prefix.length + 1), scope = request.headers()['x-workspace-conversation-scope'];
        const method = request.method();
        const transport = prefix !== base ? 'conversation' : ['attention', 'coordination'].includes(operation) ? 'session-observation' :
          operation === 'pi-questions' || operation === 'pi-plan' || operation.startsWith('pi-plan/') ? 'workflow' : 'session-client';
        const observed = {method, path, operation, transport, scope: scope ?? null};
        scopeRequests.push(observed);
        if (transport === 'session-observation') assert.equal(method, 'GET', JSON.stringify(observed));
        if (operation === 'pi-plan' || operation.startsWith('pi-plan/acceptances')) {
          if (request.method() === 'POST') {
            assert.equal(operation, 'pi-plan/acceptances'); captures++;
            const response = await route.fetch(); assert.equal(response.status(), 202);
            original = await response.json();
            assert.deepEqual(Object.keys(request.postDataJSON()).sort(), ['acceptanceId', 'planId', 'revision', 'textSha256']);
            return route.abort('failed'); // Server capture succeeded; its reply is lost.
          }
          if (operation.startsWith('pi-plan/acceptances/')) {lookups++; assert(operation.endsWith(original.acceptanceId));}
          if (operation === 'pi-plan' && holdRead) {
            const held = holdRead; holdRead = null; const response = await route.fetch();
            held.started = true; await held.promise; held.finished = true;
            return route.fulfill({response}).catch(() => {});
          }
          return route.continue();
        }
        const json = value => route.fulfill({json: value, headers: {'X-Workspace-Conversation-Scope': scope}});
        if (operation === 'queue/reconcile' && request.method() === 'POST') return json({ok: true});
        assert.equal(request.method(), 'GET', 'no unrelated Pi fixture mutation');
        if (['thread', 'thread/page'].includes(operation)) return json({threadId: 'plan-thread', latestTurnId: continued ? 'input-later' : 'input-a', status: 'idle', model: 'model-1', reasoningEffort: 'medium', inputDelivery: true,
          entries: [{kind: 'userMessage', itemId: 'input-a', turnId: 'input-a', text: 'Plan the change.'},
            {kind: 'agentMessage', itemId: 'proposal-a', turnId: 'input-a', text: 'Published the proposal.'},
            ...(amended ? [{kind: 'toolCall', itemId: 'proposal-b:pi-block:0', turnId: 'input-a', summary: 'Publish amendment'}] : []),
            ...(continued ? [{kind: 'userMessage', itemId: 'input-later', turnId: 'input-later', text: 'Continue planning.'},
              {kind: 'agentMessage', itemId: 'later-output', turnId: 'input-later', text: 'Later progress.'}] : [])], hasOlder: false});
        if (operation === 'pi-questions') return json({revision: 0, questions: []});
        if (operation === 'details') return json({sessionBackend: 'pi', conversationScope: scope, conversationInteractive: true, conversationHeld: false, readyMembers: [], repositoriesHTML: '', artifactsHTML: ''});
        if (['pending', 'queue'].includes(operation)) return json([]);
        if (operation === 'provider') return json({threadId: 'plan-thread', providerId: 'fixture', providerLabel: 'Fixture', usage: {state: 'unavailable', reason: 'fixture'}});
        if (operation === 'models') return json([{id: 'model-1', model: 'model-1', supportedReasoningEfforts: [{reasoningEffort: 'medium'}], defaultReasoningEffort: 'medium'}]);
        if (operation === 'activity') return json({threadId: 'plan-thread', currentState: 'idle', workingMs: 0, waitingMs: 0, idleMs: 0, observedAtMs: Date.now(), coverageComplete: true});
        if (operation === 'operation') return json({state: 'idle', options: {}});
        if (operation === 'progress') return json({available: false});
        if (operation === 'attention') return json({version: 1, identity: 'b'.repeat(64), rootThread: '', enabled: false, revision: 0, event: null, fresh: true});
        return route.continue();
      });
      const refresh = () => page.evaluate(() => {for (const source of window.fixtureEvents) source.onmessage?.({data: '{}'});});
      await page.goto(baseURL + (backend === 'pi' ? `/fixture/pi-plan?case=${kind}` : '/example/'), {waitUntil: 'domcontentloaded'});
      const bar = page.locator('#plan-actions'), composer = page.locator('#message-form');
      const bodies = backend === 'pi' ? page.locator('#transcript-entries .message[data-plan-id]') : page.locator('#transcript-entries .message.markdown');
      await expect(bar).toBeVisible(); await expect(bodies).toHaveCount(1);
      await expect(bodies.first().locator('h1')).toHaveText('Retained plan é');
      await expect(bodies.first().locator('ul')).toHaveCount(1); await expect(bodies.first().locator('pre code')).toHaveCount(1);
      await expect(bodies.first().locator('.table-scroll table')).toHaveCount(1);
      await expect(bodies.first().locator('script, [onerror], a[href^="javascript:"]')).toHaveCount(0);
      assert.equal(await page.evaluate(() => window.planExecuted), undefined);
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), true);
      assert((await bar.boundingBox()).height < 220, 'approval bar remains compact');
      await expect(bar.locator('pre')).toHaveCount(0);
      if (backend === 'codex') await expect(page.locator('#plan-implement-new')).toBeVisible();
      else await expect(page.locator('#plan-implement-new')).toBeHidden();
      await expect(page.locator('#plan-proposals')).toHaveCount(0);
      if (backend === 'codex') await expect(page.locator('#transcript-entries [data-plan-id]')).toHaveCount(0);
      else {
        await expect(page.locator('#transcript-entries [data-plan-id]')).toHaveCount(1);
        await page.evaluate(() => {
          window.originalProposalNode = document.querySelector('#transcript-entries [data-plan-id]');
          const range = document.createRange(), text = window.originalProposalNode.querySelector('h1').firstChild;
          range.setStart(text, 0); range.setEnd(text, 5); const selection = getSelection(); selection.removeAllRanges(); selection.addRange(range);
        });
        await refresh(); await expect.poll(() => page.evaluate(() => document.querySelector('#transcript-entries [data-plan-id]') === window.originalProposalNode)).toBe(true);
        assert.equal(await page.evaluate(() => getSelection().toString()), 'Retai');
      }
      await page.locator('#plan-keep-planning').focus(); await page.keyboard.press('Enter');
      await expect(composer).toBeVisible(); await expect(composer.locator('textarea')).toBeFocused(); await expect(bodies).toHaveCount(1);
      if (backend === 'pi') {
        await page.reload(); await expect(bar).toBeVisible(); await expect(bodies).toHaveCount(1);
        await page.locator('#plan-implement-same').click();
        await expect.poll(() => captures).toBe(1); await expect(bar).toBeHidden(); await expect(composer).toBeVisible();
        const first = page.locator('#transcript-entries [data-plan-id]').first();
        await expect(first.locator('.plan-request-status')).toHaveAttribute('data-acceptance-id', original.acceptanceId);
        await first.getByRole('button', {name: 'Check request', exact: true}).click();
        await expect.poll(() => lookups).toBe(1); assert.equal(captures, 1);
        const publish = await page.request.post(baseURL + `/fixture/refresh?planCase=${kind}&amend=1`); assert(publish.ok()); amended = true;
        await page.reload(); await expect(bodies).toHaveCount(2);
        const cards = page.locator('#transcript-entries [data-plan-id]');
        assert.notEqual(await cards.nth(0).getAttribute('data-plan-id'), await cards.nth(1).getAttribute('data-plan-id'));
        await expect(cards.nth(0).locator('.plan-request-status')).toHaveAttribute('data-acceptance-id', original.acceptanceId);
        await page.locator('#plan-keep-planning').click(); await expect(composer).toBeVisible();
        const phase = kind === 'desktop' ? 'held' : 'accepted';
        const settle = await page.request.post(baseURL + `/fixture/refresh?planCase=${kind}&settle=${phase}`); assert(settle.ok());
        await refresh();
        await expect(cards.nth(0).locator('.plan-request-status')).toHaveAttribute('data-phase', phase);
        await expect(composer).toBeVisible(); await expect(bodies).toHaveCount(2); assert.equal(captures, 1);
        const actualScope = await page.locator('body').getAttribute('data-conversation-scope');
        const required = scopeRequests.filter(value => value.transport !== 'session-observation');
        const sessionReads = scopeRequests.filter(value => value.transport === 'session-observation');
        const diagnostics = JSON.stringify({requiredCount: required.length, sessionReadCount: sessionReads.length,
          requests: scopeRequests.map(value => ({...value, scopeMatches: value.scope === (value.transport === 'session-observation' ? null : actualScope)}))});
        assert(required.length > 0, diagnostics);
        assert(required.some(value => value.transport === 'conversation' && ['thread', 'thread/page'].includes(value.operation)), diagnostics);
        assert(required.some(value => value.transport === 'workflow' && value.operation === 'pi-plan'), diagnostics);
        assert(required.every(value => value.scope === actualScope), diagnostics);
        assert(sessionReads.every(value => value.method === 'GET' && value.scope === null), diagnostics);
        continued = true; await refresh();
        await expect(page.locator('#transcript-entries .message').last()).toContainText('Later progress.');
        await expect(bar).toBeHidden(); await expect(bodies).toHaveCount(2);
        await page.reload(); await expect(bar).toBeHidden(); await expect(bodies).toHaveCount(2);
        await expect(cards.nth(0).locator('.plan-request-status')).toHaveAttribute('data-phase', phase);
        assert.equal(captures, 1, 'manual continuation never resubmits or transitions the original approval');
        let release; const held = holdRead = {promise: new Promise(resolve => {release = resolve;}), started: false, finished: false};
        await refresh(); await expect.poll(() => held.started).toBe(true);
        await page.evaluate(() => {document.body.dataset.conversationScope = 'pi-workflow-v1:' + 'e'.repeat(64); dispatchEvent(new Event('pagehide'));});
        await expect(page.locator('#transcript-entries [data-plan-id]')).toHaveCount(0);
        release(); await expect.poll(() => held.finished).toBe(true); await expect(page.locator('#transcript-entries [data-plan-id]')).toHaveCount(0);
      }
      assert.deepEqual(errors, []); await page.close();
    }
  }
}
(async () => {
  const browser = await chromium.launch({headless: true, channel: "chromium"});
  try {
    if (process.env.PLAN_PRESENTATION_ONLY === '1') {await planPresentation(browser); return;}
    const page = await browser.newPage({ignoreHTTPSErrors: true, viewport: {width: 1280, height: 720}});
    const errors = [], dialogs = [], requests = [], results = [];
    page.on("pageerror", error => errors.push(error.message));
    page.on("dialog", async dialog => { dialogs.push(dialog.message()); await dialog.accept(); });
    let entries = [], offline = false, failAnswer = false, blockUpload = false, releaseUpload;
    let failSnooze = false, holdSnooze = false, releaseSnooze;
    let thread = {threadId: "thread-1", latestTurnId: "turn-1", status: "active", collaborationMode: "plan", model: "model-1", reasoningEffort: "medium", entries: []};
    let reads = 0;
    await page.route("**/api/sessions/example/**", async route => {
      const operation = new URL(route.request().url()).pathname.split("/").at(-1);
      const json = (data, status = 200) => route.fulfill({status, contentType: "application/json", body: JSON.stringify(data)});
      if (["thread", "pending", "queue"].includes(operation) && offline) return json({error: "Fixture offline"}, 503);
      switch (operation) {
        case "thread": reads++; return json(thread);
        case "pending": return json(entries);
        case "queue": return json([]);
        case "reconcile": return json({ok: true});
        case "events": return route.continue();
        case "respond":
          if (route.request().postDataJSON().snooze) {
            requests.push({operation: "snooze", token: route.request().postDataJSON().token});
            if (holdSnooze) {
              holdSnooze = false;
              await new Promise(resolve => { releaseSnooze = resolve; });
              return json({error: "Retired offer"}, 503);
            }
            if (failSnooze) { failSnooze = false; return json({error: "Temporary snooze failure"}, 503); }
            return json({ok: true});
          }
          requests.push({operation, body: route.request().postDataJSON()});
          if (typeof failAnswer === "string") {
            const changed = failAnswer === "changed";
            const restored = {...entries[0], id: "restored-" + entries[0].id, token: "restored-" + entries[0].token,
              ...(changed ? {questions: [{...entries[0].questions[0], question: "A different question"}]} : {})};
            failAnswer = false; entries = [];
            setTimeout(() => { entries = [restored]; }, 300);
            return json({error: "Connection changed", code: "prompt_transport", notSent: true}, 503);
          }
          if (failAnswer) return json({error: "Fixture answer failed"}, 503);
          entries = entries.filter(entry => entry.id !== route.request().postDataJSON().id);
          return json({ok: true});
        case "interrupt": requests.push({operation}); entries = []; thread.status = "idle"; return json({ok: true});
        default: return route.continue();
      }
    });
    // Keep the real upload service; hold a chunk so hiding is tested mid-transfer.
    await page.route("**/uploads/**", async route => {
      if (blockUpload && route.request().method() === "PATCH") {
        await new Promise(resolve => { releaseUpload = resolve; });
      }
      return route.continue();
    });
    const refresh = async () => {
      const before = reads;
      await page.request.post(baseURL + "/fixture/refresh");
      await expect.poll(() => reads).toBeGreaterThan(before);
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    };
    const wizard = () => page.locator(".question-approval");
    const composer = page.locator("#message-form");
    const prompt = composer.locator("textarea[name=message]");
    const interrupt = page.locator("#interrupt");
    await page.goto(baseURL + "/example/");
    await expect(page.locator("#codex-model")).toHaveValue("model-1");
    await expect(page.locator("#auto-archive-panel")).toBeHidden();
    await page.getByRole("tab", {name: "Settings", exact: true}).click();
    await page.locator('[data-settings-tab="automation"]').click();
    await expect(page.locator("#auto-archive-panel")).toBeVisible();
    await page.locator("#session-tab-codex").click();
    await expect(page.locator("#auto-archive-panel")).toBeHidden();
    await expect(page.locator(".codex-upload-toggle")).toBeEnabled();
    await prompt.fill("Keep my prompt draft");
    await prompt.focus();
    entries = [question("question-1")]; await refresh();
    await expect(wizard()).toHaveCount(1);
    await expect(composer).toBeHidden();
    await expect(wizard().locator("input[value=First]")).toBeFocused();
    await expect(wizard().locator(".question-heading #interrupt")).toHaveCount(1);
    await expect(interrupt).toBeEnabled();
    await refresh(); await refresh();
    await expect(composer).toBeHidden();
    results.push("blocking question hides composer across thread refreshes");

    await wizard().locator("input[value=Second]").check();
    const note = wizard().locator("textarea");
    await note.fill("Keep this answer");
    await note.evaluate(input => input.setSelectionRange(4, 9));
    // A changed snapshot replaces the DOM, retaining answer focus and selection.
    entries = [{...entries[0], autoResolveSnoozed: true}]; await refresh();
    await expect(wizard().locator("textarea")).toBeFocused();
    await expect(wizard().locator("textarea")).toHaveValue("Keep this answer");
    assert.deepEqual(await wizard().locator("textarea").evaluate(input => [input.selectionStart, input.selectionEnd]), [4, 9]);
    failAnswer = true;
    await wizard().getByRole("button", {name: "Submit answers"}).click();
    await expect(wizard().locator(".prompt-response-status")).toContainText("Delivery could not be confirmed");
    assert.equal(dialogs.length, 0);
    assert.equal(requests.filter(request => request.operation === "respond").length, 1, "uncertain answer was retried automatically");
    await expect(composer).toBeHidden();
    await expect(wizard().locator("textarea")).toHaveValue("Keep this answer");
    offline = true;
    await page.request.post(baseURL + "/fixture/refresh");
    // Transient refresh failures stay quiet for thirty visible seconds.
    await expect(page.locator("#conversation-connection")).toBeHidden();
    await expect(page.locator("#conversation-connection")).toBeVisible({timeout: 35000});
    await expect(composer).toBeHidden();
    offline = false; failAnswer = false;
    await page.locator("#conversation-connection button").click();
    await expect(page.locator("#conversation-connection")).toBeHidden();
    await wizard().getByRole("button", {name: "Submit answers"}).click();
    await expect(wizard()).toHaveCount(0);
    await expect(composer).toBeVisible();
    await expect(prompt).toHaveValue("Keep my prompt draft");
    await expect(prompt).toBeFocused();
    await expect(composer.locator("#interrupt")).toHaveCount(1);
    results.push("failed answers and disconnected snapshots preserve drafts and visibility; successful answer restores focus");

    const beforeRecovery = requests.filter(request => request.operation === "respond").length;
    entries = [question("recover-question")]; await refresh();
    await wizard().locator("input[value=Second]").check();
    await wizard().locator("textarea").fill("Survives reconnection");
    failAnswer = "unsent";
    await wizard().getByRole("button", {name: "Submit answers"}).click();
    await expect(wizard()).toHaveCount(0, {timeout: 15000});
    const recoveredSends = requests.filter(request => request.operation === "respond").slice(beforeRecovery);
    assert.equal(recoveredSends.length, 2);
    assert.notEqual(recoveredSends[0].body.token, recoveredSends[1].body.token);
    assert.deepEqual(recoveredSends[0].body.answers, recoveredSends[1].body.answers);
    assert.equal(dialogs.length, 0);
    results.push("one retry restores the exact question and answers after a temporary missing prompt");

    entries = [question("changed-question")]; await refresh();
    await wizard().locator("input[value=First]").check();
    await wizard().locator("textarea").fill("Answer only the original question");
    const beforeChanged = requests.filter(request => request.operation === "respond").length;
    failAnswer = "changed";
    await wizard().getByRole("button", {name: "Submit answers"}).click();
    await expect(wizard().locator(".wizard-question")).toHaveText("A different question", {timeout: 15000});
    await expect(wizard().locator("textarea")).toHaveValue("");
    assert.equal(requests.filter(request => request.operation === "respond").length - beforeChanged, 1);
    entries = []; await refresh();
    await page.getByRole("button", {name: "Hide saved question"}).click();
    results.push("changed questions never receive an automatic answer or an old draft");

    entries = [question("snooze-retry", false)]; await refresh();
    failSnooze = true;
    await wizard().locator("textarea").fill("Keep answering");
    await expect(wizard().locator(".prompt-response-status")).toContainText("Automatic resolution could not be paused");
    const beforeSnoozeRetry = requests.filter(request => request.operation === "snooze").length;
    await wizard().getByRole("button", {name: "Refresh question"}).click();
    await expect.poll(() => requests.filter(request => request.operation === "snooze").length).toBe(beforeSnoozeRetry + 1);
    await expect(wizard().locator(".auto-resolution")).toContainText("paused while you answer");
    await expect(wizard().locator("textarea")).toHaveValue("Keep answering");
    results.push("Refresh question retries a failed snooze with the unchanged token");

    entries = [question("snooze-replaced", false)]; await refresh();
    holdSnooze = true;
    await wizard().locator("textarea").fill("Survive the old snooze");
    await expect.poll(() => Boolean(releaseSnooze)).toBe(true);
    entries = [{...entries[0], token: "replacement-snooze-token"}]; await refresh();
    releaseSnooze();
    await wizard().locator("textarea").fill("Still answering the current offer");
    await expect.poll(() => requests.filter(request => request.operation === "snooze" && request.token === "replacement-snooze-token").length).toBe(1);
    await expect(wizard().locator(".auto-resolution")).toContainText("paused while you answer");
    await expect(wizard().locator(".prompt-response-status")).toBeHidden();
    entries = []; await refresh();
    results.push("a late snooze failure cannot disable the replacement offer");


    thread.status = "active"; thread.collaborationMode = "default";
    await page.locator("#transcript").focus();
    entries = [question("async-1", false), question("async-2", false)]; await refresh();
    await expect(page.locator("#transcript")).toBeFocused();
    await expect(wizard()).toHaveCount(2);
    await expect(composer).toBeHidden();
    await expect(interrupt).toHaveCount(1);
    await wizard().nth(1).locator("input[value=First]").check();
    await wizard().nth(1).locator("textarea").fill("Second question draft");
    entries = [entries[1]]; await refresh();
    await expect(wizard()).toHaveCount(1);
    await expect(wizard().locator("textarea")).toBeFocused();
    await expect(wizard().locator("textarea")).toHaveValue("Second question draft");
    await expect(wizard().locator("#interrupt")).toHaveCount(1);
    await interrupt.click();
    await expect(composer).toBeVisible();
    await expect(interrupt).toBeDisabled();
    await expect(prompt).toBeFocused();
    assert.equal(requests.filter(request => request.operation === "interrupt").length, 1);
    results.push("asynchronous questions in Default mode hide composer; one interrupt survives replacement and works");

    for (const entry of [
      {id: "approval", kind: "command", method: "item/commandExecution/requestApproval", authorityAvailable: true, availableDecisions: ["accept"]},
      {id: "terminal", kind: "terminalOnly", method: "item/permissions/requestApproval"},
      {...question("unavailable"), authorityAvailable: false},
      {...question("error"), error: "Fixture request error"},
    ]) {
      entries = [entry]; await refresh();
      await expect(page.locator("#pending .approval")).toHaveCount(1);
      await expect(wizard()).toHaveCount(0);
      await expect(composer).toBeVisible();
    }
    entries = []; await refresh();
    results.push("approvals, terminal-only requests and unanswerable questions retain normal controls");

    blockUpload = true;
    await composer.locator("input[type=file]").setInputFiles({name: "draft.txt", mimeType: "text/plain", buffer: Buffer.from("Preserved attachment")});
    await expect.poll(() => Boolean(releaseUpload)).toBe(true).catch(async error => {
      console.error("Upload state:", await page.locator("#message-uploads").textContent());
      throw error;
    });
    await page.locator(".codex-upload-toggle").click();
    await expect(page.locator(".codex-upload-menu")).toBeVisible();
    thread.status = "active"; thread.collaborationMode = "plan";
    entries = [question("upload-question")]; await refresh();
    await expect(composer).toBeHidden();
    await expect(page.locator(".codex-upload-menu")).toBeHidden();
    blockUpload = false; releaseUpload();
    await expect(composer.locator("#message-send")).toBeEnabled();
    entries = []; await refresh();
    await expect(composer).toBeVisible();
    await expect(composer.locator("#message-uploads")).toContainText("draft.txt");
    await expect(prompt).toHaveValue("Keep my prompt draft");
    results.push("attachment popover closes; an in-progress upload completes while hidden and remains attached");

    await prompt.focus();
    entries = [question("plan-question")]; await refresh();
    thread = {...thread, status: "idle", latestTurnId: "plan-turn", entries: [{kind: "plan", turnId: "plan-turn", turnStatus: "completed", text: "Implement the feature."}]};
    await refresh();
    await expect(page.locator("#plan-actions")).toBeHidden();
    await expect(composer).toBeHidden();
    entries = []; await refresh();
    await expect(page.locator("#plan-actions")).toBeVisible();
    await expect(composer).toBeHidden();
    await expect(page.locator("#plan-keep-planning")).toBeFocused();
    await page.locator("#plan-keep-planning").click();
    await expect(composer).toBeVisible();
    await expect(prompt).toBeFocused();
    await expect(prompt).toHaveValue("Keep my prompt draft");
    results.push("pending questions take precedence over completed plans; Keep planning restores the composer");

    thread = {...thread, status: "active", latestTurnId: "next-turn", entries: []};
    const longQuestion = question("geometry");
    longQuestion.questions[0].options = Array.from({length: 12}, (_, i) => ({label: "Option " + i, description: "An explanation of this approach. ".repeat(4)}));
    entries = [longQuestion]; await refresh();
    for (const [width, height] of [[1440, 1000], [1280, 720], [1280, 540], [390, 844], [640, 360]]) {
      await page.setViewportSize({width, height});
      await expect(composer).toBeHidden();
      const geometry = await page.evaluate(() => {
        const rect = selector => document.querySelector(selector).getBoundingClientRect().toJSON();
        const content = document.querySelector(".wizard-content");
        return {width: innerWidth, height: innerHeight, documentWidth: document.documentElement.scrollWidth,
          composer: rect("#message-form"), actions: rect(".wizard-actions"), interrupt: rect("#interrupt"),
          scrolls: content.scrollHeight > content.clientHeight};
      });
      assert.equal(geometry.composer.height, 0);
      assert(geometry.documentWidth <= width, JSON.stringify(geometry));
      assert(geometry.actions.top >= 0 && geometry.actions.bottom <= height, JSON.stringify(geometry));
      assert(geometry.interrupt.top >= 0 && geometry.interrupt.bottom <= height, JSON.stringify(geometry));
      assert(geometry.scrolls, JSON.stringify(geometry));
      results.push({viewport: [width, height], geometry});
    }
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({results, answerRequests: requests.filter(request => request.operation === "respond").length}, null, 2));
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
