"use strict";
const fs = require("node:fs"), vm = require("node:vm"), assert = require("node:assert/strict");
const {randomUUID} = require("node:crypto");
const helpers = require("./static/app.js");
const source = fs.readFileSync("static/app.js", "utf8");
const section = (start, end) => {
  const a = source.indexOf(start), b = source.indexOf(end, a);
  assert(a > 0 && b > a); return source.slice(a, b);
};
const json = value => JSON.parse(JSON.stringify(value));
function fixture(attachmentIds = []) {
  const records = new Map(), pendingMessages = new Map(), calls = [], failures = [];
  const textarea = {value: "Original message", disabled: false, focus() { throw new Error("Late focus"); }};
  const form = {dataset: {}, querySelectorAll: () => [], elements: {message: textarea}};
  const uploads = {values: [...attachmentIds], locked: false, count() {return this.values.length;},
    ready: () => true, ids() {return [...this.values];}, clear() {this.values = [];}, lock(value) {this.locked = value;}};
  let resolve, reject;
  const client = {message(...args) {calls.push(json(args)); return new Promise((yes, no) => {resolve = yes; reject = no;});}};
  const context = vm.createContext({crypto: {randomUUID}, currentThreadId: "thread-1", conversationID: "conversation", slug: "example",
    client, interactive: true, liveSettingsSaving: false, threadActive: true, messageDispatchInFlight: false,
    form, textarea, composerUploads: uploads, pendingMessages, inFlightMessageIDs: new Set(),
    queueAttemptStorage: {}, sendAttemptStorage: {}, conversationAssets: {sameAttachments: helpersSameAttachments},
    sameAttemptAttachments: helpersSameAttachments, matchingSendAttempt: helpers.matchingSendAttempt,
    requireQueueAttempts: () => [], loadSendAttempts: () => [...records.values()],
    storeSendAttempt: (_store, _slug, thread, attempt) => {records.set(`${thread}/${attempt.id}`, json(attempt)); return true;},
    optionalQuestions: {ordinaryAccepted() {}}, submissionIdentity: (thread, id, action) => ({thread, id, action}),
    showSubmissionFailure: (error, identity) => failures.push({message: error.message, identity}), clearSubmissionFailure() {},
    renderMessageReceipts() {}, updateMessageActions() {}, followTranscript() {}, scheduleRefresh() {},
    applyCurrentSettings() {}, acknowledgeTranscriptMessages() {},
  });
  vm.runInContext("let draftGeneration = 0;\n" +
    section("  const markMessageOutcomeUnknown =", "  const retryMessageReceipt =") +
    section("    const submitMessage = async", "    textarea.addEventListener(\"keydown\"") +
    "\nglobalThis.submit = submitMessage;", context);
  return {context, textarea, uploads, calls, records, pendingMessages, failures,
    accept: () => resolve({steered: true}), lose: () => reject(new Error("Lost response"))};
}
const helpersSameAttachments = (left = [], right = []) => JSON.stringify(left) === JSON.stringify(right);
(async () => {
  for (const kind of ["text", "attachments", "both"]) {
    const ids = kind === "text" ? [] : [randomUUID()];
    const f = fixture(ids); if (kind === "attachments") f.textarea.value = "";
    const original = f.textarea.value;
    const pending = f.context.submit(false);
    assert.equal(f.calls.length, 1); assert.equal(f.textarea.value, ""); assert(!f.textarea.disabled); assert(!f.uploads.locked);
    assert.equal([...f.pendingMessages.values()][0].state, "sending");
    assert.equal([...f.records.values()][0].message, original);
    assert.deepEqual([...f.records.values()][0].attachmentIds || [], ids);
    f.textarea.value = original; f.uploads.values = [...ids];
    await f.context.submit(false); await f.context.submit(true);
    assert.equal(f.calls.length, 1, "held dispatch cannot buffer a send or queue");
    f.accept(); await pending;
    assert.equal(f.textarea.value, original); assert.deepEqual(f.uploads.values, ids);
    assert(!f.context.messageDispatchInFlight);
    const second = f.context.submit(false);
    assert.equal(f.calls.length, 2); assert.notEqual(f.calls[1][1], f.calls[0][1]);
    assert.equal(f.calls[1][2], false, "accepted equal text is new explicit intent");
    f.accept(); await second;
  }
  for (const failure of ["load", "write", "readback", "detach"]) {
    const id = randomUUID(), f = fixture([id]);
    if (failure === "load") f.context.loadSendAttempts = () => null;
    if (["write", "readback"].includes(failure)) f.context.storeSendAttempt = () => false;
    if (failure === "detach") f.uploads.clear = () => {throw new Error("Attachment selection could not be retained");};
    await f.context.submit(false);
    assert.equal(f.calls.length, 0, failure); assert.equal(f.textarea.value, "Original message");
    assert.deepEqual(f.uploads.values, [id]); assert(!f.uploads.locked); assert(!f.context.messageDispatchInFlight);
    assert.equal(f.failures.length, 1);
    if (failure === "detach") assert.equal(f.records.size, 1, "detach failure retains exact recovery identity");
  }
  const f = fixture([randomUUID()]);
  const lost = f.context.submit(false); const original = f.calls[0];
  f.textarea.value = "Next draft"; f.uploads.values = [randomUUID()];
  const nextIDs = [...f.uploads.values]; f.lose(); await lost;
  assert.equal([...f.pendingMessages.values()][0].state, "unknown");
  assert.equal(f.textarea.value, "Next draft"); assert.deepEqual(f.uploads.values, nextIDs);
  f.textarea.value = original[0]; f.uploads.values = original[3];
  const retry = f.context.submit(false);
  assert.equal(f.calls[1][1], original[1]); assert.equal(f.calls[1][2], true);
  f.accept(); await retry;

  const accepted = fixture();
  const persist = accepted.context.storeSendAttempt;
  accepted.context.storeSendAttempt = (...args) => args[3].state === "accepted" ? false : persist(...args);
  const first = accepted.context.submit(false); const firstID = accepted.calls[0][1];
  accepted.accept(); await first;
  accepted.textarea.value = "Original message";
  const again = accepted.context.submit(false);
  assert.notEqual(accepted.calls[1][1], firstID, "known acceptance survives metadata write failure for this page");
  accepted.accept(); await again;

  // A different observed thread cannot receive the old response or clear its draft.
  const switched = fixture(); const request = switched.context.submit(false);
  switched.context.currentThreadId = "member-thread";
  switched.pendingMessages.clear(); switched.textarea.value = "Member draft";
  switched.context.client = {message() {throw new Error("Response rerouted");}};
  switched.accept(); await request;
  assert.equal(switched.pendingMessages.size, 0); assert.equal(switched.textarea.value, "Member draft");
  assert.equal([...switched.records.keys()].every(key => key.startsWith("thread-1/")), true);
  console.log("Composer handoff: one HTTP dispatch, exact durable identity, early draft ownership and late-proof isolation passed.");
})().catch(error => {console.error(error); process.exitCode = 1;});
