"use strict";
const fs = require("node:fs"), vm = require("node:vm"), assert = require("node:assert/strict");
const {createHash} = require("node:crypto");
const helpers = require("./static/app.js");
const source = fs.readFileSync("static/app.js", "utf8");
const section = (start, end) => {
  const a = source.indexOf(start), b = source.indexOf(end, a);
  assert(a > 0 && b > a);
  return source.slice(a, b);
};
const tick = () => new Promise(resolve => setImmediate(resolve));
function fixture() {
  let observation;
  const status = {hidden:true, textContent:""}, records = new Map(), pendingMessages = new Map();
  const form = {elements:{message:{value:"New draft"}}};
  const uploads = {values:["new-upload"], ids() {return this.values;}, ready:()=>true, clear() {this.values=[];}};
  const context = vm.createContext({
    conversationID:"conversation", currentThreadId:"thread", slug:"session", interactive:true,
    executionHeld:false, liveSettingsSaving:false, messageDispatchInFlight:false, updateMessageActions(){}, form, composerUploads:uploads, pendingMessages,
    inFlightMessageIDs:new Set(), sendAttemptStorage:{}, pageReads:{paused:false}, transcriptEntries:[],
    document:{getElementById:id=>id==="message-submission-status" ? status : form},
    conversationAssets:{sameAttachments:(a=[],b=[])=>JSON.stringify(a)===JSON.stringify(b)},
    optionalQuestions:{ordinaryAccepted(){}}, renderMessageReceipts(){}, scheduleRefresh(){},
    storeSendAttempt:(_storage,_slug,thread,attempt)=>{if (thread==="thread") records.set(attempt.id,{...attempt}); return true;},
    loadSendAttempts:()=>[...records.values()], deleteSendAttempt:(_storage,_slug,_thread,id)=>records.delete(id),
    markTranscriptMessagesObserved:(...args)=>(observation=helpers.markTranscriptMessagesObserved(...args)),
    sendAcknowledgementCandidates:helpers.sendAcknowledgementCandidates,
    createTranscriptAcknowledgementScheduler:helpers.createTranscriptAcknowledgementScheduler,
    client:{message:async()=>({steered:false}), acknowledgeMessages:async batch=>({acknowledgedClientUserMessageIds:batch.map(x=>x.clientUserMessageId)})},
  });
  vm.runInContext([
    section('  const submissionStatus =', '  const form = document.getElementById("message-form");'),
    section('  const markMessageOutcomeUnknown =', '  const recoverPlanAttempts ='),
    section('  const acknowledgeTranscriptMessages =', '  const renderPlanActions ='),
    section('  const observePageReceipts =', '  const renderThread ='),
    'globalThis.test = {showSubmissionFailure, clearSubmissionFailure, submissionIdentity, acceptMessageReceipt, retryMessageReceipt, observePageReceipts};',
  ].join("\n"), context);
  const attempt = {id:"original", message:"Original message", state:"unknown", attachmentIds:["original-upload"]};
  pendingMessages.set(attempt.id, {...attempt}); records.set(attempt.id, {...attempt});
  const identity = context.test.submissionIdentity("thread", attempt.id);
  const fail = () => context.test.showSubmissionFailure(new Error("Delivery could not be confirmed."), identity);
  const entry = {kind:"userMessage",clientUserMessageId:attempt.id,text:"Canonical envelope",displayText:attempt.message,attachments:[{id:"original-upload"}],clientUserMessageDigest:createHash("sha256").update("Canonical envelope").digest("hex")};
  const observe = async () => {
    context.transcriptEntries=[entry]; context.test.observePageReceipts([entry],"thread");
    await observation; await tick(); await tick();
  };
  return {context,status,records,pendingMessages,form,uploads,attempt,identity,fail,observe};
}
(async () => {
  // A failed same-ID retry keeps both warning and durable recovery record.
  let f=fixture(); f.fail();
  f.context.client.message=async()=>{throw new Error("Still unresolved");};
  await f.context.test.retryMessageReceipt(f.attempt.id);
  assert(!f.status.hidden); assert.equal(f.pendingMessages.get(f.attempt.id).state,"unknown");
  assert(f.records.has(f.attempt.id));
  // Later success clears exactly that warning, preserving a different draft/upload.
  const calls=[]; f.context.client.message=async(...args)=>{calls.push(args); return {};};
  await f.context.test.retryMessageReceipt(f.attempt.id);
  assert(f.status.hidden); assert.equal(f.status.textContent,"");
  assert.equal(f.records.get(f.attempt.id).state,"accepted");
  assert.equal(JSON.stringify(calls),JSON.stringify([[f.attempt.message,f.attempt.id,true,f.attempt.attachmentIds]]));
  assert.equal(f.form.elements.message.value,"New draft"); assert.deepEqual(f.uploads.values,["new-upload"]);

  // Unrelated conversation/thread/attempt/action confirmations cannot hide it.
  f=fixture(); f.fail();
  f.context.test.acceptMessageReceipt({...f.attempt,id:"other"},{},"thread");
  f.context.test.acceptMessageReceipt(f.attempt,{},"other-thread");
  f.context.test.clearSubmissionFailure({...f.identity,conversationID:"other-conversation"});
  f.context.test.clearSubmissionFailure({...f.identity,action:"queue"});
  assert(!f.status.hidden); assert(f.records.has(f.attempt.id));
  assert.equal(f.records.get(f.attempt.id).state,"unknown");
  f.context.test.showSubmissionFailure(new Error("Queue warning"),{...f.identity,action:"queue"});
  f.context.test.acceptMessageReceipt(f.attempt,{});
  assert.equal(f.status.textContent,"Queue warning"); assert(!f.status.hidden);

  // Validated transcript proof clears the warning before acknowledgement;
  // eventual acknowledgement retains a newer draft and different uploads.
  f=fixture(); f.fail(); await f.observe();
  assert(f.status.hidden); assert(!f.records.has(f.attempt.id));
  assert.equal(f.form.elements.message.value,"New draft"); assert.deepEqual(f.uploads.values,["new-upload"]);

  // Observation or acceptance before a lost retry response keeps precedence.
  for (const proof of ["observed","accepted"]) {
    f=fixture(); let reject;
    f.context.client.message=()=>new Promise((_resolve,no)=>{reject=no;});
    const pending=f.context.test.retryMessageReceipt(f.attempt.id);
    if (proof==="observed") await f.observe();
    else f.context.test.acceptMessageReceipt(f.attempt,{});
    assert.equal(f.pendingMessages.get(f.attempt.id).state,proof);
    reject(new Error("Late response loss")); await pending;
    assert(f.status.hidden);
    assert.equal(f.form.elements.message.value,"New draft"); assert.deepEqual(f.uploads.values,["new-upload"]);
  }
  // Equal text with changed uploads is still a different composer draft.
  f=fixture(); f.form.elements.message.value=f.attempt.message; f.fail();
  await f.context.test.retryMessageReceipt(f.attempt.id);
  assert(f.status.hidden); assert.equal(f.form.elements.message.value,f.attempt.message);
  assert.deepEqual(f.uploads.values,["new-upload"]);
  // Even identical new text/attachments are never late-cleared by a receipt
  // or transcript acknowledgment from a previous explicit handoff.
  f=fixture(); f.form.elements.message.value=f.attempt.message;
  f.uploads.values=[...f.attempt.attachmentIds];
  await f.context.test.retryMessageReceipt(f.attempt.id);
  assert.equal(f.form.elements.message.value,f.attempt.message);
  assert.deepEqual(f.uploads.values,f.attempt.attachmentIds);
  await f.observe();
  assert.equal(f.form.elements.message.value,f.attempt.message);
  assert.deepEqual(f.uploads.values,f.attempt.attachmentIds);
  console.log("Submission notices follow exact attempt proof and retain unrelated drafts/records.");
})().catch(error=>{console.error(error);process.exitCode=1;});
