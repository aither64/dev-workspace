"use strict";
const fs = require("node:fs"), vm = require("node:vm"), assert = require("node:assert/strict");
const {randomUUID, createHash} = require("node:crypto");
const helpers = require("./static/app.js");
const source = fs.readFileSync("static/app.js", "utf8");
let completed = false;
if (require.main === module) process.on("beforeExit", () => {if (!process.exitCode) assert(completed, "composer contracts must finish every awaited recovery");});
const section = (start, end) => {
  const a = source.indexOf(start), b = source.indexOf(end, a);
  assert(a > 0 && b > a); return source.slice(a, b);
};
const json = value => JSON.parse(JSON.stringify(value));
const settle = async () => {for (let index = 0; index < 10; index++) await Promise.resolve();};
const storage = () => ({values: new Map(), get length() {return this.values.size;}, key(index) {return [...this.values.keys()][index] ?? null;},
  getItem(key) {return this.values.get(key) || null;}, setItem(key, value) {this.values.set(key, value);}, removeItem(key) {this.values.delete(key);}});
function fixture(attachmentIds = [], saved = storage(), retained = {}) {
  const records = retained.records || new Map(), queues = retained.queues || new Map(), pendingMessages = new Map(), calls = [], failures = [], held = [], readWakes = [];
  for (const entry of records.values()) pendingMessages.set(entry.id, {...entry, state: entry.state || "unknown"});
  const textarea = {value: "Original message", disabled: false, focus() {throw new Error("Late focus");}};
  const form = {dataset: {}, elements: {message: textarea}};
  const uploads = {values: [...attachmentIds], locked: false, count() {return this.values.length;},
    ready: () => true, ids() {return [...this.values];}, clear() {this.values = [];}, lock(value) {this.locked = value;}};
  const dispatch = action => (...args) => {
    calls.push({action, args: json(args)}); return new Promise((resolve, reject) => held.push({resolve, reject, id: args[1]}));
  };
  const context = vm.createContext({portalDeliverySupported:false, deliveryRetries: null, crypto: {randomUUID}, currentThreadId: "thread-1", conversationID: "conversation", slug: "example",
    client: {message: dispatch("send"), queueMessage: dispatch("queue")}, interactive: true, executionHeld: false,
    canSubmitExplicitInput: () => context.interactive && !context.inputBlockedReason,
    piBackend: retained.pi === true, isTerminalPiSend: helpers.isTerminalPiSend, isPiDelivery: helpers.isPiDelivery, isPendingPiSend: helpers.isPendingPiSend, piReady: true,
    piInputDeliverySupported: retained.delivery === true, reconcilePiPendingMessages: null,
    piSendReady: () => !context.piBackend || context.piReady, consumePiIdle: () => { if (context.piBackend) context.piReady = false; },
    piDeliveryReady: () => context.piBackend && context.piInputDeliverySupported && context.piReady,
    piAttemptReady: () => !context.piBackend || context.piReady,
    composerUploadReady: true, liveSettingsSaving: false, threadActive: true, messageDispatchInFlight: false, planImplementationInFlight: false, messageDispatcher: null,
    document: {hidden: false}, navigator: {onLine: true}, pageReads: {paused: false}, dispatchReadReady: true,
    retryFailedMessage: null, discardFailedMessage: null, dropPendingMessage: null, replaceFailedWithDraft: null, enqueueMessageCopy: null, confirm: () => true, failedMessageDrafts: new Map(),
    form, textarea, composerUploads: uploads, pendingMessages, receiptEnquiries: new Set(), inFlightMessageIDs: new Set(),
    queueAttemptStorage: {}, sendAttemptStorage: Object.assign(saved, retained.pi ? {conversationScope:"fixture-pi"} : {}), createMessageDispatch: helpers.createMessageDispatch,
    conversationAssets: {sameAttachments: (a = [], b = []) => JSON.stringify(a) === JSON.stringify(b)},
    sameAttemptAttachments: (a = [], b = []) => JSON.stringify(a) === JSON.stringify(b), matchingSendAttempt: helpers.matchingSendAttempt,
    requireQueueAttempts: () => [...queues.values()], loadSendAttempts: () => [...records.values()],
    storeSendAttempt: (_store, _slug, thread, attempt) => {records.set(attempt.id, json(attempt)); return true;},
    storeQueueAttempt: (_store, _slug, thread, attempt) => {queues.set(attempt.id, json(attempt)); return true;},
    deleteQueueAttempt: (_store, _slug, thread, id) => {queues.delete(id); return true;},
    deleteSendAttempt: (_store, _slug, thread, id) => {records.delete(id); return true;},
    beginSubmission: identity => identity,
    optionalQuestions: {ordinaryAccepted() {}}, submissionIdentity: (thread, id, action) => ({thread, id, action}),
    showSubmissionFailure: (error, identity) => failures.push({message: error.message, notSent: error.notSent, identity}), clearSubmissionFailure() {},
    renderMessageReceipts() {}, updateMessageActions() {}, followTranscript() {}, scheduleRefresh(delay) {readWakes.push(delay);}, refreshQueue() {}, refreshSessionDetails() {},
    applyCurrentSettings() {}, acknowledgeTranscriptMessages() {},
  });
  const queueSaved = retained.queueSaved || storage();
  if (retained.assets) {
    helpers.configureDurableAttemptStore(retained.assets.createDurableAttemptStore);
    context.queueAttemptStorage = Object.assign(queueSaved, retained.pi ? {conversationScope: "fixture-pi"} : {});
    context.requireQueueAttempts = helpers.requireQueueAttempts;
    context.loadSendAttempts = helpers.loadSendAttempts;
    context.storeSendAttempt = helpers.storeSendAttempt;
    context.storeQueueAttempt = helpers.storeQueueAttempt;
    context.deleteSendAttempt = helpers.deleteSendAttempt;
    context.deleteQueueAttempt = helpers.deleteQueueAttempt;
    pendingMessages.clear();
    for (const entry of helpers.loadSendAttempts(context.sendAttemptStorage, "example", "thread-1") || []) pendingMessages.set(entry.id, {...entry, state: entry.state || "unknown"});
  }
  vm.runInContext("let draftGeneration = 0;\n" +
    section("  const markMessageOutcomeUnknown =", "  const recoverPlanAttempts =") +
    section("    const dispatchThreadID =", "    textarea.addEventListener(\"keydown\"") +
    "\nglobalThis.submit = submitMessage; globalThis.retry = retryMessageReceipt; globalThis.resend = resendMessageReceipt;", context);
  return {context, textarea, uploads, calls, records, queues, pendingMessages, failures, saved, queueSaved, readWakes,
    accept: async () => {const reply = held.shift(); reply.resolve({steered: true, clientUserMessageId: reply.id, turnId: "accepted"}); await settle();},
    lose: async error => {held.shift().reject(error || new Error("Lost response")); await settle();}};
}
function receiptButtons(f) {
  if (!f.context.testRenderReceipts) {
    const element = tag => ({tag, children: [], get childElementCount() {return this.children.length;}, dataset: {}, textContent: "", append(...nodes) {this.children.push(...nodes);},
      replaceChildren(...nodes) {this.children=nodes;}, setAttribute() {}, addEventListener() {}});
    f.receiptContainer=element("div");
    f.context.document={activeElement:null,getElementById:()=>f.receiptContainer,createElement:element};
    f.context.messageReceiptLabel=helpers.messageReceiptLabel;
    f.context.messageReceiptDescription=helpers.messageReceiptDescription;
    vm.runInContext(section("  const renderMessageReceipts =", "  loadMessageReceipts();")+
      "globalThis.testRenderReceipts=renderMessageReceipts;",f.context);
  }
  f.context.testRenderReceipts();
  return f.receiptContainer.children.map(item=>{
    const buttons=item.children.find(child=>child.className==="message-receipt-actions")?.children || [];
    return {label:item.children[0].textContent, buttons:buttons.map(button=>button.textContent),
      disabled:Object.fromEntries(buttons.map(button=>[button.textContent,Boolean(button.disabled)]))};
  });
}
module.exports = {fixture, storage, settle, section};
if (require.main === module) (async () => {
  if (process.argv[2] === "--communication") {
    await communicationPresentation(JSON.parse(fs.readFileSync(0, "utf8")));
    completed = true;
    return;
  }
  assert.equal(helpers.messageReceiptLabel({state:"pending",delivery:"steer"},"pi"),"Steer pending");
  assert.equal(helpers.messageReceiptLabel({state:"pending",delivery:"followUp"},"pi"),"Follow-up queued");
  assert.equal(helpers.messageReceiptDescription({state:"pending",delivery:"steer"}),"Waiting for Pi to accept the steer.");
  assert.equal(helpers.messageReceiptDescription({state:"pending",delivery:"followUp"}),"Waiting for Pi to accept the follow-up.");
  // Pi intentionally has no Codex pending root. Exercise the shared plan and
  // focus updates with that exact null, as well as the Codex question lane.
  for (const codex of [false, true]) {
    const document = {body: {}, activeElement: null};
    const textarea = {focus() {document.activeElement = this;}};
    const keepPlanning = {focus() {document.activeElement = this;}};
    const interrupt = {disabled: false, focus() {document.activeElement = this;}};
    const home = {append(node) {node.parentElement = this;}};
    home.append(interrupt);
    const chat = {classList: {toggle(_name, value) {chat.planDecision = value;}}};
    const form = {ownerDocument: document, hidden: false, closest: () => chat,
      contains: node => node === textarea || node === interrupt && interrupt.parentElement === home,
      querySelector: selector => selector === "#interrupt" ? interrupt : textarea,
      querySelectorAll: () => []};
    const panel = {hidden: true, contains: node => node === keepPlanning, querySelector: () => keepPlanning};
    const heading = {append(node) {node.parentElement = this;}};
    const answer = {focus() {document.activeElement = this;}};
    const question = {querySelector: selector => selector === ".question-heading" ? heading : answer};
    let entries = [];
    const pending = codex ? {querySelector: () => entries[0] || null, contains: () => false,
      replaceChildren(...nodes) {entries = nodes;}} : null;
    const view = helpers.createComposerView(panel, form, pending);
    document.activeElement = document.body;
    view.setPlanVisible(false); // The Pi workflow's ordinary refresh callback.
    assert.equal(form.hidden, false); assert.equal(panel.hidden, true);
    view.setPlanVisible(true, true);
    assert.equal(form.hidden, true); assert.equal(panel.hidden, false);
    assert.equal(chat.planDecision, true); assert.equal(document.activeElement, keepPlanning);
    view.setPlanVisible(false, true);
    assert.equal(form.hidden, false); assert.equal(panel.hidden, true);
    assert.equal(chat.planDecision, false); assert.equal(document.activeElement, textarea);
    document.activeElement = interrupt;
    view.setInterruptEnabled(false);
    assert.equal(interrupt.disabled, true); assert.equal(document.activeElement, textarea);
    if (codex) {
      view.replacePending([question]);
      assert.equal(form.hidden, true); assert.equal(panel.hidden, true);
      assert.equal(interrupt.parentElement, heading); assert.equal(document.activeElement, answer);
      view.setPlanVisible(true);
      assert.equal(panel.hidden, true, "a Codex question takes precedence over the plan");
      view.replacePending([]);
      assert.equal(panel.hidden, false); assert.equal(form.hidden, true);
      assert.equal(interrupt.parentElement, home);
      view.setPlanVisible(false, true);
      assert.equal(form.hidden, false); assert.equal(document.activeElement, textarea);
    }
  }
  // Known-unsent restoration never activates a held session. Its explicit
  // input admission remains separate from automatic current-read continuation.
  for (const state of ["waiting", "unknown"]) {
    const id = randomUUID(), attachment = randomUUID();
    const original = {id, message: "say hi", action: "send", context: "", attachmentIds: [attachment], state};
    const saved = storage();
    saved.setItem("workspace-portal.message-dispatch.example.thread-1", JSON.stringify({schema: 1, entries: [original]}));
    const f = fixture([], saved, {records: new Map([[id, original]])});
    f.context.executionHeld = true;
    f.textarea.value = "Keep this draft";
    await f.context.messageDispatcher.drain();
    assert.equal(f.calls.length, 0, "recovery refresh cannot drain restored intent");
    f.context.inputBlockedReason = "Saved queued messages must be handled first.";
    await f.context.retry(id);
    assert.equal(f.calls.length, 0, "a known blocker prevents even explicit activation");
    f.context.inputBlockedReason = "";
    const resume = f.context.retry(id); await settle();
    assert.equal(f.calls.length, 1);
    assert.equal(f.calls[0].args[0], original.message);
    assert.equal(f.calls[0].args[1], id);
    assert.equal(f.calls[0].args[2], state === "unknown");
    assert.deepEqual(f.calls[0].args.at(-1), original.attachmentIds);
    assert.equal(f.textarea.value, "Keep this draft");
    await f.accept(); await resume;
  }
  const cold = fixture([randomUUID()]); cold.context.executionHeld = true;
  await cold.context.submit(false);
  assert.equal(cold.calls.length, 1, "one fresh explicit send reaches guarded server admission while cold");
  const freshID = cold.calls[0].args[1];
  cold.textarea.value = "Later explicit input"; await cold.context.submit(false);
  await cold.accept();
  assert.equal(cold.calls.length, 1, "held automatic draining cannot release the next entry");
  cold.context.executionHeld = false; const drain = cold.context.messageDispatcher.drain(); await settle();
  assert.equal(cold.calls.length, 2); await cold.accept(); await drain;
  assert.notEqual(cold.calls[1].args[1], freshID);
  const blocked = fixture([randomUUID()]); blocked.context.executionHeld = true;
  blocked.context.inputBlockedReason = "An active goal must be handled first.";
  const blockedFiles = blocked.uploads.ids();
  await blocked.context.submit(false);
  assert.equal(blocked.calls.length, 0); assert.equal(blocked.records.size, 0);
  assert.equal(blocked.textarea.value, "Original message"); assert.deepEqual(blocked.uploads.ids(), blockedFiles);
  const refused = fixture(); refused.context.executionHeld = true;
  await refused.context.submit(false); const refusedID = refused.calls[0].args[1];
  await refused.lose(Object.assign(new Error("The mode could not be confirmed."), {
    status: 503, code: "message_not_sent", notSent: true, retryable: false,
  }));
  assert.equal(refused.context.messageDispatcher.entries[0].state, "unsent");
  assert.equal(refused.context.messageDispatcher.entries[0].retryable, false);
  await refused.context.messageDispatcher.drain(); assert.equal(refused.calls.length, 1);
  const explicitRetry = refused.context.retry(refusedID); await settle();
  assert.equal(refused.calls[1].args[1], refusedID); await refused.accept(); await explicitRetry;
  for (const kind of ["text", "attachments", "both"]) {
    const ids = kind === "text" ? [] : [randomUUID()], f = fixture(ids);
    if (kind === "attachments") f.textarea.value = "";
    const original = f.textarea.value;
    await f.context.submit(false);
    assert.equal(f.calls.length, 1); assert.equal(f.textarea.value, ""); assert(!f.uploads.locked);
    for (const queue of [false, false, true]) {
      f.textarea.value = original; f.uploads.values = [...ids]; await f.context.submit(queue);
      assert.equal(f.textarea.value, "");
    }
    await f.context.submit(false);
    assert.equal(f.context.messageDispatcher.entries.length, 4); assert.equal(f.calls.length, 1);
    f.textarea.value = "Newest draft"; f.uploads.values = [randomUUID()]; const latest = [...f.uploads.values];
    for (let index = 0; index < 4; index++) {
      assert.equal(f.calls.length, index + 1); assert.deepEqual(f.calls[index].args.at(-1), ids);
      assert.equal(f.calls[index].action, index === 3 ? "queue" : "send"); await f.accept();
    }
    assert.equal(new Set(f.calls.map(call => call.args[1])).size, 4);
    assert.equal(f.context.messageDispatcher.entries.length, 0);
    assert.equal(f.textarea.value, "Newest draft"); assert.deepEqual(f.uploads.values, latest);
  }
  for (const failure of ["load", "write", "order", "detach"]) {
    const id = randomUUID(), f = fixture([id]);
    if (failure === "load") f.context.loadSendAttempts = () => null;
    if (failure === "write") f.context.storeSendAttempt = (...args) => {
      f.records.set(args[3].id, json(args[3])); return false;
    };
    if (failure === "order") f.saved.setItem = () => {throw new Error("Full");};
    if (failure === "detach") f.uploads.clear = () => {throw new Error("Selection could not be retained");};
    await f.context.submit(false);
    assert.equal(f.calls.length, 0, failure); assert.equal(f.textarea.value, "Original message");
    assert.deepEqual(f.uploads.values, [id]); assert(!f.uploads.locked); assert.equal(f.failures.length, 1);
    if (failure === "detach") {
      const attempt = f.context.messageDispatcher.entries[0]; assert.equal(attempt.state, "failed");
      f.context.retryFailedMessage(attempt.id, "Revised original"); await settle();
      assert.equal(f.calls[0].args[0], "Revised original"); assert.notEqual(f.calls[0].args[1], attempt.id); await f.accept();
    }
    if (["order", "write"].includes(failure)) {
      assert.equal(f.records.size, 0, "failed ordered capture removes only its newly created unsent attempt");
      const restored = fixture([], f.saved, f);
      assert.equal(restored.pendingMessages.size, 0, "capture failure leaves no reload blocker");
    }
  }
  // A failed new capture cannot reclassify a recovered submission as unsent.
  for (const action of ["send", "queue"]) {
    const attachment = randomUUID(), id = randomUUID();
    const legacy = {id, message: "Original message", attachmentIds: [attachment]};
    const retained = {records: new Map(), queues: new Map()};
    retained[action === "send" ? "records" : "queues"].set(id, legacy);
    const recovered = fixture([attachment], storage(), retained);
    recovered.uploads.clear = () => {throw new Error("Selection could not be retained");};
    await recovered.context.submit(action === "queue");
    const entry = recovered.context.messageDispatcher.entries[0];
    assert.equal(recovered.calls.length, 0);
    assert.equal(entry.id, id); assert.equal(entry.retry, true); assert.equal(entry.state, "unknown");
    assert.equal(recovered.failures.at(-1).notSent, false, "recovery failure must retain uncertain guidance");
    recovered.context.discardFailedMessage(id);
    assert.equal(recovered.context.messageDispatcher.entries.length, 1);
    assert(retained[action === "send" ? "records" : "queues"].has(id));
    const restored = fixture([], recovered.saved, retained);
    void restored.context.retry(id); await settle();
    assert.equal(restored.calls.length, 1); assert.equal(restored.calls[0].action, action);
    assert.equal(restored.calls[0].args[1], id); await restored.accept();
  }
  // Failed edit persistence leaves exactly the original failed snapshot, even
  // when the ordered write happened before readback failed.
  for (const action of ["send", "queue"]) {
    for (const fault of ["write", "readback", "record-write", "record-readback", "remove-original", "remove-original-readback"]) {
      const failed = fixture([randomUUID()]);
      failed.uploads.clear = () => {throw new Error("Selection could not be retained");};
      await failed.context.submit(action === "queue");
      const original = failed.context.messageDispatcher.entries[0];
      const get = failed.saved.getItem, set = failed.saved.setItem;
      if (fault === "write") failed.saved.setItem = () => {throw new Error("Full");};
      else if (fault === "readback") {
        let changed = false;
        failed.saved.setItem = function(key, value) {set.call(this, key, value); changed = true;};
        failed.saved.getItem = function(key) {if (changed) {changed = false; throw new Error("Readback failed");} return get.call(this, key);};
      } else {
        const storeName = action === "send" ? "storeSendAttempt" : "storeQueueAttempt";
        const removeName = action === "send" ? "deleteSendAttempt" : "deleteQueueAttempt";
        const store = failed.context[storeName], remove = failed.context[removeName];
        if (fault.startsWith("record-")) failed.context[storeName] = (...args) => {
          if (args[3].id === original.id) return store(...args);
          if (fault === "record-readback") store(...args);
          return false;
        };
        else failed.context[removeName] = (...args) => {
          if (args[3] !== original.id) return remove(...args);
          if (fault === "remove-original-readback") remove(...args);
          return false;
        };
      }
      failed.context.retryFailedMessage(original.id, "Edited but not captured"); await settle();
      assert.equal(failed.calls.length, 0);
      const records = action === "send" ? failed.records : failed.queues;
      assert.deepEqual([...records.keys()], [original.id], "failed edit must not leave an outside-buffer orphan");
      assert.equal(failed.context.messageDispatcher.entries[0].id, original.id);
      failed.saved.getItem = get; failed.saved.setItem = set;
      const restored = fixture([], failed.saved, failed);
      assert.equal(restored.context.messageDispatcher.entries.length, 1);
      assert.equal(restored.context.messageDispatcher.entries[0].id, original.id);
      void restored.context.retry(original.id); await settle();
      assert.equal(restored.calls.length, 1, "original failed entry remains recoverable after reload");
      assert.equal(restored.calls[0].args[0], original.message);
      assert.notEqual(restored.calls[0].args[1], original.id, "definitely unsent retry gets a fresh identity"); await restored.accept();
    }
  }
  const refusals = [
    [400, "message and browser attempt ID are invalid"], [400, "too many attachments"],
    [400, "attachment IDs are invalid or repeated"], [400, "attachments are unavailable"],
    [400, "message and attachment references exceed the prompt limit"], [400, "Too many files for one prompt"],
    [400, "An attachment is unavailable, incomplete or repeated"], [413, "Attachments exceed the prompt size limit"],
    [413, "Message and attachment references exceed the prompt limit"], [409, "An uploaded file is unavailable"],
  ];
  for (const action of ["send", "queue"]) {
    const repeated = fixture([randomUUID()]); await repeated.context.submit(action === "queue");
    repeated.textarea.value = "Waiting after repeated refusal"; await repeated.context.submit(false);
    const firstID = repeated.calls[0].args[1], originalAttachments = repeated.calls[0].args.at(-1);
    await repeated.lose(Object.assign(new Error(refusals[6][1]), {status: 400}));
    const failed = repeated.context.messageDispatcher.entries[0];
    repeated.context.retryFailedMessage(failed.id, failed.message); await settle();
    assert.equal(repeated.calls.length, 2); assert.notEqual(repeated.calls[1].args[1], firstID);
    assert.equal(repeated.calls[1].action, action); assert.deepEqual(repeated.calls[1].args.at(-1), originalAttachments);
    await repeated.lose(Object.assign(new Error(refusals[6][1]), {status: 400}));
    assert.equal(repeated.context.messageDispatcher.entries[0].state, "failed", "repeated refusal remains editable and discardable");
    assert.equal(repeated.context.messageDispatcher.entries.length, 2);
    repeated.textarea.value = "Corrected after repeated refusal"; repeated.uploads.values = [];
    repeated.context.replaceFailedWithDraft(repeated.context.messageDispatcher.entries[0].id); await settle();
    assert.equal(repeated.calls.length, 3); assert.equal(repeated.calls[2].action, action);
    assert.deepEqual(repeated.calls[2].args.at(-1), []);
    await repeated.accept(); assert.equal(repeated.calls[3].args[0], "Waiting after repeated refusal"); await repeated.accept();
    const persisted = fixture([randomUUID()]); await persisted.context.submit(action === "queue");
    await persisted.lose(Object.assign(new Error(refusals[6][1]), {status: 400}));
    const persistedID = persisted.context.messageDispatcher.entries[0].id;
    const failedReload = fixture([], persisted.saved, persisted);
    void failedReload.context.retry(persistedID); await settle();
    assert.equal(failedReload.calls[0].action, action);
    assert.notEqual(failedReload.calls[0].args[1], persistedID, "persisted failed receipt also retries with a new identity");
    await failedReload.lose(Object.assign(new Error(refusals[6][1]), {status: 400}));
    assert.equal(failedReload.context.messageDispatcher.entries[0].state, "failed");
    failedReload.context.discardFailedMessage(failedReload.context.messageDispatcher.entries[0].id);
    assert.equal(failedReload.context.messageDispatcher.entries.length, 0, "persisted failed retry remains explicitly discardable");
    for (const [status, message] of refusals) {
      const refusal = Object.assign(new Error(message), {status});
      const fresh = fixture([randomUUID()]); await fresh.context.submit(action === "queue");
      fresh.textarea.value = "Later waiting message"; await fresh.context.submit(false);
      await fresh.lose(refusal);
      assert.equal(fresh.context.messageDispatcher.entries[0].state, "failed", `${action}/${status}/${message}`);
      assert.equal(fresh.calls.length, 1, "refusal still holds later work");
      const original = fresh.context.messageDispatcher.entries[0];
      fresh.textarea.value = "Corrected text only"; fresh.uploads.values = [];
      fresh.context.replaceFailedWithDraft(original.id); await settle();
      assert.equal(fresh.textarea.value, ""); assert.equal(fresh.calls[1].action, action);
      assert.notEqual(fresh.calls[1].args[1], original.id); assert.deepEqual(fresh.calls[1].args.at(-1), []);
      assert.equal(fresh.calls[1].args[0], "Corrected text only");
      fresh.textarea.value = "Newer draft"; await fresh.accept();
      assert.equal(fresh.calls[2].args[0], "Later waiting message"); await fresh.accept();
      assert.equal(fresh.textarea.value, "Newer draft");
      const uncertain = fixture([randomUUID()]); await uncertain.context.submit(action === "queue");
      await uncertain.lose(); const id = uncertain.context.messageDispatcher.entries[0].id;
      void uncertain.context.retry(id); await settle(); await uncertain.lose(refusal);
      assert.equal(uncertain.context.messageDispatcher.entries[0].state, "unknown", "same refusal cannot classify a previous uncertain identity");
    }
    for (const refusal of [Object.assign(new Error(refusals[6][1]), {status: 403}),
      Object.assign(new Error(refusals[6][1] + "."), {status: 400}), new Error(refusals[6][1]),
      Object.assign(new Error("Other validation"), {status: 400}), Object.assign(new Error("Catalog IO failure after queue acceptance"), {status: 503})]) {
      const unknown = fixture([randomUUID()]); await unknown.context.submit(action === "queue"); await unknown.lose(refusal);
      assert.equal(unknown.context.messageDispatcher.entries[0].state, "unknown");
    }
    const restored = fixture([randomUUID()]); await restored.context.submit(action === "queue");
    const saved = JSON.parse([...restored.saved.values.values()][0]); saved.entries[0].retry = false; saved.entries[0].state = "waiting";
    restored.saved.values.set([...restored.saved.values.keys()][0], JSON.stringify(saved));
    const reloaded = fixture([], restored.saved, restored); void reloaded.context.retry(saved.entries[0].id); await settle();
    await reloaded.lose(Object.assign(new Error(refusals[6][1]), {status: 400}));
    assert.equal(reloaded.context.messageDispatcher.entries[0].state, "unknown", "restored retry:false is not fresh proof");
    const failure = fixture([randomUUID()]); await failure.context.submit(action === "queue");
    const store = failure.saved.setItem;
    failure.saved.setItem = function(key, value) {if (JSON.parse(value).entries[0]?.state === "failed") throw Error("Full"); return store.call(this, key, value);};
    await failure.lose(Object.assign(new Error(refusals[6][1]), {status: 400}));
    assert.equal(failure.context.messageDispatcher.entries[0].state, "unknown", "unverified failed-state write stays uncertain");
    const detach = fixture([randomUUID()]); await detach.context.submit(action === "queue");
    await detach.lose(Object.assign(new Error(refusals[6][1]), {status: 400}));
    const original = detach.context.messageDispatcher.entries[0], correctedFiles = [randomUUID()];
    detach.textarea.value = ""; detach.uploads.values = [...correctedFiles];
    detach.uploads.clear = () => {throw Error("Could not detach");};
    detach.context.replaceFailedWithDraft(original.id); await settle();
    assert.equal(detach.calls.length, 1); assert.equal(detach.textarea.value, ""); assert.deepEqual(detach.uploads.values, correctedFiles);
    assert.equal(detach.context.messageDispatcher.entries[0].id, original.id);
    assert.equal(detach.context.messageDispatcher.entries[0].state, "failed");
    assert.deepEqual([...(action === "send" ? detach.records : detach.queues).keys()], [original.id]);
    assert.equal(detach.pendingMessages.size, 1, "rollback removes replacement's temporary receipt");
  }
  const f = fixture(); await f.context.submit(false); const original = f.calls[0];
  f.textarea.value = "Wait behind uncertainty"; await f.context.submit(true);
  await f.lose(); assert.equal(f.calls.length, 1); assert.equal(f.context.messageDispatcher.entries[0].state, "unknown");
  f.context.messageDispatcher.retry(f.context.messageDispatcher.entries[1].id); await settle();
  assert.equal(f.calls.length, 1, "later entries cannot overtake");
  assert.equal(f.context.messageDispatcher.discard(original.args[1]), false, "uncertain cannot be discarded");
  const reload = fixture([], f.saved, f); await reload.context.messageDispatcher.drain(); assert.equal(reload.calls.length, 0);
  void reload.context.retry(original.args[1]); await settle();
  assert.equal(reload.calls[0].args[1], original.args[1]); assert.equal(reload.calls[0].args[2], true);
  await reload.accept(); assert.equal(reload.calls[1].action, "queue"); await reload.accept();
  const positive = fixture(); await positive.context.submit(false);
  positive.textarea.value = "Next"; await positive.context.submit(false);
  positive.pendingMessages.set(positive.calls[0].args[1], {...positive.pendingMessages.get(positive.calls[0].args[1]), state: "observed"});
  positive.context.messageDispatcher.confirm(positive.calls[0].args[1]);
  await positive.lose(); assert.equal(positive.calls.length, 2, "late failure cannot erase positive proof"); await positive.accept();
  const full = fixture();
  for (let index = 0; index < 21; index++) {full.textarea.value = `Message ${index}`; await full.context.submit(false);}
  assert.equal(full.context.messageDispatcher.entries.length, 20); assert.equal(full.textarea.value, "Message 20");
  assert.match(full.failures.at(-1).message, /20 messages/);
  assert.equal(full.records.size, 20, "capacity rejection must not create an outside-buffer orphan");
  const fullReload = fixture([], full.saved, full);
  assert.equal(fullReload.context.messageDispatcher.entries.length, 20);
  void fullReload.context.retry(fullReload.context.messageDispatcher.entries[0].id); await settle();
  assert.equal(fullReload.calls.length, 1, "full buffer remains recoverable after reload");
  await fullReload.accept(); assert.equal(fullReload.calls.length, 2);
  const confirmed = fixture();
  const persist = confirmed.context.storeSendAttempt;
  confirmed.context.storeSendAttempt = (...args) => args[3].state === "accepted" ? false : persist(...args);
  await confirmed.context.submit(false); await confirmed.accept();
  assert.equal(confirmed.context.messageDispatcher.entries.length, 1);
  assert.equal(confirmed.context.messageDispatcher.isConfirmed(confirmed.calls[0].args[1]), true);
  confirmed.context.storeSendAttempt = persist;
  void confirmed.context.retry(confirmed.calls[0].args[1]); await settle(); await settle();
  assert.equal(confirmed.calls.length, 1, "retry saving cannot resend confirmed content");
  assert.equal(confirmed.context.messageDispatcher.entries.length, 0);
  const switched = fixture(); await switched.context.submit(false); switched.context.currentThreadId = "replacement";
  switched.pendingMessages.clear(); switched.textarea.value = "Replacement draft"; await switched.accept();
  assert.equal(switched.pendingMessages.size, 0); assert.equal(switched.textarea.value, "Replacement draft");
  const nonadmission = () => Object.assign(new Error("The message was not sent."), {status:503, code:"message_not_sent", notSent:true});
  for (const recovered of [false,true]) {
    let typed = fixture(); await typed.context.submit(false);
    const id = typed.calls[0].args[1];
    if (recovered) {await typed.lose(); typed = fixture([],typed.saved,typed); void typed.context.retry(id); await settle();}
    await typed.lose(nonadmission());
    assert.equal(typed.context.messageDispatcher.entries[0].state,"unsent");
    assert.equal(typed.records.get(id).state,"unsent");
    typed.context.discardFailedMessage(id); assert.equal(typed.context.messageDispatcher.entries.length,1);
    const request = typed.context.retry(id); await settle();
    assert.equal(typed.calls.at(-1).args[1],id); assert.equal(typed.calls.at(-1).args[2],true);
    assert.equal(typed.records.get(id).state,"unknown","retry persists uncertainty before HTTP");
    assert.equal(typed.context.messageDispatcher.entries[0].state,"unknown");
    await typed.accept(); await request;
  }
  const typedQueue=fixture(); await typedQueue.context.submit(true); await typedQueue.lose(nonadmission());
  assert.equal(typedQueue.context.messageDispatcher.entries[0].state,"unknown","typed message proof is not queue proof");
  const typedWrite=fixture(); await typedWrite.context.submit(false);
  const typedSet=typedWrite.saved.setItem;
  typedWrite.saved.setItem=function(key,value) {if (JSON.parse(value).entries[0]?.state==="unsent") throw Error("Full"); return typedSet.call(this,key,value);};
  await typedWrite.lose(nonadmission()); assert.equal(typedWrite.context.messageDispatcher.entries[0].state,"unknown");
  const typedPositive=fixture(); await typedPositive.context.submit(false); const typedID=typedPositive.calls[0].args[1];
  typedPositive.pendingMessages.set(typedID,{...typedPositive.pendingMessages.get(typedID),state:"observed"});
  typedPositive.context.messageDispatcher.confirm(typedID); await typedPositive.lose(nonadmission());
  assert.equal(typedPositive.pendingMessages.get(typedID).state,"observed");

  // A legacy receipt is independently recoverable, not a global automatic hold.
  const legacyID=randomUUID(), legacyRecord={id:legacyID,message:"Legacy unknown",state:"unknown",context:""};
  const legacy=fixture([],storage(),{records:new Map([[legacyID,legacyRecord]])});
  legacy.textarea.value="Independent new input"; await legacy.context.submit(false);
  assert.equal(legacy.calls.length,1); assert.equal(legacy.calls[0].args[0],"Independent new input"); await legacy.accept();
  assert.equal(legacy.pendingMessages.get(legacyID).state,"unknown");

  // Explicit copy reconciles the buffered head first, substitutes one fresh ID,
  // and preserves the original, successor order, and current draft/uploader.
  const copied=fixture([randomUUID()]); await copied.context.submit(false); const copiedID=copied.calls[0].args[1];
  copied.textarea.value="Waiting send"; await copied.context.submit(false);
  copied.textarea.value="Waiting queue"; await copied.context.submit(true); await copied.lose();
  const successors=json(copied.context.messageDispatcher.entries.slice(1).map(entry=>entry.id));
  copied.textarea.value="Keep newer draft"; copied.uploads.values=[randomUUID()]; const selected=[...copied.uploads.values];
  let confirmations=0; copied.context.confirm=()=>{confirmations++; return true;};
  const copying=copied.context.resend(copiedID); await settle();
  assert.equal(copied.calls.length,2); assert.equal(copied.calls[1].args[1],copiedID); assert.equal(confirmations,0);
  await copied.context.resend(copiedID); assert.equal(copied.calls.length,2,"parallel recovery cannot allocate a copy");
  await copied.lose(); assert.equal(confirmations,1); assert.equal(copied.calls.length,3);
  const copyID=copied.calls[2].args[1]; assert.notEqual(copyID,copiedID);
  assert.equal(copied.calls[2].args[0],"Original message");
  assert.deepEqual(copied.calls[2].args.at(-1),copied.calls[0].args.at(-1));
  assert.equal(copied.records.get(copiedID).replacementId,copyID);
  assert.equal(copied.pendingMessages.get(copiedID).state,"unknown");
  assert.deepEqual(json(copied.context.messageDispatcher.entries.map(entry=>entry.id)),[copyID,...successors]);
  assert.equal(copied.textarea.value,"Keep newer draft"); assert.deepEqual(copied.uploads.values,selected);
  await copied.lose(); await copying;
  const copiedReload=fixture([],copied.saved,copied);
  assert.equal(copiedReload.calls.length,0); assert.equal(copiedReload.context.messageDispatcher.entries[0].id,copyID);
  assert.equal(copiedReload.records.get(copiedID).replacementId,copyID);
  const resumedCopy=copiedReload.context.retry(copyID); await settle();
  assert.equal(copiedReload.calls[0].args[1],copyID); await copiedReload.accept(); await resumedCopy;
  assert.equal(copiedReload.calls[1].args[0],"Waiting send"); await copiedReload.accept();
  assert.equal(copiedReload.calls[2].action,"queue"); await copiedReload.accept();
  await copiedReload.context.resend(copiedID); assert.equal(copiedReload.calls.length,3,"linked original cannot allocate a third identity");

  for (const outcome of ["accepted","unsent","declined"]) {
    const noCopy=fixture(); await noCopy.context.submit(false); await noCopy.lose();
    const id=noCopy.calls[0].args[1]; let confirmed=0;
    noCopy.context.confirm=()=>{confirmed++; return outcome!=="declined";};
    const work=noCopy.context.resend(id); await settle();
    if (outcome==="accepted") await noCopy.accept(); else await noCopy.lose(outcome==="unsent" ? nonadmission() : undefined);
    await work; assert.equal(noCopy.calls.length,2); assert.equal(noCopy.records.size,1);
    assert.equal(confirmed,outcome==="declined" ? 1 : 0);
  }
  // Every persistence boundary keeps both reachable IDs, and reload recovers the
  // saved replacement without creating another or dispatching automatically.
  for (const fault of ["new-write","new-readback","link-write","link-readback","order-write","order-readback","link-read-unavailable"]) {
    const damaged=fixture(); await damaged.context.submit(false); await damaged.lose(); const id=damaged.calls[0].args[1];
    damaged.textarea.value="Retained draft"; damaged.uploads.values=[randomUUID()]; const files=[...damaged.uploads.values];
    const work=damaged.context.resend(id); await settle();
    const store=damaged.context.storeSendAttempt, get=damaged.saved.getItem, set=damaged.saved.setItem;
    if (fault.startsWith("new-")) damaged.context.storeSendAttempt=(...args)=>{
      if (args[3].id===id) return store(...args);
      if (fault==="new-readback") store(...args); return false;
    };
    if (fault.startsWith("link-")) damaged.context.storeSendAttempt=(...args)=>{
      if (!args[3].replacementId) return store(...args);
      if (fault!=="link-write") store(...args);
      if (fault==="link-read-unavailable") damaged.context.loadSendAttempts=()=>null;
      return false;
    };
    if (fault==="order-write") damaged.saved.setItem=()=>{throw Error("Full");};
    if (fault==="order-readback") {
      let changed=false; damaged.saved.setItem=function(key,value) {set.call(this,key,value); changed=true;};
      damaged.saved.getItem=function(key) {if(changed) {changed=false;throw Error("Readback");} return get.call(this,key);};
    }
    await damaged.lose(); await work;
    assert.equal(damaged.calls.length,2,fault); assert.equal(damaged.textarea.value,"Retained draft"); assert.deepEqual(damaged.uploads.values,files);
    assert(damaged.records.has(id));
    const linked=damaged.records.get(id).replacementId;
    damaged.saved.getItem=get; damaged.saved.setItem=set;
    const restored=fixture([],damaged.saved,damaged);
    assert.equal(restored.calls.length,0);
    if (linked) {
      assert(damaged.records.has(linked),"reachable replacement must never be cleaned as an orphan");
      assert.equal(restored.context.messageDispatcher.entries[0].id,linked);
      const retry=restored.context.retry(linked); await settle(); assert.equal(restored.calls[0].args[1],linked); await restored.accept(); await retry;
    } else assert.deepEqual([...damaged.records.keys()],[id],"provably unreferenced new ID may be cleaned");
  }
  for (const action of ["edit","discard","interrupted-discard"]) {
    const retired=fixture(); await retired.context.submit(false); const originalID=retired.calls[0].args[1];
    await retired.lose(); const copying=retired.context.resend(originalID); await settle(); await retired.lose();
    const replacementID=retired.calls[2].args[1];
    await retired.lose(Object.assign(new Error("An attachment is unavailable, incomplete or repeated"),{status:400})); await copying;
    assert.equal(retired.context.messageDispatcher.entries[0].state,"failed");
    const set=retired.saved.setItem;
    if (action==="edit") {retired.context.retryFailedMessage(replacementID,"Corrected copy"); await settle(); await retired.accept();}
    else {
      if (action==="interrupted-discard") retired.saved.setItem=()=>{throw Error("Full");};
      retired.context.discardFailedMessage(replacementID);
    }
    const proof=retired.records.get(replacementID);
    assert.equal(proof.state,"failed"); assert.equal(proof.dismissed,true); assert.equal(retired.records.get(originalID).replacementId,replacementID);
    retired.saved.setItem=set;
    const after=fixture([],retired.saved,retired);
    assert.equal(after.calls.length,0);
    if (action==="interrupted-discard") {
      assert.equal(after.context.messageDispatcher.entries[0].id,replacementID);
      assert.equal(after.pendingMessages.get(replacementID).dismissed,undefined,"order wins over an interrupted retirement");
      after.context.discardFailedMessage(replacementID);
    } else assert.equal(after.context.messageDispatcher.entries.length,0,"retired failed proof is not recaptured on reload");
    after.textarea.value="Original message"; await after.context.submit(false);
    assert.notEqual(after.calls[0].args[1],replacementID,"retired target cannot match ordinary draft capture");
    assert.notEqual(after.calls[0].args[1],originalID,"linked original cannot match ordinary draft capture"); await after.accept();
  }
  const queuedID=randomUUID(), queued={id:queuedID,message:"Earlier restored queue",action:"queue",attachmentIds:[],state:"waiting"};
  const tailStorage=storage(); tailStorage.setItem("workspace-portal.message-dispatch.example.thread-1",JSON.stringify({schema:1,entries:[queued]}));
  const tail=fixture([],tailStorage,{records:new Map([[legacyID,legacyRecord]]),queues:new Map([[queuedID,queued]])});
  tail.context.dispatchReadReady=false; // Hold the safe head while testing explicit copy recovery behind it.
  const tailCopy=tail.context.resend(legacyID); await settle(); await tail.lose(); await tailCopy;
  const tailID=tail.records.get(legacyID).replacementId;
  assert.deepEqual(json(tail.context.messageDispatcher.entries.map(entry=>entry.id)),[queuedID,tailID]);
  tail.context.dispatchReadReady=true;
  const tailResume=tail.context.retry(queuedID); await settle(); assert.equal(tail.calls[1].action,"queue"); await tail.accept(); await tailResume;
  assert.equal(tail.calls[2].args[1],tailID); await tail.accept();
  // A saved replacement target has exact-ID recovery, but cannot create a
  // second explicit copy and turn a one-hop recovery link into a chain.
  for (const location of ["buffered","legacy"]) {
    const rootID=randomUUID(), targetID=randomUUID();
    const target={id:targetID,message:"One lineage",state:"unknown",context:"",attachmentIds:[]};
    const root={...target,id:rootID,replacementId:targetID};
    let guarded;
    if (location==="buffered") {
      const saved=storage(); saved.setItem("workspace-portal.message-dispatch.example.thread-1",JSON.stringify({schema:1,entries:[{...target,action:"send",retry:true}]}));
      guarded=fixture([],saved,{records:new Map([[rootID,root],[targetID,target]])});
    } else {
      guarded=fixture();
      for (const attempt of [root,target]) {guarded.records.set(attempt.id,attempt); guarded.pendingMessages.set(attempt.id,attempt);}
    }
    for (const reload of [false,true]) {
      if (reload) guarded=fixture([],guarded.saved,guarded);
      guarded.textarea.value="Keep lineage draft"; guarded.uploads.values=[randomUUID()];
      const draftFiles=[...guarded.uploads.values], beforeOrder=json([...guarded.saved.values]), beforeReceipts=json([...guarded.records]);
      const setOrder=guarded.saved.setItem, uuid=guarded.context.crypto.randomUUID; let orderWrites=0, allocations=0;
      guarded.saved.setItem=function(...args) {orderWrites++;return setOrder.apply(this,args);};
      guarded.context.crypto.randomUUID=()=>{allocations++;return uuid();};
      assert.deepEqual([...guarded.pendingMessages.keys()],[rootID,targetID]);
      const renderedRows=receiptButtons(guarded); assert.equal(renderedRows.length,2);
      const rendered=renderedRows[1]; // The fixture inserts root A first and target B second.
      assert.equal(rendered.label,"Outcome unknown.");
      if (location==="legacy" && !reload) assert(renderedRows[0].buttons.includes("Send another copy…"),"root A may recover its saved B");
      assert(rendered.buttons.includes("Retry original"),"target retains its exact-ID recovery control");
      assert(!rendered.buttons.includes("Send another copy…"),"target must not offer a nested-copy control");
      const blocked=guarded.context.resend(targetID); await settle();
      assert.equal(guarded.calls.length,0,"replacement target must not reconcile toward a nested copy"); await blocked;
      const direct=guarded.context.enqueueMessageCopy(targetID,true); await settle();
      assert.equal(guarded.calls.length,0,"direct copy entry must reject a saved replacement target"); await direct;
      assert.deepEqual(json([...guarded.saved.values]),beforeOrder,"forbidden copy cannot write order");
      assert.deepEqual(json([...guarded.records]),beforeReceipts,"both immutable IDs/link survive without C");
      assert.equal(guarded.textarea.value,"Keep lineage draft"); assert.deepEqual(guarded.uploads.values,draftFiles);
      const load=guarded.context.loadSendAttempts; guarded.context.loadSendAttempts=()=>null;
      assert(receiptButtons(guarded).every(item=>!item.buttons.includes("Send another copy…")),"unknown durable references hide copy controls");
      await guarded.context.resend(targetID); await guarded.context.enqueueMessageCopy(targetID,true);
      assert.equal(guarded.calls.length,0,"unavailable reference storage cannot authorize a copy");
      assert.deepEqual(json([...guarded.saved.values]),beforeOrder); assert.deepEqual(json([...guarded.records]),beforeReceipts);
      assert.equal(guarded.textarea.value,"Keep lineage draft"); assert.deepEqual(guarded.uploads.values,draftFiles);
      guarded.context.loadSendAttempts=load;
      assert.equal(orderWrites,0,"forbidden or uncertain copy eligibility cannot write order");
      assert.equal(allocations,0,"forbidden copy cannot allocate C");
      guarded.saved.setItem=setOrder; guarded.context.crypto.randomUUID=uuid;
      const recoverExisting=guarded.context.resend(rootID); await settle();
      assert.equal(guarded.calls.length,1); assert.equal(guarded.calls[0].args[1],targetID,"root's saved A-to-B recovery reuses B");
      await guarded.lose(); await recoverExisting;
      assert.equal(guarded.records.size,2); guarded.calls.length=0;
      const retry=guarded.context.retry(targetID); await settle();
      assert.equal(guarded.calls.at(-1).args[1],targetID,"target still supports exact-ID retry");
      await guarded.lose(); await retry;
      guarded.calls.length=0;
    }
    guarded.pendingMessages.set(targetID,{...guarded.pendingMessages.get(targetID),state:"observed"});
    guarded.context.messageDispatcher.confirm(targetID);
    assert.equal(guarded.context.messageDispatcher.entries.length,0,"positive target proof releases its existing hold");
    assert.equal(guarded.records.get(rootID).replacementId,targetID); assert.equal(guarded.records.get(rootID).state,"unknown");
  }
  // A saved incoming link that appears during reconciliation invalidates a
  // stale copy action before confirmation or replacement persistence.
  const staleCopy=fixture(); await staleCopy.context.submit(false); await staleCopy.lose();
  const staleID=staleCopy.calls[0].args[1]; let staleConfirmations=0;
  assert(receiptButtons(staleCopy)[0].buttons.includes("Send another copy…"),"an eligible original still offers its first copy");
  staleCopy.context.confirm=()=>{staleConfirmations++;return true;};
  const staleRecovery=staleCopy.context.resend(staleID); await settle();
  const incoming={id:randomUUID(),message:"Original message",state:"unknown",replacementId:staleID};
  staleCopy.records.set(incoming.id,incoming); await staleCopy.lose(); await staleRecovery;
  assert.equal(staleConfirmations,0); assert.equal(staleCopy.calls.length,2);
  assert.equal(staleCopy.records.size,2); assert.equal(staleCopy.records.get(staleID).replacementId,undefined);
  // Pi consumes idle per request; existing Codex tests above retain their
  // explicit cold-input and transient retry behavior.
  {
    const f = fixture([randomUUID()], storage(), {pi: true});
    f.context.piReady = false;
    const files = f.uploads.ids();
    await f.context.submit(false);
    const first = f.context.messageDispatcher.entries[0];
    assert.equal(f.calls.length, 0); assert.equal(first.message, "Original message");
    assert.deepEqual(json(first.attachmentIds), files); assert.equal(f.textarea.value, "");
    f.textarea.value = "Second Pi message"; await f.context.submit(false);
    f.context.piReady = true; const sending = f.context.messageDispatcher.drain(); await settle();
    assert.equal(f.calls.length, 1); assert.equal(f.calls[0].args[1], first.id);
    await f.accept(); await sending; assert.equal(f.calls.length, 1, "settlement alone cannot reuse consumed idle");
    f.context.piReady = true; const next = f.context.messageDispatcher.drain(); await settle();
    assert.equal(f.calls.length, 2); assert.equal(f.calls[1].args[0], "Second Pi message");
    await f.accept(); await next;
  }
  for (const proof of [false, undefined]) {
    const f = fixture([randomUUID()], storage(), {pi: true});
    await f.context.submit(false); const id = f.calls[0].args[1];
    await f.lose(Object.assign(new Error("Pi refusal"), {status: 503, code: "message_not_sent", notSent: true, retryable: proof}));
    const entry = f.context.messageDispatcher.entries[0];
    assert.equal(entry.state, proof === false ? "failed" : "unsent");
    assert.equal(helpers.isTerminalPiSend(entry), proof === false);
    assert.equal(f.calls.length, 1);
    if (proof === false) {
      assert.equal(receiptButtons(f)[0].label, "Pi did not accept this message");
      assert(receiptButtons(f)[0].buttons.includes("Send as new message"));
      assert(!receiptButtons(f)[0].buttons.includes("Retry original"));
      f.context.piReady = true;
      f.textarea.value = entry.message; f.uploads.values = [...entry.attachmentIds];
      await f.context.submit(false);
      assert.equal(f.calls.length, 1, "ordinary matching draft never silently replaces a terminal ID");
      assert.equal(f.context.messageDispatcher.entries[0].id, id);
      f.context.retryFailedMessage(id, entry.message); await settle();
      assert.equal(f.calls.length, 2); assert.notEqual(f.calls[1].args[1], id);
      assert.deepEqual(f.calls[1].args.at(-1), json(entry.attachmentIds));
      await f.accept();
    }
  }
  {
    const id = randomUUID(), attempt = {id, message: "historical", attachmentIds: [randomUUID()], state: "unsent", retryable: false};
    const f = fixture([], storage(), {pi: true, records: new Map([[id, attempt]])});
    assert.equal(helpers.isTerminalPiSend(attempt), false, "historical false has no wire proof");
    const retry = f.context.retry(id); await settle();
    assert.equal(f.calls.length, 1); assert.equal(f.calls[0].args[1], id);
    await f.lose(Object.assign(new Error("Pi refusal"), {status: 503, code: "message_not_sent", notSent: true, retryable: false}));
    await retry;
    assert.equal(f.context.messageDispatcher.entries[0].piTerminalNotSubmitted, true);
    assert.equal(helpers.isTerminalPiSend(f.context.messageDispatcher.entries[0]), true);
  }
  // An original receipt read neither consumes readiness nor clears a concurrent send.
  {
    const f = fixture([], storage(), {pi: true, delivery: true});
    await f.context.submit(false);
    const id = f.calls[0].args[1];
    await f.lose(Object.assign(new Error("Pending"), {status:409, code:"message_pending", pending:true,
      clientUserMessageId:id, delivery:"steer"}));
    f.context.piReady = true;
    const checking = f.context.reconcilePiPendingMessages(id); await settle();
    assert.equal(receiptButtons(f)[0].label, "Checking delivery…");
    assert.equal(f.context.messageDispatchInFlight, false);
    assert.equal(f.context.piReady, true);
    await f.context.reconcilePiPendingMessages(id);
    assert.equal(f.calls.length, 2, "one enquiry per original ID");
    assert.deepEqual(f.calls[1].args, ["Original message", id, true, [], "steer"]);
    f.textarea.value = "Next input";
    await f.context.submit(false); await settle();
    assert.equal(f.calls.length, 3, "a receipt enquiry permits a new input");
    assert.notEqual(f.calls[2].args[1], id);
    assert.equal(f.calls[2].args[2], false);
    await f.accept(); await checking;
    assert.equal(f.context.messageDispatchInFlight, true, "lookup completion cannot clear the new send");
    await f.accept();
    assert.equal(f.records.get(id).state, "accepted");
    assert.equal(f.context.messageDispatchInFlight, false);
  }
  {
    const f = fixture([], storage(), {pi:true, delivery:true});
    await f.context.submit(false); const id=f.calls[0].args[1];
    await f.lose(Object.assign(new Error("Pending"), {status:409,code:"message_pending",pending:true,clientUserMessageId:id,delivery:"steer"}));
    f.context.piReady=true;
    const checking=f.context.reconcilePiPendingMessages(id); await settle();
    f.context.currentThreadId="replacement";
    await f.accept(); await checking;
    assert.equal(f.records.get(id).state,"pending","stale enquiry cannot settle a replacement page");
    assert.equal(f.context.receiptEnquiries.size,0);
  }
  // Exercise the actual apply callback: commit idle only after rendering succeeds.
  {
    const file = randomUUID(), f = fixture([file], storage(), {pi: true, delivery: true});
    const pending = (id, delivery) => Object.assign(new Error("Queued"), {status: 409, code: "message_pending", pending: true, clientUserMessageId: id, delivery});
    await f.context.submit(true);
    const followUp = f.calls[0].args[1];
    assert.equal(f.calls[0].action, "send"); assert.equal(f.calls[0].args[4], "followUp");
    await f.lose(pending(followUp, "followUp"));
    assert.equal(f.records.get(followUp).state, "pending"); assert.equal(f.context.messageDispatcher.entries.length, 0);
    assert.equal(receiptButtons(f)[0].label, "Follow-up queued");
    assert(!receiptButtons(f)[0].buttons.includes("Check delivery"));
    assert(!receiptButtons(f)[0].buttons.includes("Drop message"));
    f.context.piReady = true;
    assert(!receiptButtons(f)[0].buttons.includes("Check delivery"), "known Pending is status-only even when checking is available");
    f.context.piReady = true; f.textarea.value = "Original message"; f.uploads.values = [file];
    await f.context.submit(false);
    const steer = f.calls[1].args[1];
    assert.notEqual(steer, followUp); assert.equal(f.calls[1].args[4], "steer");
    assert.deepEqual(f.calls[1].args[3], [file]);
    await f.lose(pending(steer, "steer"));
    assert(receiptButtons(f).every(row=>!row.buttons.includes("Check delivery")));
    f.context.piReady = true;
    const checking = f.context.reconcilePiPendingMessages(steer); await settle();
    assert.equal(f.calls[2].args[1], steer); assert.equal(f.calls[2].args[2], true); assert.equal(f.calls[2].args[4], "steer");
    await f.accept(); await checking;
    assert.equal(f.records.get(steer).state, "accepted"); assert.equal(f.records.get(followUp).state, "pending");
    assert.equal(receiptButtons(f).filter(row=>row.label==="Sent").length,1);
    f.context.piReady = true;
    const lost = f.context.reconcilePiPendingMessages(followUp); await settle(); await f.lose(); await lost;
    assert.equal(f.records.get(followUp).state, "unknown"); assert.equal(f.records.get(followUp).piNativePending, true);
    assert.equal(receiptButtons(f)[0].label,"Outcome unknown.");
    assert.equal(receiptButtons(f)[0].disabled["Check delivery"],false,"lookup preserves existing readiness; Unknown still fences new dispatch");
    f.context.piReady = true; f.textarea.value = "Later"; await f.context.submit(false);
    assert.equal(f.calls.length, 4, "uncertain admitted input fences later local dispatch");
    f.context.dropPendingMessage(followUp); assert.equal(f.records.get(followUp).dropped, undefined);
    assert.equal(receiptButtons(f)[0].disabled["Check delivery"],false);
    f.context.liveSettingsSaving=true;
    assert.equal(receiptButtons(f)[0].disabled["Check delivery"],true);
    f.context.liveSettingsSaving=false;
    const original=f.records.get(followUp), manual=f.context.retry(followUp); await settle();
    assert.deepEqual(f.calls[4].args,[original.message,followUp,true,original.attachmentIds,"followUp"]);
    assert.equal(receiptButtons(f)[0].disabled["Check delivery"],true,"an in-flight original-ID check stays disabled");
    await f.accept(); await manual;
    assert.equal(f.calls.length,5,"checking uncertain native input does not capture or enqueue another request");
    assert.equal(f.records.get(followUp).state,"accepted");
    assert.equal(receiptButtons(f).filter(row=>row.label==="Sent").length,2);
    assert(receiptButtons(f).every(row=>!row.buttons.includes("Check delivery")));
    await helpers.markTranscriptMessagesObserved(f.pendingMessages,[{clientUserMessageId:followUp,
      clientUserMessageDigest:createHash("sha256").update(original.message).digest("hex"), text:original.message,
      displayText:original.message,attachments:original.attachmentIds.map(id=>({id}))}]);
    assert.equal(f.pendingMessages.get(followUp).state,"observed");
    assert.equal(receiptButtons(f).filter(row=>row.label==="Sent").length,1,"observed acceptance removes only its own receipt");
  }
  for (const mismatch of ["id", "mode", "flag"]) {
    const f = fixture([], storage(), {pi: true, delivery: true}); await f.context.submit(false);
    const id = f.calls[0].args[1];
    await f.lose(Object.assign(new Error("Unproved Pending"), {status: 409, code: "message_pending", pending: mismatch !== "flag",
      clientUserMessageId: mismatch === "id" ? randomUUID() : id, delivery: mismatch === "mode" ? "followUp" : "steer"}));
    assert.equal(f.context.messageDispatcher.entries[0].state, "unknown");
    assert.equal(f.records.get(id).piNativePending, undefined);
  }
  {
    const c = vm.createContext({piBackend: true, piPageScope: {current: () => true}, messageDispatchInFlight: false, isPiDelivery: helpers.isPiDelivery,
      threadActive: false, renderThread() {}, piWorkflows: null, refreshPending() {}, refreshQueue() {},
      scheduleHistoryRepair() {}, updateMessageActions() {}, messageDispatcher: {drain() {}}});
    vm.runInContext(section("  let piDispatchRevision =", "  let bindMessageComposer =") +
      "globalThis.ready=piSendReady; globalThis.consume=consumePiIdle; globalThis.read=()=>piDispatchRevision;" +
      "globalThis.callbacks=({" + section("    apply: (thread, {isCurrent}) => {", "    onPendingInvalidation:") + "});" +
      "globalThis.apply=r=>callbacks.apply({status:'idle',piDispatchRevision:r},{isCurrent:()=>true});", c);
    const old = c.read(); c.consume(); const during = c.read();
    c.consume(); c.apply(old); assert.equal(c.ready(), false);
    c.apply(during); assert.equal(c.ready(), false);
    c.apply(c.read()); assert.equal(c.ready(), true);
    c.renderThread = () => {throw new Error("Render failed");};
    assert.throws(() => c.apply(c.read()), /Render failed/); assert.equal(c.ready(), false);
  }
  completed = true;
  for (const state of ["unknown", "waiting", "unsent", "failed"]) {
    const id = randomUUID(), later = randomUUID(), saved = storage();
    const original = {id, message: "Drop this", action: "send", state, attachmentIds: [randomUUID()]};
    const successor = {id: later, message: "Keep this", action: "send", state: "waiting", attachmentIds: []};
    saved.setItem("workspace-portal.message-dispatch.example.thread-1", JSON.stringify({schema: 1, entries: [original, successor]}));
    const f = fixture([randomUUID()], saved, {records: new Map([[id, original], [later, successor]])});
    f.context.executionHeld = true;
    const draft = f.textarea.value, files = f.uploads.ids(); let confirmations = 0;
    f.context.confirm = () => {confirmations++; return false;};
    if (state === "unknown") {f.context.dropPendingMessage(id); assert.equal(f.context.messageDispatcher.entries.length, 2);}
    f.context.confirm = () => {confirmations++; return true;};
    assert(receiptButtons(f)[0].buttons.includes("Drop message"));
    f.context.dropPendingMessage(id); await settle();
    assert.equal(f.calls.length, 0); assert(f.records.get(id).dropped);
    assert.equal(f.context.messageDispatcher.entries[0].id, later);
    assert.equal(f.textarea.value, draft); assert.deepEqual(f.uploads.ids(), files);
    assert.equal(confirmations, state === "unknown" ? 2 : 0);
    assert.equal(helpers.matchingSendAttempt([...f.records.values()], original.message), undefined);
    const reload = fixture([], saved, f);
    assert.equal(reload.calls.length, 0); assert.equal(reload.context.messageDispatcher.entries.length, 1);
    await reload.context.retry(id); assert.equal(reload.calls.length, 0, "dropped receipt cannot retry");
    const next = reload.context.retry(later); await settle(); await reload.accept(); await next;
    assert.equal(reload.calls[0].args[1], later);
  }
  {
    const id = randomUUID(), saved = storage();
    const attempt = {id, message: "Only pending message", state: "failed", action: "send", attachmentIds: []};
    saved.setItem("workspace-portal.message-dispatch.example.thread-1", JSON.stringify({schema: 1, entries: [attempt]}));
    const f = fixture([], saved, {records: new Map([[id, attempt]])});
    f.context.dropPendingMessage(id);
    f.textarea.value = "New explicit input"; await f.context.submit(false); await settle();
    assert.equal(f.calls.length, 1, "dropping the last restored entry permits fresh explicit input");
    await f.accept();
  }
  for (const fault of ["receipt-write", "receipt-readback", "order-write", "order-readback"]) {
    const f = fixture(); await f.context.submit(false); const id = f.calls[0].args[1]; await f.lose();
    f.textarea.value = "Following input"; await f.context.submit(false);
    const writeReceipt = f.context.storeSendAttempt, writeOrder = f.saved.setItem;
    if (fault.startsWith("receipt")) f.context.storeSendAttempt = (...args) => {if (fault.endsWith("readback")) writeReceipt(...args); return false;};
    else f.saved.setItem = function(...args) {if (fault.endsWith("readback")) writeOrder.apply(this, args); throw Error("Full");};
    f.context.dropPendingMessage(id); await settle();
    await f.context.messageDispatcher.drain(); assert.equal(f.calls.length, 1, fault + " must hold dispatch");
    f.saved.setItem = writeOrder;
    const reload = fixture([], f.saved, f);
    assert.equal(reload.calls.length, 0);
    assert.equal(reload.context.messageDispatcher.entries.some(entry => entry.id === id), fault === "receipt-write");
  }
  // A dropped replacement remains linked for reconciliation, but never returns
  // to the order on reload or blocks later independent messages.
  const originalID = randomUUID(), replacementID = randomUUID();
  const linked = {id: originalID, message: "copy", state: "unknown", replacementId: replacementID, attachmentIds: []};
  const replacement = {id: replacementID, message: "copy", state: "unknown", attachmentIds: []};
  const droppedCopy = fixture([], storage(), {records: new Map([[originalID, linked], [replacementID, replacement]])});
  droppedCopy.context.dropPendingMessage(replacementID);
  const copyReload = fixture([], droppedCopy.saved, droppedCopy);
  assert.equal(copyReload.context.messageDispatcher.entries.length, 0);
  copyReload.textarea.value = "Fresh task"; await copyReload.context.submit(false); await settle();
  assert.equal(copyReload.calls.length, 1); await copyReload.accept();
  {
  const root = "root", messageID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
  const message = {id: messageID, from: {slug: "sender", thread: root}, to: {slug: "recipient", thread: "peer"}, text: "CLI-only message", status: "held", createdAt: "2026-10-28T12:00:00Z"};
  const native = [{key: "native", entry: {kind: "commandExecution", timestamp: "2026-10-28T11:59:00Z"}}];
  const project = helpers.mergeSessionCommunicationRows;
  assert.equal(project(native, [message], root, root).length, 2);
  assert.equal(project(native, [message], "member", root), native, "member view excludes lead traffic");
  assert.equal(native.length, 1, "projection cannot mutate native paging history");
  const incoming = {key: "incoming", entry: {kind: "userMessage", clientUserMessageId: messageID, text: "envelope"}};
  const displayed = project([...native, incoming], [message], root, root);
  assert.equal(displayed.length, 2); assert.match(displayed[1].entry.sessionDelivery, /Delivery: held/);
  const delivered = project(native, [{...message, status: "delivered"}], root, root);
  assert.equal(delivered[1].key, displayed[1].key); assert.match(delivered[1].entry.sessionDelivery, /Delivery: delivered/);
  const pi = {backend: "pi", session: {slug: "pi-peer"}, leadIncarnation: "incarnation", nativeId: "pi-native"};
  for (const [from, to, selected] of [[message.from, pi, root], [pi, message.from, root], [pi, {...pi, session: {slug: "pi-other"}, nativeId: "pi-other-native"}, "pi-native"]]) {
    const mixed = {...message, from, to};
    const envelope = {key: "envelope", entry: {kind: "userMessage", text: `Cross-session message from sender to recipient (message ${messageID}).\n\nQuoted data`}};
    const rows = project([...native, envelope], [mixed], selected, selected);
    assert.equal(rows.length, 2, "mixed native envelope appears only once");
    assert.equal(rows[1].entry.communication.from, from.slug || from.session.slug);
    assert.equal(rows[1].entry.communication.to, to.slug || to.session.slug);
    assert.equal(rows[1].entry.text, mixed.text);
    assert.equal(project(native, [mixed], "pi-member", selected), native, "Pi member cannot see lead exchange");
  }

  }
  for (const backend of ["codex", "pi"]) {
    for (const selectedMember of [backend === "pi" ? "lead" : "", "reviewer0"]) {
      const calls = [], events = [];
      const element = () => ({hidden: false, value: "peer", children: [], handlers: {}, textContent: "",
        append(...nodes) {this.children.push(...nodes);},
        replaceChildren(...nodes) {this.children = nodes; this.firstChild = nodes[0];},
        addEventListener(name, callback) {this.handlers[name] = callback;}});
      const warning = element(), selector = element(), grants = element(), counters = element(), allow = element();
      const controls = {"[data-communication-warning]": warning, select: selector, "[data-communication-grants]": grants,
        "[data-communication-counts]": counters, "[data-communication-allow]": allow};
      const panel = backend === "codex" ? {hidden: true, querySelector: name => controls[name]} : null;
      const scope = backend === "pi" ? "pi-workflow-v1:exact" : "exact-codex-observation";
      const document = {hidden: false, body: {dataset: {session: "current", sessionBackend: backend, selectedMember,
          conversationScope: scope, sessionObservation: scope}},
        getElementById: id => id === "communication" ? panel : id === "session-communication-warning" ? element() : {classList: {contains: () => id === "codex"}},
        querySelector: () => ({dataset: {transcriptFilter: "sessions"}}), createElement: element,
        addEventListener() {}, dispatchEvent: event => events.push(event)};
      const generation = "a".repeat(64);
      const current = backend === "pi" ? {backend: "pi", session: {slug: "current"}, nativeId: "native"} : {slug: "current", thread: "root"};
      const response = {candidates: ["peer"], generation: backend === "pi" ? generation : "",
        grants: [{a: current, b: {slug: "peer"}, approvedBy: "current", approval: "user reference"}],
        messages: [{from: current, to: {slug: "peer"}, status: "held"}, {from: {slug: "peer"}, to: current, status: "delivered"}]};
      const context = {document, AbortController, addEventListener() {}, clearInterval() {}, MutationObserver: class {observe() {}}, setInterval() {},
        CustomEvent: class {constructor(type, value) {this.type = type; this.detail = value.detail;}},
        fetch: async (url, options) => {calls.push({url, ...json(options)}); return {ok: true, json: async () => response};}};
      vm.runInNewContext(fs.readFileSync("static/communication.js", "utf8"), context);
      await settle();
      if (selectedMember === "reviewer0") {assert.equal(calls.length, 0, "member transcript cannot read lead exchanges"); continue;}
      assert.equal(calls[0].headers["X-Workspace-Conversation-Scope"], scope);
      assert.equal(events[0].detail.messages.length, 2);
      if (backend === "pi") {assert.equal(calls.length, 1, "Pi display does not construct settings mutations"); continue;}
      assert.equal(counters.textContent, "1 sent · 1 received");
      allow.handlers.click(); await settle();
      const post = calls.find(call => call.method === "POST");
      assert.equal(post.headers["X-Workspace-Conversation-Scope"], scope);
      assert.equal(post.headers["X-Workspace-Pi-Generation"], backend === "pi" ? generation : undefined);
      assert.deepEqual(JSON.parse(post.body), {action: "allow", peer: "peer"});
    }
  }
  console.log("Composer: immutable ordered sends/queues, recovery barriers, reload, storage failures, edit and bounds passed.");
})().catch(error => {console.error(error); process.exitCode = 1;});

// Consume server-rendered markup and the real communication HTTP projection in
// the existing finite VM fixture; Pi deliberately has no settings form.
async function communicationPresentation(input) {
  const sharedStart = input.asset.indexOf("export function transcriptEntryPresentation("),
    sharedEnd = input.asset.indexOf("// This is display text", sharedStart);
  assert(sharedStart >= 0 && sharedEnd > sharedStart);
  const shared = await import("data:text/javascript," + encodeURIComponent(input.asset.slice(sharedStart, sharedEnd)));
  const presentationContext = vm.createContext({sharedTranscriptPresentation: shared.transcriptEntryPresentation,
    sharedTranscriptVisible: shared.transcriptEntryVisible});
  vm.runInContext(section("  const transcriptEntryPresentation =", "  const transcriptErrorPresentation =") +
    "globalThis.present = transcriptEntryPresentation; globalThis.visible = transcriptEntryVisible;", presentationContext);
  const {present, visible} = presentationContext;
  const element = () => ({hidden: false, value: "", children: [], childNodes: [], handlers: {}, dataset: {}, textContent: "",
    classList: {toggle() {}}, append(...nodes) {this.children.push(...nodes);}, prepend(...nodes) {this.children.unshift(...nodes);},
    replaceChildren(...nodes) {this.children = nodes; this.firstChild = nodes[0];},
    querySelectorAll() {return [];}, addEventListener(name, callback) {this.handlers[name] = callback;}});
  const renderContext = vm.createContext({document: {createElement: element}, Node: {TEXT_NODE: 3},
    transcriptEntryPresentation: present, pagingHelpersAvailable: false, legacyTranscriptEntryKey: helpers.legacyTranscriptEntryKey,
    transcriptDisplayKey: helpers.legacyTranscriptEntryKey,
    conversationAssets: {createTranscriptActivity: () => null}, renderMessageMarkdown: helpers.renderMessageMarkdown,
    optionalQuestions: null});
  vm.runInContext(section("  const appendMessage =", "  const renderTranscriptEntries =") + "globalThis.render = appendMessage;", renderContext);
  const fixture = (html, respond) => {
    const body = /<body\b([^>]*)>/.exec(html); assert(body);
    const dataset = Object.fromEntries([...body[1].matchAll(/data-([a-z-]+)="([^"]*)"/g)].map(([, key, value]) =>
      [key.replace(/-([a-z])/g, (_, letter) => letter.toUpperCase()), value]));
    const ids = new Set([...html.matchAll(/\bid="([^"]+)"/g)].map(match => match[1]));
    const controls = {"[data-communication-warning]": element(), select: element(), "[data-communication-grants]": element(),
      "[data-communication-counts]": element(), "[data-communication-allow]": element()};
    const nodes = new Map([...ids].map(id => [id, element()]));
    if (nodes.has("communication")) {nodes.get("communication").hidden = true; nodes.get("communication").querySelector = name => controls[name];}
    for (const id of ["codex", "settings"]) if (nodes.has(id)) nodes.get(id).classList.contains = () => id === "codex";
    const calls = [], events = [], handlers = {}, timers = [], pages = {};
    const observerState = {connected:false, observes:0};
    const document = {body: {dataset}, hidden: false, createElement: element, getElementById: id => nodes.get(id) || null,
      querySelector: () => ({dataset: {transcriptFilter: "messages"}}),
      addEventListener(name, callback) {handlers[name] = callback;}, dispatchEvent(event) {events.push(event);}};
    const context = {document, AbortController, addEventListener(name, callback) {pages[name] = callback;},
      setInterval(callback) {timers.push(callback); return 1;}, clearInterval() {timers.length = 0;},
      MutationObserver: class {observe() {observerState.connected=true;observerState.observes++;} disconnect() {observerState.connected=false;}},
      CustomEvent: class {constructor(type, value) {this.type = type; this.detail = value.detail;}},
      fetch: async (url, options) => {calls.push({url, options}); return respond();}};
    vm.runInNewContext(fs.readFileSync("static/communication.js", "utf8"), context);
    return {document, nodes, controls, calls, events, handlers, timers, pages, observerState};
  };
  for (const page of input.pages) {
    let payload = page.response;
    const respond = async () => ({ok: true, json: async () => payload});
    const f = fixture(page.html, respond);
    assert.equal(f.nodes.has("communication"), page.backend === "codex");
    assert.equal(f.calls.length, 0, "Messages does not poll hidden session rows");
    f.handlers["session-transcript-filter-change"]?.({detail: "sessions"}); await settle();
    assert.equal(f.calls.length, 1, "actual Pi/Codex markup starts the same display reader");
    assert.equal(f.calls[0].url, `/api/sessions/${page.slug}/communication`);
    assert.equal(f.calls[0].options.cache, "no-store");
    assert.equal(f.calls[0].options.headers["X-Workspace-Conversation-Scope"], page.scope);
    assert.equal(f.calls[0].options.method, undefined);
    assert.equal(f.events.length, 1);
    const native = page.entries.map(entry => ({key: entry.itemId, entry}));
    const original = JSON.stringify(native);
    const project = messages => helpers.mergeSessionCommunicationRows(native, messages, page.thread, page.thread);
    const rows = project(f.events[0].detail.messages);
    const messages = rows.filter(row => row.entry.kind === "sessionMessage");
    assert.equal(messages.length, payload.messages.length);
    assert.equal(rows.filter(row => row.entry.kind === "userMessage" && row.entry.communication?.kind === "session").length, 0);
    assert.equal(JSON.stringify(native), original, "display projection leaves native history/IDs/digests intact");
    const times = rows.filter(row => row.entry.timestamp).map(row => row.entry.timestamp);
    assert.deepEqual(times, [...times].sort(), "creation timestamps interleave with native history in order");
    for (const {entry} of messages) {
      const saved = payload.messages.find(message => `session-message:${message.id}` === entry.itemId);
      const from = saved.from.slug || saved.from.session.slug, to = saved.to.slug || saved.to.session.slug;
      assert.equal(present(entry).label, `Session · ${from} → ${to}`);
      const rendered = renderContext.render(entry, 0, [entry], new Map(), true);
      assert.equal(rendered.children[0].className, "message-session-label");
      assert.equal(rendered.children[0].textContent, `Session · ${from} → ${to}`);
      for (const filter of ["sessions", "all"]) assert.equal(visible(entry, filter), true);
      for (const filter of ["messages", "team", "activity"]) assert.equal(visible(entry, filter), false);
      assert(!entry.html.includes("<script>")); assert(entry.html.includes("<strong>"));
    }
    for (const entry of page.entries) {
      if (entry.communication?.kind === "session") {
        assert.equal(visible(entry, "sessions"), true, "cold/failed ledger still classifies native envelope");
        assert.equal(visible(entry, "messages"), false);
      }
      if (entry.communication?.kind === "team") {
        assert.equal(visible(entry, "team"), true); assert.equal(visible(entry, "sessions"), false);
      }
      if (entry.kind === "error") for (const filter of ["all", "messages", "team", "sessions", "activity"]) assert.equal(visible(entry, filter), true);
    }
    const firstKeys = rows.map(row => row.key);
    payload = {...payload, messages: payload.messages.map(message => ({...message, status: "delivered"}))};
    f.timers[0](); await settle();
    assert.equal(f.calls.length, 2); assert.equal(f.events.length, 2);
    if (page.backend === "pi") assert.equal(f.calls[1].options.headers["X-Workspace-Pi-Generation"], payload.generation);
    const updated = project(f.events[1].detail.messages);
    assert.deepEqual(updated.map(row => row.key), firstKeys);
    assert(updated.filter(row => row.entry.kind === "sessionMessage").every(row => row.entry.sessionDelivery === "Delivery: delivered"));
    const reloaded = fixture(page.html, respond);
    reloaded.handlers["session-transcript-filter-change"]({detail: "sessions"}); await settle();
    assert.deepEqual(project(reloaded.events[0].detail.messages).map(row => row.key), firstKeys);
    const older = {key: "older", entry: {kind: "agentMessage", itemId: "older", timestamp: "2026-10-01T00:00:00Z"}};
    const paged = helpers.mergeSessionCommunicationRows([older, ...native], payload.messages, page.thread, page.thread);
    assert.equal(paged.length, rows.length + 1); assert.equal(paged[0].key, "older");
    const member = fixture(page.memberHTML, respond);
    member.handlers["session-transcript-filter-change"]?.({detail: "sessions"}); member.timers[0]?.(); await settle();
    assert.equal(member.calls.length, 0); assert.equal(member.events.length, 0);
    assert.equal(helpers.mergeSessionCommunicationRows(native, payload.messages, "member", page.thread), native);
    for (const change of ["scope", "member", "pagehide", "hidden"]) {
      let finish;
      const late = fixture(page.html, () => new Promise(resolve => {finish = resolve;}));
      late.handlers["session-transcript-filter-change"]({detail: "sessions"});
      if (change === "scope") late.document.body.dataset[page.backend === "pi" ? "conversationScope" : "sessionObservation"] = "replacement";
      if (change === "member") late.document.body.dataset.selectedMember = "reviewer0";
      if (change === "pagehide") late.pages.pagehide();
      if (change === "hidden") {late.document.hidden = true; late.handlers.visibilitychange();}
      finish(await respond()); await settle(); assert.equal(late.events.length, 0, change + " refuses late display publication");
    }
    // BFCache restores these same closures, not a newly executed script.
    let finishOld, reads = 0;
    const restored = fixture(page.html, () => ++reads === 2 ? new Promise(resolve => {finishOld = resolve;}) : respond());
    restored.handlers["session-transcript-filter-change"]({detail:"sessions"}); await settle();
    assert.equal(restored.events.length,1);
    restored.timers[0](); await settle();
    assert.equal(restored.calls.length,2);
    const oldRead=restored.calls[1];
    restored.pages.pagehide({persisted:true});
    assert(oldRead.options.signal.aborted);
    assert.equal(restored.timers.length,0);
    assert.equal(restored.observerState.connected,false);
    restored.pages.pageshow({persisted:true}); await settle();
    assert.equal(restored.calls.length,3,"restore starts one fresh read without waiting for the old response");
    assert.equal(restored.events.length,2);
    assert.equal(restored.timers.length,1,"restore reconnects one polling timer");
    assert.equal(restored.observerState.observes,page.backend === "codex" ? 2 : 0);
    finishOld(await respond()); await settle();
    assert.equal(restored.events.length,2,"late pre-hide response cannot publish after restoration");
    restored.pages.pageshow({persisted:true}); await settle();
    assert.equal(restored.calls.length,3,"repeated pageshow does not duplicate a reader");
    assert.equal(restored.timers.length,1);
    assert.equal(restored.observerState.observes,page.backend === "codex" ? 2 : 0);
    restored.timers[0](); await settle();
    assert.equal(restored.calls.length,4,"the resumed poll refreshes the same document");
    assert.equal(restored.events.length,3);
    if (page.backend === "codex") {
      assert(restored.observerState.connected);
      const grant=payload.grants.find(value=>!value.revokedAt); assert(grant);
      const slug=endpoint=>endpoint.slug || endpoint.session.slug;
      restored.controls.select.value=slug(grant.a) === page.slug ? slug(grant.b) : slug(grant.a);
      const allow=restored.controls["[data-communication-allow]"];
      allow.handlers.click(); await new Promise(setImmediate);
      assert.equal(allow.disabled,false,"Allow remains available on the restored document");
      const row=restored.controls["[data-communication-grants]"].children[0];
      const revoke=row.children.find(node=>node.handlers.click); assert(revoke);
      await revoke.handlers.click();
      assert.equal(revoke.disabled,false,"Revoke remains available on the restored document");
      assert.deepEqual(restored.calls.filter(call=>call.options.method === "POST").map(call=>JSON.parse(call.options.body).action),["allow","revoke"]);
    }
    const failed = fixture(page.html, async () => ({ok: false, json: async () => ({error: "Fixture read unavailable"})}));
    failed.handlers["session-transcript-filter-change"]({detail: "sessions"}); await settle();
    assert.equal(failed.events.length, 0); assert.equal(failed.nodes.get("session-communication-warning").hidden, false);
    assert(f.calls.every(call => call.options.method === undefined), "display reads never mutate");
  }
  console.log("Communication: actual Pi/Codex markup, scoped HTTP rows, labels/filter/dedupe/order, status/reload/member and late-read controls passed.");
}
