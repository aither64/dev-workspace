"use strict";
const fs = require("node:fs"), vm = require("node:vm"), assert = require("node:assert/strict");
(async () => {
  const source = fs.readFileSync("static/app.js", "utf8");
  const start = source.indexOf('  document.addEventListener("submit", async (event) => {\n    const form = event.target.closest("[data-direct-team-form]");');
  const end = source.indexOf('  document.addEventListener("change", (event) => {', start);
  assert(start > 0 && end > start);
  let submit, release, fail = false, requests = [], refreshes = 0;
  const controls = [{disabled:false}], status = {}, form = {
    dataset:{action:"add"}, elements:{role:{value:"implementer"}, model:{value:"model-1"}, effort:{value:"xhigh"}},
    querySelector: () => status, querySelectorAll: () => controls,
  };
  const context = {interactive:true, models:[{model:"model-1", supportedReasoningEfforts:[{reasoningEffort:"xhigh"}]}],
    document:{addEventListener: (type, callback) => {if (type === "submit") submit = callback;}},
    updateAddMemberEligibility: () => {}, refreshSessionDetails: () => {refreshes++;},
    directTeamRequest: async body => {requests.push({...body}); await new Promise(resolve => {release = resolve;}); if (fail) throw new Error("fixture failure");},
    setTimeout: () => {throw new Error("Add must not reload/reset the form");},
  };
  vm.runInNewContext(source.slice(start, end), context);
  const event = {target:{closest: () => form}, preventDefault(){}};
  const pending = submit(event); await submit(event);
  assert.equal(requests.length,1); assert(controls[0].disabled);
  // A delayed response cannot retarget the already captured request.
  form.elements.effort.value = "low";
  release(); await pending;
  assert.equal(requests[0].reasoningEffort,"xhigh"); assert.equal(form.elements.effort.value,"xhigh");
  assert.equal(form.elements.model.value,"model-1"); assert.equal(form.elements.role.value,"implementer");
  assert(!controls[0].disabled); assert.equal(refreshes,1);
  const next = submit(event); release(); await next;
  assert.equal(requests.length,2); assert.deepEqual(requests[1],requests[0]);
  fail = true; const failure = submit(event); release(); await failure;
  assert.equal(form.elements.effort.value,"xhigh"); assert.equal(status.textContent,"fixture failure");
  assert(!controls[0].disabled); assert(!form.dataset.teamSubmitting);
  form.elements.effort.value = "unsupported"; await submit(event); assert.equal(requests.length,3);
  console.log("Add-member captured settings, duplicate submit and sequential/error retention passed.");
})().catch(error => {console.error(error); process.exitCode=1;});
