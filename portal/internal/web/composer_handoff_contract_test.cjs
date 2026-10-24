"use strict";
const fs = require("node:fs"), vm = require("node:vm"), assert = require("node:assert/strict");
const {randomUUID} = require("node:crypto");
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
const storage = () => ({values: new Map(), getItem(key) {return this.values.get(key) || null;}, setItem(key, value) {this.values.set(key, value);}});
function fixture(attachmentIds = [], saved = storage(), retained = {}) {
  const records = retained.records || new Map(), queues = retained.queues || new Map(), pendingMessages = new Map(), calls = [], failures = [], held = [];
  for (const entry of records.values()) pendingMessages.set(entry.id, {...entry, state: entry.state || "unknown"});
  const textarea = {value: "Original message", disabled: false, focus() {throw new Error("Late focus");}};
  const form = {dataset: {}, elements: {message: textarea}};
  const uploads = {values: [...attachmentIds], locked: false, count() {return this.values.length;},
    ready: () => true, ids() {return [...this.values];}, clear() {this.values = [];}, lock(value) {this.locked = value;}};
  const dispatch = action => (...args) => {
    calls.push({action, args: json(args)}); return new Promise((resolve, reject) => held.push({resolve, reject}));
  };
  const context = vm.createContext({deliveryRetries: null, crypto: {randomUUID}, currentThreadId: "thread-1", conversationID: "conversation", slug: "example",
    client: {message: dispatch("send"), queueMessage: dispatch("queue")}, interactive: true, executionHeld: false,
    liveSettingsSaving: false, threadActive: true, messageDispatchInFlight: false, planImplementationInFlight: false, messageDispatcher: null,
    document: {hidden: false}, navigator: {onLine: true},
    retryFailedMessage: null, discardFailedMessage: null, replaceFailedWithDraft: null, enqueueMessageCopy: null, confirm: () => true, failedMessageDrafts: new Map(),
    form, textarea, composerUploads: uploads, pendingMessages, inFlightMessageIDs: new Set(),
    queueAttemptStorage: {}, sendAttemptStorage: saved, createMessageDispatch: helpers.createMessageDispatch,
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
    renderMessageReceipts() {}, updateMessageActions() {}, followTranscript() {}, scheduleRefresh() {}, refreshQueue() {},
    applyCurrentSettings() {}, acknowledgeTranscriptMessages() {},
  });
  vm.runInContext("let draftGeneration = 0;\n" +
    section("  const markMessageOutcomeUnknown =", "  const recoverPlanAttempts =") +
    section("    const dispatchThreadID =", "    textarea.addEventListener(\"keydown\"") +
    "\nglobalThis.submit = submitMessage; globalThis.retry = retryMessageReceipt; globalThis.resend = resendMessageReceipt;", context);
  return {context, textarea, uploads, calls, records, queues, pendingMessages, failures, saved,
    accept: async () => {held.shift().resolve({steered: true}); await settle();},
    lose: async error => {held.shift().reject(error || new Error("Lost response")); await settle();}};
}
function receiptButtons(f) {
  if (!f.context.testRenderReceipts) {
    const element = tag => ({tag, children: [], get childElementCount() {return this.children.length;}, dataset: {}, textContent: "", append(...nodes) {this.children.push(...nodes);},
      replaceChildren(...nodes) {this.children=nodes;}, setAttribute() {}, addEventListener() {}});
    f.receiptContainer=element("div");
    f.context.document={activeElement:null,getElementById:()=>f.receiptContainer,createElement:element};
    f.context.messageReceiptLabel=helpers.messageReceiptLabel;
    vm.runInContext(section("  const renderMessageReceipts =", "  loadMessageReceipts();")+
      "globalThis.testRenderReceipts=renderMessageReceipts;",f.context);
  }
  f.context.testRenderReceipts();
  return f.receiptContainer.children.map(item=>({label:item.children[0].textContent,
    buttons:item.children.find(child=>child.className==="message-receipt-actions").children.map(button=>button.textContent)}));
}
module.exports = {fixture, storage, settle, section};
if (require.main === module) (async () => {
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
  const reload = fixture([], f.saved); await reload.context.messageDispatcher.drain(); assert.equal(reload.calls.length, 0);
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
  const tailCopy=tail.context.resend(legacyID); await settle(); await tail.lose(); await tailCopy;
  const tailID=tail.records.get(legacyID).replacementId;
  assert.deepEqual(json(tail.context.messageDispatcher.entries.map(entry=>entry.id)),[queuedID,tailID]);
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
  completed = true;
  console.log("Composer: immutable ordered sends/queues, recovery barriers, reload, storage failures, edit and bounds passed.");
})().catch(error => {console.error(error); process.exitCode = 1;});
