"use strict";
const assert = require("node:assert/strict"), vm = require("node:vm");
const {randomUUID} = require("node:crypto");
const {fixture, storage, settle, section} = require("./composer_handoff_contract_test.cjs");
const transient = () => Object.assign(new Error("Connection was unavailable"), {
  status: 503, code: "message_not_sent", notSent: true, retryable: true,
});

module.exports = async assets => {
  assert.equal(typeof assets.createDeliveryRetryScheduler, "function");
  function retries(f) {
    const timers = new Map(), plans = [], planReplies = [], waits = [];
    let sequence = 0, beforeRetry = null;
    f.context.document.hidden = false;
    f.context.document.getElementById = () => ({addEventListener() {}});
    f.context.planActions = {dataset: {planTurnId: "plan-turn", planSha256: "a".repeat(64)}};
    f.context.planRenderGeneration = 0;
    f.context.recordSettingsWrite = () => {};
    f.context.confirmMode = () => {};
    f.context.planIdentity = (...args) => args.join(":");
    f.context.client.implementPlan = request => {
      plans.push(JSON.parse(JSON.stringify(request)));
      return new Promise((resolve, reject) => planReplies.push({resolve, reject}));
    };
    f.context.conversationAssets.createDeliveryRetryScheduler = options => assets.createDeliveryRetryScheduler({
      ...options,
      setTimer(run, delay) {const id = ++sequence; timers.set(id, {run, delay}); return id;},
      clearTimer(id) {timers.delete(id);},
      retry(id) {beforeRetry?.(); beforeRetry = null; return options.retry(id);},
    });
    vm.runInContext(section("  async function retryPlanImplementation(", "  const planSessionDialog ="), f.context);
    return {
      timers, plans,
      async fire() {
        assert(timers.size, "a retry must remain scheduled");
        const [id, timer] = timers.entries().next().value;
        timers.delete(id); waits.push(timer.run()); await settle(); return timer.delay;
      },
      before(run) {beforeRetry = run;},
      async acceptPlan() {planReplies.shift().resolve({steered: false}); await settle();},
      async losePlan(error = transient()) {planReplies.shift().reject(error); await settle();},
      stop() {f.context.deliveryRetries.destroy();},
    };
  }

  // A typed transient head retries its immutable snapshot through FIFO and
  // retains the deployed backoff and hidden/offline/busy gates.
  let f = fixture([randomUUID()]), r = retries(f);
  const originalFiles = [...f.uploads.values];
  await f.context.submit(false);
  const id = f.calls[0].args[1];
  f.textarea.value = "Queue after retry"; await f.context.submit(true);
  f.textarea.value = "Current draft"; f.uploads.values = [randomUUID()];
  const draftFiles = [...f.uploads.values];
  await f.lose(transient());
  assert.equal(f.context.messageDispatcher.entries[0].state, "unsent");
  assert.equal(f.context.messageDispatcher.entries[0].retryable, true);
  assert.equal(f.records.get(id).retryable, true);
  assert.equal(await r.fire(), 250);
  assert.deepEqual(f.calls[1], {action: "send", args: ["Original message", id, true, originalFiles]});
  await f.lose(transient());
  for (const [gate, release] of [
    [() => {f.context.document.hidden = true;}, () => {f.context.document.hidden = false;}],
    [() => {f.context.navigator.onLine = false;}, () => {f.context.navigator.onLine = true;}],
    [() => {f.context.liveSettingsSaving = true;}, () => {f.context.liveSettingsSaving = false;}],
    [() => {f.context.planImplementationInFlight = true;}, () => {f.context.planImplementationInFlight = false;}],
  ]) {
    gate(); await r.fire(); assert.equal(f.calls.length, 2); release();
  }
  assert.equal(await r.fire(), 5000);
  assert.equal(f.calls.length, 3);
  await f.accept();
  assert.equal(f.calls[3].action, "queue", "the successor dispatches only after positive retry acceptance");
  await f.accept();
  assert.equal(f.textarea.value, "Current draft"); assert.deepEqual(f.uploads.values, draftFiles);
  assert.equal(r.timers.size, 0); r.stop();

  // Durable unsent/retryable proof survives reload, without changing the draft.
  f = fixture([randomUUID()]); r = retries(f);
  await f.context.submit(false); await f.lose(transient());
  const restoredID = f.calls[0].args[1], originalRecord = f.records.get(restoredID); r.stop();
  f = fixture([], f.saved, f); r = retries(f);
  f.textarea.value = "Restored draft";
  assert.equal(f.calls.length, 0);
  await r.fire();
  assert.equal(f.calls[0].args[1], restoredID); assert.equal(f.calls[0].args[2], true);
  assert.deepEqual(f.calls[0].args.at(-1), originalRecord.attachmentIds);
  await f.accept(); assert.equal(f.textarea.value, "Restored draft"); r.stop();

  // An UNKNOWN buffered head holds automatic legacy retries. Once the existing
  // FIFO is resolved, a legacy transient may adopt the empty owner with its ID.
  const legacyID = randomUUID(), legacy = {id: legacyID, message: "Legacy text", attachmentIds: [randomUUID()],
    state: "unsent", retryable: true, steered: true};
  f = fixture([], storage(), {records: new Map([[legacyID, legacy]])}); r = retries(f);
  await f.context.submit(false);
  const heldID = f.calls[0].args[1];
  f.textarea.value = "Waiting successor"; await f.context.submit(false);
  await f.lose();
  await r.fire();
  assert.equal(f.calls.length, 1); assert.equal(f.context.messageDispatcher.entries[0].id, heldID);
  assert.equal(f.context.messageDispatcher.entries.length, 2);
  f.textarea.value = "Unchanged legacy draft";
  const manual = f.context.retry(heldID); await settle(); await f.accept(); await manual;
  await f.accept();
  await r.fire();
  assert.equal(f.calls.at(-1).args[1], legacyID); assert.equal(f.calls.at(-1).args[2], true);
  assert.equal(f.calls.at(-1).args[0], legacy.message); assert.deepEqual(f.calls.at(-1).args.at(-1), legacy.attachmentIds);
  assert.equal(f.context.messageDispatcher.entries[0].steered, true);
  assert.equal(f.textarea.value, "Unchanged legacy draft");
  await f.accept(); r.stop();

  // Recheck state and owner availability after the scheduler's availability
  // check. Stale positive/UNKNOWN evidence and a newly captured intent win.
  for (const state of ["accepted", "observed", "unknown"]) {
    f = fixture([], storage(), {records: new Map([[legacyID, {...legacy}]])}); r = retries(f);
    r.before(() => f.pendingMessages.set(legacyID, {...legacy, state}));
    await r.fire();
    assert.equal(f.calls.length, 0, state); assert.equal(f.context.messageDispatcher.entries.length, 0);
    assert.equal(f.pendingMessages.get(legacyID).state, state); assert.equal(f.textarea.value, "Original message");
    r.stop();
  }
  f = fixture([], storage(), {records: new Map([[legacyID, {...legacy}]])}); r = retries(f);
  const newer = {id: randomUUID(), message: "New capture", action: "send", attachmentIds: []};
  r.before(() => f.context.messageDispatcher.capture(newer));
  await r.fire();
  assert.equal(f.calls.length, 0); assert.equal(f.context.messageDispatcher.entries.length, 1);
  assert.equal(f.context.messageDispatcher.entries[0].id, newer.id); r.stop();
  f = fixture([], storage(), {records: new Map([[legacyID, {...legacy}]])}); r = retries(f);
  f.saved.setItem = () => {throw new Error("Storage full");};
  await r.fire();
  assert.equal(f.calls.length, 0); assert.equal(f.context.messageDispatcher.entries.length, 0);
  assert.equal(f.records.get(legacyID).id, legacyID); assert.equal(f.textarea.value, "Original message"); r.stop();

  // Plan retries keep the plan context and native endpoint. New ordinary
  // drafts can be captured during the request but cannot overlap its HTTP.
  const planID = randomUUID(), plan = {id: planID, message: "Implement the plan.", context: `plan:plan-turn:${"a".repeat(64)}`,
    state: "unsent", retryable: true};
  f = fixture([], storage(), {records: new Map([[planID, plan]])}); r = retries(f);
  await r.fire();
  assert.deepEqual(r.plans[0], {action: "same", planContextVersion: 2, planTurnId: "plan-turn", planSha256: "a".repeat(64), clientUserMessageId: planID});
  const statusReadID = randomUUID();
  f.pendingMessages.set(statusReadID, {...plan, id: statusReadID, state: "unknown", retryable: false});
  f.context.inFlightMessageIDs.add(statusReadID);
  await f.context.submit(false);
  assert.equal(f.calls.length, 0, "a captured ordinary message waits for the plan request");
  f.textarea.value = "Draft while plan retries";
  await r.losePlan();
  assert.equal(f.calls.length, 1, "ordinary waiting intent can run after the plan HTTP settles");
  f.context.inFlightMessageIDs.delete(statusReadID);
  await r.fire(); assert.equal(r.plans.length, 1, "the retry waits for ordinary HTTP");
  await f.accept();
  await r.fire(); assert.equal(r.plans.length, 2); assert.equal(r.plans[1].clientUserMessageId, planID);
  await r.acceptPlan();
  assert.equal(f.pendingMessages.get(planID).state, "accepted");
  assert.equal(f.textarea.value, "Draft while plan retries"); r.stop();
  console.log("Composer retries: durable transient FIFO, legacy admission, stale evidence, draft and plan context passed.");
};
