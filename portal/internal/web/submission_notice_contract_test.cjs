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
function fixture(options = {}) {
  let observation; const ackResults=[];
  const status = {hidden:true, textContent:""}, records = new Map(), pendingMessages = new Map();
  const form = {elements:{message:{value:"New draft"}}};
  const uploads = {values:["new-upload"], ids() {return this.values;}, ready:()=>true, clear() {this.values=[];}};
  const context = vm.createContext({deliveryRetries: null,
    conversationID:"conversation", currentThreadId:"thread", slug:"session", interactive:true,
    confirm:()=>true, crypto:{randomUUID:()=>"00000000-0000-4000-8000-000000000100"},
    executionHeld:false, liveSettingsSaving:false, messageDispatchInFlight:false, planImplementationInFlight:false, messageDispatcher:null, enqueueMessageCopy:null, updateMessageActions(){}, form, composerUploads:uploads, pendingMessages,
    inFlightMessageIDs:new Set(), sendAttemptStorage:{}, pageReads:{paused:false}, transcriptEntries:[],
    document:{getElementById:id=>id==="message-submission-status" ? status : form},
    conversationAssets:{sameAttachments:(a=[],b=[])=>JSON.stringify(a)===JSON.stringify(b),
      submissionFailurePresentation:error=>({message:typeof error.notSent!=="boolean" ? error.message : error.notSent ? "Definitely not sent" : "Delivery remains uncertain"})},
    optionalQuestions:{ordinaryAccepted(){}}, renderMessageReceipts(){}, scheduleRefresh(){},
    storeSendAttempt:(_storage,_slug,thread,attempt)=>{if (thread==="thread") records.set(attempt.id,{...attempt}); return true;},
    loadSendAttempts:()=>[...records.values()], deleteSendAttempt:(_storage,_slug,_thread,id)=>records.delete(id),
    markTranscriptMessagesObserved:(...args)=>(observation=helpers.markTranscriptMessagesObserved(...args)),
    sendAcknowledgementCandidates:helpers.sendAcknowledgementCandidates,
    createTranscriptAcknowledgementScheduler:options.directACK ? run=>()=>Promise.resolve(run()).then(result=>{ackResults.push(result);}) : helpers.createTranscriptAcknowledgementScheduler,
    client:{message:async()=>({steered:false}), acknowledgeMessages:async batch=>({acknowledgedClientUserMessageIds:batch.map(x=>x.clientUserMessageId)})},
  });
  vm.runInContext([
    section('  const submissionStatus =', '  const form = document.getElementById("message-form");'),
    section('  const markMessageOutcomeUnknown =', '  const recoverPlanAttempts ='),
    section('  const acknowledgeTranscriptMessages =', '  const renderPlanActions ='),
    section('  const observePageReceipts =', '  const renderThread ='),
    'globalThis.test = {showSubmissionFailure, clearSubmissionFailure, submissionIdentity, beginSubmission, acceptMessageReceipt, retryMessageReceipt, resendMessageReceipt, observePageReceipts, acknowledgeTranscriptMessages};',
  ].join("\n"), context);
  const attempt = {id:"original", message:"Original message", state:"unknown", attachmentIds:["original-upload"]};
  pendingMessages.set(attempt.id, {...attempt}); records.set(attempt.id, {...attempt});
  const identity = context.test.submissionIdentity("thread", attempt.id);
  const fail = () => context.test.showSubmissionFailure(new Error("Delivery could not be confirmed."), identity);
  const entry = {kind:"userMessage",clientUserMessageId:attempt.id,text:"Canonical envelope",displayText:attempt.message,attachments:[{id:"original-upload"}],clientUserMessageDigest:createHash("sha256").update("Canonical envelope").digest("hex")};
  const observe = async (entries = [entry]) => {
    context.transcriptEntries=entries; context.test.observePageReceipts(entries,"thread");
    await observation; await tick(); await tick();
  };
  return {context,status,records,pendingMessages,form,uploads,attempt,identity,fail,observe,ackResults};
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
  // A newer explicit success clears the composer error, without changing the
  // earlier uncertain receipt. An old failure cannot resurrect that banner.
  f=fixture();
  const oldOperation=f.context.test.beginSubmission(f.identity);
  f.context.test.showSubmissionFailure(new Error("Old failure"),f.identity,oldOperation);
  const revised={...f.attempt,id:"revised",message:"Reworded message"};
  const newOperation=f.context.test.beginSubmission({...f.identity,id:revised.id});
  f.context.test.acceptMessageReceipt(revised,{},"thread",newOperation);
  assert(f.status.hidden); assert.equal(f.records.get(f.attempt.id).state,"unknown");
  f.context.test.showSubmissionFailure(new Error("Late old failure"),f.identity,oldOperation);
  assert(f.status.hidden);

  // Failed browser persistence cannot establish non-delivery of an earlier
  // uncertain send, and prevents this retry from reaching HTTP.
  f=fixture(); f.context.storeSendAttempt=()=>false;
  f.context.client.message=async()=>assert.fail("storage failure dispatched a retry");
  await f.context.test.retryMessageReceipt(f.attempt.id);
  assert.equal(f.status.textContent,"Delivery remains uncertain");

  // An old queue-start completion cannot replace or hide a newer composer error.
  for (const fails of [false,true]) {
    f=fixture(); let runQueue,resolve,reject;
    f.context.queueStart={dataset:{queuedSubmissionId:"queued"},addEventListener:(_event,cb)=>{runQueue=cb;}};
    f.context.queueStartInFlight=false;
    f.context.followTranscript=()=>{}; f.context.refreshQueue=async()=>{};
    f.context.client.startQueue=()=>new Promise((yes,no)=>{resolve=yes;reject=no;});
    vm.runInContext(section('  queueStart?.addEventListener("click", async () => {','  const planActions ='),f.context);
    const old=runQueue();
    const newer={...f.identity,id:"newer"};
    const operation=f.context.test.beginSubmission(newer);
    f.context.test.showSubmissionFailure(Object.assign(new Error("Newer error"),{notSent:true}),newer,operation);
    if (fails) reject(new Error("Old queue failure")); else resolve();
    await old;
    assert.equal(f.status.textContent,"Definitely not sent");
    assert(!f.status.hidden);
  }

  // Copy orchestration awaits reconciliation, and delegates an immutable copy
  // only after the remaining uncertainty and duplicate warning are accepted.
  for (const outcome of ["unknown", "accepted", "declined", "unsent"]) {
    f=fixture(); const calls=[], copies=[]; let confirmations=0;
    f.context.confirm=()=>{confirmations++; return outcome!=="declined";};
    f.context.enqueueMessageCopy=async (...args)=>copies.push(args);
    f.context.client.message=async (...args)=>{
      calls.push(args);
      if (outcome==="unsent") throw Object.assign(new Error("Not sent"),{status:503,code:"message_not_sent",notSent:true});
      if (outcome!=="accepted") throw Object.assign(new Error("Unknown"),{code:"message_delivery_unknown"});
      return {clientUserMessageId:args[1],turnId:"accepted"};
    };
    await f.context.test.resendMessageReceipt(f.attempt.id);
    assert.equal(confirmations,["accepted","unsent"].includes(outcome) ? 0 : 1);
    assert.equal(calls.length,1); assert.deepEqual(copies,outcome==="unknown" ? [[f.attempt.id,true]] : []);
    assert.equal(f.form.elements.message.value,"New draft"); assert.deepEqual(f.uploads.values,["new-upload"]);
  }
  // Authoritative nonadmission on recovered IDs keeps the immutable retry ID.
  f=fixture(); f.context.client.message=async()=>{throw Object.assign(new Error("Not sent"),{status:503,code:"message_not_sent",notSent:true});};
  await f.context.test.retryMessageReceipt(f.attempt.id);
  assert.equal(f.records.get(f.attempt.id).state,"unsent");
  assert.equal(f.pendingMessages.get(f.attempt.id).state,"unsent");
  f.context.client.message=async (_text,id)=>{
    assert.equal(id,f.attempt.id); assert.equal(f.records.get(id).state,"unknown"); return {};
  };
  await f.context.test.retryMessageReceipt(f.attempt.id); assert(f.status.hidden);
  // Linked observed proof is retained without an acknowledgement retry loop.
  f=fixture({directACK:true}); let acknowledgements=0;
  const parent={id:"parent",message:"Earlier original",state:"unknown",replacementId:f.attempt.id};
  f.records.set(parent.id,parent); f.pendingMessages.set(parent.id,parent);
  f.context.client.acknowledgeMessages=async batch=>{acknowledgements++; return {acknowledgedClientUserMessageIds:batch.map(x=>x.clientUserMessageId)};};
  await f.observe(); assert.equal(acknowledgements,0); assert.equal(f.records.get(f.attempt.id).state,"observed");
  assert.equal(f.ackResults.some(result=>result?.retry),false);
  const parentEntry={kind:"userMessage",clientUserMessageId:parent.id,text:parent.message,clientUserMessageDigest:createHash("sha256").update(parent.message).digest("hex")};
  await f.observe([...f.context.transcriptEntries,parentEntry]);
  assert(!f.records.has(parent.id),"original can retire on its own positive proof");
  assert(f.records.has(f.attempt.id));
  f.context.test.acknowledgeTranscriptMessages(); await tick(); await tick();
  assert.equal(acknowledgements,2); assert(!f.records.has(f.attempt.id));

  // Recheck durable links after the asynchronous ACK, including failed reads.
  for (const scenario of ["new-link","storage-unavailable"]) {
    f=fixture({directACK:true}); let resolveAck;
    f.context.client.acknowledgeMessages=batch=>new Promise(resolve=>{resolveAck=()=>resolve({acknowledgedClientUserMessageIds:batch.map(x=>x.clientUserMessageId)});});
    await f.observe(); assert(resolveAck);
    if (scenario==="new-link") f.records.set("parent",{id:"parent",message:"Original",state:"unknown",replacementId:f.attempt.id});
    else f.context.loadSendAttempts=()=>null;
    resolveAck(); await tick(); await tick();
    assert(f.records.has(f.attempt.id),"post-ACK reference/read uncertainty must retain proof");
    assert.equal(f.records.get(f.attempt.id).state,"observed");
    if (scenario==="new-link") assert.equal(f.ackResults.some(result=>result?.retry),false);
  }
  console.log("Submission recovery preserves IDs and drafts, and newer success clears stale composer feedback.");
})().catch(error=>{console.error(error);process.exitCode=1;});
