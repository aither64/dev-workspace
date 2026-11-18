"use strict";
const fs = require("node:fs"), vm = require("node:vm"), assert = require("node:assert/strict");
(async () => {
  const source = fs.readFileSync("static/app.js", "utf8");
  const section = (first, last) => {
    const start = source.indexOf(first), end = source.indexOf(last, start);
    assert(start > 0 && end > start);
    return source.slice(start, end);
  };
  let submit, release, resolveCatalog, rejectCatalog, fail = false, refreshes = 0, uuidCalls = 0, modelReads = 0;
  const requests = [], bodies = [], status = {}, button = {type:"submit", disabled:false, textContent:"Add member"};
  const field = value => ({value, disabled:false, dataset:{}});
  const elements = {role:field("implementer"), model:field("model-1"), effort:field("xhigh")};
  const controls = [...Object.values(elements), button], form = {
    id:"team-add", dataset:{action:"add"}, elements,
    querySelector: selector => selector.includes("submit") ? button : status,
    querySelectorAll: selector => selector === "[data-model-select]" ? [elements.model] : controls,
  };
  elements.model.closest = () => form;
  elements.model.replaceChildren = () => assert.fail("An unresolved addition must retain its visible options");
  const models = [{model:"model-1", supportedReasoningEfforts:[{reasoningEffort:"xhigh"}, {reasoningEffort:"low"}]}];
  const context = {interactive:true, piBackend:false, codexBackend:false, models,
    document:{addEventListener: (type, callback) => {if (type === "submit") submit = callback;},
      querySelector: () => form, getElementById: () => null},
    refreshSessionDetails: () => {refreshes++;},
    directTeamRequest: async body => {
      requests.push({...body}); bodies.push(JSON.stringify(body));
      await new Promise(resolve => {release = resolve;}); if (fail) throw new Error("fixture failure");
    },
    setTimeout: () => {throw new Error("Add must not reload/reset the form");},
    crypto:{randomUUID: () => `11111111-1111-4111-8111-${String(++uuidCalls).padStart(12,"0")}`},
    modelCatalogs:new Map(), selectModelCatalogs:new WeakMap(), selectModelURLs:new WeakMap(),
    client:{models: () => {modelReads++; return new Promise((resolve, reject) => {resolveCatalog = resolve; rejectCatalog = reject;});}},
    isPiCreation: () => false, creationSelectionEpoch: () => 0,
    pairedEffortSelect: () => elements.effort,
    restoreManagedCreationDrafts: () => {}, updateCreationCLI: () => {},
  };
  vm.runInNewContext([
    section("  const restorePiAdditionSelection =", "  const bindModelSelects ="),
    section("  const updateAddMemberEligibility =", '  document.addEventListener("change", (event) => {'),
    section('  document.addEventListener("submit", async (event) => {\n    const form = event.target.closest("[data-direct-team-form]");', '  document.addEventListener("change", (event) => {'),
    "globalThis.addControls = {updateAddMemberEligibility, applyMemberDefaults, applyCurrentSettings, loadModels};",
  ].join("\n"), context);
  const event = {target:{closest: () => form}, preventDefault(){}};
  const pending = submit(event); await submit(event);
  assert.equal(requests.length,1); assert(controls.every(control => control.disabled));
  // A delayed response cannot retarget the already captured request.
  elements.effort.value = "low";
  release(); await pending;
  assert.equal(requests[0].reasoningEffort,"xhigh"); assert.equal(elements.effort.value,"xhigh");
  assert.equal(elements.model.value,"model-1"); assert.equal(elements.role.value,"implementer");
  assert(controls.every(control => !control.disabled)); assert.equal(refreshes,1);
  const next = submit(event); release(); await next;
  assert.equal(requests.length,2); assert.deepEqual(requests[1],requests[0]);
  fail = true; const failure = submit(event); release(); await failure;
  assert.equal(elements.effort.value,"xhigh"); assert.equal(status.textContent,"fixture failure");
  assert(controls.every(control => !control.disabled)); assert(!form.dataset.teamSubmitting);
  elements.effort.value = "unsupported"; await submit(event); assert.equal(requests.length,3);

  context.piBackend = true;
  const capturedVisible = () => {
    assert.equal(elements.role.value,"implementer"); assert.equal(elements.model.value,"model-1");
    assert.equal(elements.effort.value,"xhigh");
    assert(Object.values(elements).every(control => control.disabled));
  };
  for (const catalogFails of [false, true]) {
    elements.effort.value = "xhigh"; context.modelCatalogs.clear();
    // Start a catalog refresh before capture, then finish it after the failed Add.
    const catalog = context.addControls.loadModels(form);
    fail = true; const uncertain = submit(event); release(); await uncertain;
    const retained = requests.at(-1), retainedBody = bodies.at(-1), captured = form.dataset.piAddition;
    assert(retained.messageId); assert(captured); capturedVisible();
    assert.equal(status.textContent,"Could not confirm the addition. Retry with the same selection. fixture failure");
    assert.equal(button.textContent,"Retry addition"); assert(!button.disabled); assert(!form.dataset.teamSubmitting);
    // Eligibility, role defaults and current settings cannot unlock or retarget it.
    for (const refresh of [context.addControls.updateAddMemberEligibility,
      () => context.addControls.applyMemberDefaults(true), () => context.addControls.applyCurrentSettings(form,true)]) {
      elements.role.value = "reviewer"; elements.model.value = "model-2"; elements.effort.value = "low";
      Object.values(elements).forEach(control => {control.disabled = false;});
      refresh(); capturedVisible(); assert.equal(form.dataset.piAddition,captured);
    }
    if (catalogFails) rejectCatalog(new Error("catalog unavailable"));
    else resolveCatalog([{model:"model-2", supportedReasoningEfforts:[{reasoningEffort:"low"}]}]);
    await catalog; capturedVisible(); assert.equal(form.dataset.piAddition,captured);
    const reads = modelReads; await context.addControls.loadModels(form);
    assert.equal(modelReads,reads); capturedVisible();
    // A catalog change must not block enquiry/retry of the already captured body.
    context.models = []; context.addControls.updateAddMemberEligibility(); assert(!button.disabled);
    const calls = uuidCalls; fail = false; const replay = submit(event); await submit(event);
    assert.equal(uuidCalls,calls); assert.equal(bodies.at(-1),retainedBody); assert.deepEqual(requests.at(-1),retained);
    assert(controls.every(control => control.disabled));
    context.models = models; release(); await replay;
    assert(!form.dataset.piAddition); assert(!form.dataset.teamSubmitting);
    assert(controls.every(control => !control.disabled)); assert.equal(button.textContent,"Add member");
    assert.equal(status.textContent,"Saved.");
  }
  assert.equal(uuidCalls,2); // Success unlocks a genuinely new addition UUID.
  console.log("Add-member capture, locked visible Pi retry, refresh protection and success reset passed.");
})().catch(error => {console.error(error); process.exitCode=1;});
