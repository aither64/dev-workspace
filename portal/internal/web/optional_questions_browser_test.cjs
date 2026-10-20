"use strict";
const assert = require("node:assert/strict");
const {createHash} = require("node:crypto");
const {chromium, expect} = require("@playwright/test");
const baseURL = process.argv[2];
const id = index => JSON.stringify(["request_user_input_async", "optional-source", index]);
const digest = text => createHash("sha256").update(text).digest("hex");
(async () => {
  const browser = await chromium.launch({headless: true, channel: "chromium"});
  try {
    const page = await browser.newPage({ignoreHTTPSErrors: true, viewport: {width: 1280, height: 900}});
    const errors = [], sends = [];
    page.on("pageerror", error => errors.push(error.message));
    let fail = false, reads = 0, ordinaryReply;
    // Native initial messages legitimately have a digest without a client ID.
    const initial = {kind: "userMessage", itemId: "initial", turnId: "turn-1", text: "Initial task",
      clientUserMessageDigest: digest("Initial task")};
    const thread = {threadId: "thread-1", status: "active", latestTurnId: "turn-1", entries: [initial], collaborationMode: "default",
      hasOlder: false, olderCursor: null};
    await page.route("**/api/sessions/example/**", async route => {
      const operation = new URL(route.request().url()).pathname.split("/").at(-1);
      const json = (value, status = 200) => route.fulfill({status, contentType: "application/json", body: JSON.stringify(value)});
      if (operation === "page" || operation === "thread") { reads++; return json(thread); }
      if (operation === "pending" || operation === "queue") return json([]);
      if (operation === "activity") return json({currentState: thread.status === "active" ? "working" : "idle", coverageComplete: true});
      if (operation === "message-ack") return json({acknowledgedClientUserMessageIds:
        route.request().postDataJSON().acknowledgements.map(row => row.clientUserMessageId)});
      if (operation === "message") {
        const body = route.request().postDataJSON(); sends.push(body);
        if (fail) { fail = false; return json({error: "Connection interrupted"}, 503); }
        const reply = {kind: "userMessage", turnId: "turn-1", itemId: `reply-${sends.length}`, text: body.message,
          clientUserMessageId: body.clientUserMessageId, clientUserMessageDigest: digest(body.message)};
        if (body.message.startsWith("<send_user_message_question_reply>")) {
          const replies = JSON.parse(body.message.split("\n")[1]);
          Object.assign(reply, {displayText: `> ${replies[0].question}\n\n${replies[0].answer}`, questionReplies: replies});
          thread.entries.push(reply);
        } else ordinaryReply = reply; // Exercise accepted-before-observed receipt state.
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
    const toggle = pin.locator(".codex-question-toggle");
    const visibleQuestion = pin.locator(".codex-async-question:visible");
    const inline = page.locator(".codex-inline-questions .codex-async-question");
    const prompt = page.locator("#message-form textarea[name=message]");
    await page.goto(baseURL + "/example/#codex");
    await prompt.fill("Keep this separate draft"); await prompt.focus();
    const source = {kind: "agentMessage", itemId: "optional-source", turnId: "turn-1", turnStatus: "inProgress",
      text: "A few optional questions.", delivery: "async", questions: [
        {id: id(0), title: "Which approach?", options: ["First (Recommended)", "Second"]},
        {id: id(1), title: "Anything else?"},
      ]};
    thread.entries.push(source); await refresh(); await refresh();
    await expect(toggle).toContainText("2 optional questions");
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
    await expect(visibleQuestion).toHaveCount(0);
    assert((await pin.boundingBox()).height < 42, "collapsed panel stays one line high");
    await expect(prompt).toBeFocused();
    await expect(inline.first().getByRole("radio", {name: "First (Recommended)", exact: true})).toBeChecked();
    await expect(inline.first().getByRole("button", {name: "Send answer", exact: true})).toBeEnabled();
    await inline.first().getByRole("radio", {name: "Second", exact: true}).check();
    await inline.first().getByRole("textbox").fill("Extra constraint");
    assert.equal(sends.length, 0, "choosing an option and editing text never sends");
    await toggle.click();
    await expect(visibleQuestion).toHaveCount(1);
    await expect(visibleQuestion.getByRole("radio", {name: "Second", exact: true})).toBeChecked();
    await expect(visibleQuestion.getByRole("textbox")).toHaveValue("Extra constraint");
    await expect(pin.getByRole("button", {name: "Previous question", exact: true})).toHaveText("←");
    await expect(pin.getByRole("button", {name: "Next question", exact: true})).toHaveText("→");
    await pin.getByRole("button", {name: "Next question", exact: true}).click();
    await expect(pin.getByRole("status").first()).toHaveText("Question 2 of 2");
    await visibleQuestion.getByRole("textbox").fill("Typed optional answer");
    await pin.getByRole("button", {name: "Previous question", exact: true}).click();
    await pin.getByRole("button", {name: "Question 2: Anything else?", exact: true}).click();
    await pin.getByRole("button", {name: "Question 1: Which approach?", exact: true}).click();
    await expect(pin.getByRole("button", {name: "Question 1: Which approach?", exact: true})).toHaveAttribute("aria-current", "step");
    await expect(visibleQuestion.getByRole("textbox")).toHaveValue("Extra constraint");
    await toggle.click(); await toggle.click();
    await expect(visibleQuestion.getByRole("textbox")).toHaveValue("Extra constraint");
    await page.reload();
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
    await toggle.click();
    await expect(visibleQuestion.getByRole("radio", {name: "Second", exact: true})).toBeChecked();
    await expect(visibleQuestion.getByRole("textbox")).toHaveValue("Extra constraint");
    await prompt.fill("Keep this separate draft");
    await visibleQuestion.getByRole("button", {name: "Send answer", exact: true}).click();
    await expect(inline.first()).toContainText("Answered: Second\n\nExtra constraint");
    assert.equal(JSON.parse(sends[0].message.split("\n")[1])[0].answer, "Second\n\nExtra constraint");
    await expect(toggle).toContainText("1 optional question");
    await expect(visibleQuestion.getByRole("textbox")).toHaveValue("Typed optional answer");
    await expect(prompt).toHaveValue("Keep this separate draft");
    fail = true;
    await visibleQuestion.getByRole("button", {name: "Send answer", exact: true}).click();
    await expect(visibleQuestion.getByRole("button", {name: "Retry answer"})).toBeVisible();
    await toggle.click();
    await inline.nth(1).getByRole("button", {name: "Retry answer"}).click();
    await expect(inline.nth(1)).toContainText("Answered: Typed optional answer");
    await expect(pin).toBeHidden();
    assert.equal(sends[1].clientUserMessageId, sends[2].clientUserMessageId);
    assert.equal(sends[1].message, sends[2].message);
    assert.equal(sends[2].retry, true);
    const focusQuestion = {...source, itemId: "focus-source", questions: [{id: JSON.stringify(["request_user_input_async", "focus-source", 0]), title: "Ready?", options: ["Continue"]}]};
    thread.entries.push(focusQuestion); await refresh();
    await toggle.click();
    await visibleQuestion.getByRole("radio", {name: "Continue", exact: true}).check();
    await visibleQuestion.getByRole("button", {name: "Send answer", exact: true}).click();
    await expect(pin).toBeHidden();
    await expect(prompt).toBeFocused();
    const neighborQuestions = ["First queued", "Second queued", "Third queued", "Fourth queued"].map((title, index) => ({
      id: JSON.stringify(["request_user_input_async", "neighbor-source", index]), title, options: ["Yes"],
    }));
    thread.entries.push({...source, itemId: "neighbor-source", questions: neighborQuestions}); await refresh();
    await toggle.click();
    await pin.getByRole("button", {name: "Question 4: Fourth queued", exact: true}).click();
    await visibleQuestion.getByRole("textbox").fill("Fourth question draft");
    await pin.getByRole("button", {name: "Question 2: Second queued", exact: true}).click();
    await expect(visibleQuestion).toContainText("Second queued");
    await pin.getByRole("button", {name: "Question 1: First queued", exact: true}).focus();
    thread.entries.push({kind: "userMessage", itemId: "external-replies", text: "External answers", questionReplies:
      neighborQuestions.slice(0, 2).map(question => ({questionItemId: question.id, question: question.title, answer: "Yes"}))});
    await refresh();
    await expect(visibleQuestion).toContainText("Third queued");
    await expect(visibleQuestion.locator(".codex-question-title")).toBeFocused();
    await expect(pin.locator(".codex-question-index button")).toHaveCount(2);
    await expect(pin.getByRole("button", {name: "Question 1: Third queued", exact: true})).toHaveAttribute("aria-current", "step");
    await visibleQuestion.getByRole("button", {name: "Dismiss", exact: true}).click();
    await expect(visibleQuestion).toContainText("Fourth queued");
    await expect(visibleQuestion.getByRole("textbox")).toHaveValue("Fourth question draft");
    await visibleQuestion.getByRole("button", {name: "Dismiss", exact: true}).click();
    await expect(pin).toBeHidden(); await expect(prompt).toBeFocused();
    const next = {...source, itemId: "next-source", questions: [{id: JSON.stringify(["request_user_input_async", "next-source", 0]), title: "Later?", options: ["Yes"]}]};
    thread.entries.push(next, ...Array.from({length: 30}, (_, index) => ({kind: "agentMessage", itemId: `output-${index}`,
      turnId: "turn-1", turnStatus: "inProgress", text: "Normal output\n".repeat(8)})));
    await prompt.focus(); await refresh();
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
    await expect(prompt).toBeFocused();
    await page.locator("#transcript").evaluate(element => { element.scrollTop = 0; });
    await toggle.click();
    const bounds = await pin.boundingBox();
    assert(bounds.y > 0 && bounds.y + bounds.height <= 900, "questions stay below the scrolling output");
    await page.setViewportSize({width: 390, height: 844});
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    await visibleQuestion.getByRole("radio", {name: "Yes", exact: true}).check();
    await visibleQuestion.getByRole("textbox").fill("custom details");
    await visibleQuestion.getByRole("radio", {name: "Other", exact: true}).check();
    await expect(inline.last().getByRole("radio", {name: "Other", exact: true})).toBeChecked();
    await expect(inline.last().getByRole("textbox")).toHaveValue("custom details");
    await visibleQuestion.getByRole("textbox").fill("");
    await page.reload();
    await expect(inline.last().getByRole("radio", {name: "Other", exact: true})).toBeChecked();
    await inline.last().getByRole("textbox").fill("custom details");
    await page.evaluate(() => {
      for (let index = 0; index < localStorage.length; index++) {
        const key = localStorage.key(index);
        if (!key.startsWith("codex-web:questions:")) continue;
        const saved = JSON.parse(localStorage.getItem(key));
        if (saved.note === "custom details") {
          // An older browser edits only the complete string, leaving extra fields.
          saved.draft = "Edited by older browser";
          localStorage.setItem(key, JSON.stringify(saved));
        }
      }
    });
    await page.reload();
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
    await expect(inline.last().getByRole("textbox")).toHaveValue("Edited by older browser");
    await inline.last().getByRole("textbox").fill("custom details");
    // Ordinary sends must still refresh and clear receipts after questions exist.
    await prompt.fill("Ordinary follow-up");
    await page.locator("#message-form").evaluate(form => form.requestSubmit());
    const receipt = page.locator(".message-receipt").filter({hasText: "Ordinary follow-up"});
    await expect(receipt).toContainText("Sent to Codex");
    await expect(receipt.getByRole("button", {name: "Dismiss message", exact: true})).toHaveText("×");
    thread.entries.push(ordinaryReply); await refresh();
    await expect(receipt).toHaveCount(0);
    await expect(page.locator("#codex-status")).toHaveText("active");
    thread.status = "idle";
    for (const entry of thread.entries) entry.turnStatus = "completed";
    await refresh();
    await expect(page.locator("#codex-status")).toHaveText("idle");
    await expect(pin).toBeHidden();
    await inline.last().getByRole("button", {name: "Send answer", exact: true}).click();
    await expect(inline.last()).toContainText("Answered: custom details");
    await expect(page.locator("#conversation-connection")).toBeHidden();
    assert.deepEqual(errors, []);
    console.log("Optional questions: refresh recovery, radios/notes, carousel, collapse, retry, receipts and mobile passed");
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
