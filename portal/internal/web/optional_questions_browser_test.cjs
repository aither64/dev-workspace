"use strict";
const assert = require("node:assert/strict");
const {chromium, expect} = require("@playwright/test");
const baseURL = process.argv[2];
const id = index => JSON.stringify(["request_user_input_async", "optional-source", index]);
(async () => {
  const browser = await chromium.launch({headless: true, channel: "chromium"});
  try {
    const page = await browser.newPage({ignoreHTTPSErrors: true, viewport: {width: 1280, height: 900}});
    const errors = [], sends = [];
    page.on("pageerror", error => errors.push(error.message));
    let fail = false, reads = 0;
    const thread = {threadId: "thread-1", status: "active", latestTurnId: "turn-1", entries: [], collaborationMode: "default",
      hasOlder: false, olderCursor: null};
    await page.route("**/api/sessions/example/**", async route => {
      const operation = new URL(route.request().url()).pathname.split("/").at(-1);
      const json = (value, status = 200) => route.fulfill({status, contentType: "application/json", body: JSON.stringify(value)});
      if (operation === "page" || operation === "thread") { reads++; return json(thread); }
      if (operation === "pending" || operation === "queue") return json([]);
      if (operation === "message") {
        const body = route.request().postDataJSON(); sends.push(body);
        if (fail) { fail = false; return json({error: "Connection interrupted"}, 503); }
        const replies = JSON.parse(body.message.split("\n")[1]);
        thread.entries.push({kind: "userMessage", turnId: "turn-1", itemId: `reply-${sends.length}`,
          text: body.message, displayText: `> ${replies[0].question}\n\n${replies[0].answer}`,
          questionReplies: replies, clientUserMessageId: body.clientUserMessageId});
        return json({turnId: "turn-1", clientUserMessageId: body.clientUserMessageId, steered: true}, 202);
      }
      return route.continue();
    });
    const refresh = async () => {
      const previous = reads;
      await page.request.post(baseURL + "/fixture/refresh");
      await expect.poll(() => reads).toBeGreaterThan(previous);
    };
    const pin = page.locator("#optional-questions");
    const inline = page.locator(".codex-inline-questions .codex-async-question");
    const prompt = page.locator("#message-form textarea[name=message]");
    await page.goto(baseURL + "/example/#codex");
    await prompt.fill("Keep this separate draft"); await prompt.focus();
    const source = {kind: "agentMessage", itemId: "optional-source", turnId: "turn-1", turnStatus: "inProgress",
      text: "A few optional questions.", delivery: "async", questions: [
        {id: id(0), title: "Which approach?", options: ["First", "Second"]},
        {id: id(1), title: "Anything else?"},
      ]};
    thread.entries = [source]; await refresh();
    await expect(pin.locator(".codex-async-question")).toHaveCount(2);
    await expect(prompt).toBeFocused();
    await expect(prompt).toHaveValue("Keep this separate draft");
    await inline.first().getByRole("button", {name: "Second", exact: true}).click();
    await expect(inline.first()).toContainText("Answered: Second");
    await expect(pin.locator(".codex-async-question")).toHaveCount(1);
    await expect(prompt).toHaveValue("Keep this separate draft");
    await pin.getByRole("textbox").fill("Typed optional answer");
    await pin.getByRole("button", {name: "Dismiss", exact: true}).click();
    await expect(pin).toBeHidden();
    await expect(inline.nth(1).getByRole("textbox")).toHaveValue("Typed optional answer");
    await page.reload();
    await expect(pin).toBeHidden();
    await expect(inline.first()).toContainText("Answered: Second");
    await expect(inline.nth(1).getByRole("textbox")).toHaveValue("Typed optional answer");
    fail = true;
    await inline.nth(1).getByRole("button", {name: "Send answer", exact: true}).click();
    await expect(inline.nth(1).getByRole("button", {name: "Retry answer"})).toBeVisible();
    await inline.nth(1).getByRole("button", {name: "Retry answer"}).click();
    await expect(inline.nth(1)).toContainText("Answered: Typed optional answer");
    assert.equal(sends[1].clientUserMessageId, sends[2].clientUserMessageId);
    assert.equal(sends[1].message, sends[2].message);
    assert.equal(sends[2].retry, true);
    const next = {...source, itemId: "next-source", questions: [{id: JSON.stringify(["request_user_input_async", "next-source", 0]), title: "Later?", options: ["Yes"]}]};
    thread.entries.push(next, ...Array.from({length: 30}, (_, index) => ({kind: "agentMessage", itemId: `output-${index}`,
      turnId: "turn-1", turnStatus: "inProgress", text: "Normal output\n".repeat(8)})));
    await refresh();
    await expect(pin).toBeVisible();
    await page.locator("#transcript").evaluate(element => { element.scrollTop = 0; });
    const bounds = await pin.boundingBox();
    assert(bounds.y > 0 && bounds.y + bounds.height <= 900, "questions stay below the scrolling output");
    await page.setViewportSize({width: 390, height: 844});
    await expect(pin).toBeVisible();
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    thread.status = "idle";
    for (const entry of thread.entries) entry.turnStatus = "completed";
    await refresh();
    await expect(pin).toBeHidden();
    await inline.last().getByRole("button", {name: "Yes", exact: true}).click();
    await expect(inline.last()).toContainText("Answered: Yes");
    assert.deepEqual(errors, []);
    console.log("Optional questions: inline, pinning, dismissal, draft recovery, retry, completion and mobile passed");
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
