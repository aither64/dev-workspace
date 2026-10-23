"use strict";
const assert = require("node:assert/strict");
const {chromium, expect} = require("@playwright/test");
const baseURL = process.argv[2];
(async () => {
  const browser = await chromium.launch({headless: true, channel: "chromium"});
  let diagnose = null;
  try {
    const page = await browser.newPage({ignoreHTTPSErrors: true}), errors = [], responses = [], dialogs = [];
    let pending = [], acceptUnanswered = false, phase = "keyboard";
    diagnose = async () => ({fixture: "required-question-keyboard", phase, pageErrors: errors,
      responses: responses.length, dialogs: dialogs.length, controls: await page.locator(".input-wizard").evaluateAll(forms => forms.map(form => ({
        question: form.querySelector(".wizard-question")?.textContent,
        radios: [...form.querySelectorAll('[name="answer-choice"]')].map(input => ({value: input.value, checked: input.checked,
          label: input.closest("label")?.textContent})),
        next: form.querySelector('[name="question-next"]')?.textContent,
      })))});
    page.on("pageerror", error => errors.push(error.message));
    page.on("dialog", async dialog => {
      dialogs.push(dialog.message());
      if (dialog.type() === "confirm" && !acceptUnanswered) await dialog.dismiss(); else await dialog.accept();
    });
    await page.addInitScript(() => {
      window.EventSource = class {
        constructor() {queueMicrotask(() => this.onopen?.());}
        addEventListener() {} close() {}
      };
    });
    await page.route("**/api/sessions/example/**", async route => {
      const operation = new URL(route.request().url()).pathname.split("/").at(-1), json = data => route.fulfill({json: data});
      if (operation === "thread" || operation === "page") return json({threadId: "thread-1", latestTurnId: "turn-1", status: "active",
        collaborationMode: "default", model: "model-1", reasoningEffort: "medium", entries: [], hasOlder: false});
      if (operation === "pending") return json(pending);
      if (operation === "queue") return json([]);
      if (operation === "reconcile") return json({ok: true});
      if (operation === "activity") return json({currentState: "working", coverageComplete: true});
      if (operation === "details") return json({repositoriesHTML: "", artifactsHTML: ""});
      if (operation === "respond") {responses.push(route.request().postDataJSON()); pending = []; return json({ok: true});}
      return route.continue();
    });
    const question = id => ({id, token: "offer-" + id, threadId: "thread-1", turnId: "turn-1", itemId: id,
      method: "item/tool/requestUserInput", kind: "userInput", authorityAvailable: true, isBlocking: true,
      questions: [{id: "choice", header: "First", question: "Which approach?", isOther: true,
        options: [{label: "First", description: "First approach"}]},
      {id: "second", header: "Second", question: "Additional detail?"},
      {id: "third", header: "Third", question: "Final detail?"}]});
    const wizard = page.locator(".input-wizard"), field = wizard.locator('[name="answer-note"]');
    const step = name => expect(wizard.locator(".wizard-question")).toHaveText(name);
    const refresh = () => page.evaluate(() => dispatchEvent(new Event("focus")));
    pending = [question("keyboard")];
    await page.goto(baseURL + "/example/#codex");
    await step("Which approach?");
    await wizard.getByRole("radio", {name: "None of the above"}).check();
    await field.press("Enter");
    await expect.poll(() => dialogs.length).toBe(1);
    assert.match(dialogs[0], /Type your own answer/); await step("Which approach?");
    await field.fill("First answer");
    await field.press("Shift+Enter"); await expect(field).toHaveValue("First answer\n");
    for (const key of ["Control+Enter", "Alt+Enter", "Meta+Enter"]) await field.press(key);
    await field.evaluate(node => {
      node.dispatchEvent(new CompositionEvent("compositionstart", {bubbles: true}));
      node.dispatchEvent(new KeyboardEvent("keydown", {key: "Enter", bubbles: true, cancelable: true}));
      node.dispatchEvent(new CompositionEvent("compositionend", {bubbles: true}));
      for (const extra of [{isComposing: true}, {keyCode: 229}, {repeat: true}])
        node.dispatchEvent(new KeyboardEvent("keydown", {key: "Enter", bubbles: true, cancelable: true, ...extra}));
    });
    await step("Which approach?"); assert.equal(responses.length, 0);
    for (const attribute of ["readOnly", "disabled", "hidden"]) {
      await field.evaluate((node, attribute) => {
        node[attribute] = true;
        node.dispatchEvent(new KeyboardEvent("keydown", {key: "Enter", bubbles: true, cancelable: true}));
        node[attribute] = false;
      }, attribute);
      await step("Which approach?");
    }
    await wizard.locator('[name="question-next"]').evaluate(node => {node.disabled = true;});
    await field.press("Enter"); await step("Which approach?");
    await wizard.locator('[name="question-next"]').evaluate(node => {node.disabled = false;});
    await field.press("Enter"); await step("Additional detail?");
    assert.equal(responses.length, 0, "Enter advances exactly one page");
    await page.reload(); await step("Additional detail?");
    await field.press("Enter"); await step("Final detail?");
    await field.fill("Last answer"); await field.press("Enter");
    await expect.poll(() => dialogs.length).toBe(2);
    assert.match(dialogs[1], /1 question is unanswered/); await step("Final detail?");
    assert.equal(responses.length, 0);
    acceptUnanswered = true; await field.press("Enter");
    await expect(wizard).toHaveCount(0); assert.equal(responses.length, 1);
    assert.deepEqual(responses[0].answers, {choice: {answers: ["user_note: First answer"]}, second: {answers: []}, third: {answers: ["user_note: Last answer"]}});

    // Mouse actions use the same requestSubmit path exactly once per page.
    phase = "mouse-actions";
    pending = [question("click")]; await refresh(); await step("Which approach?");
    const first = wizard.locator('input[name="answer-choice"][value="First"]');
    await first.check(); await expect(first).toBeChecked();
    await wizard.getByRole("button", {name: "Next", exact: true}).click(); await step("Additional detail?");
    assert.equal(responses.length, 1);
    await field.fill("Second click answer");
    await wizard.getByRole("button", {name: "Next", exact: true}).click(); await step("Final detail?");
    await field.fill("Final click answer");
    await wizard.getByRole("button", {name: "Submit answers", exact: true}).click();
    await expect(wizard).toHaveCount(0); assert.equal(responses.length, 2);
    assert.deepEqual(errors, []);
    console.log("Required question wizard: Enter, newline, IME/modifiers, validation, saved progression and exactly-once click/submit passed.");
  } catch (error) {
    try {if (diagnose) console.error(JSON.stringify(await diagnose()));} catch (_) {}
    throw error;
  } finally {await browser.close();}
})().catch(error => {console.error(error); process.exitCode = 1;});
