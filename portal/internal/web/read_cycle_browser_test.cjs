"use strict";
const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const {createRequest, createReadScope, createCodexLimitsReader, teamWorkPresentation} = require("./static/app.js");
const source = fs.readFileSync(__dirname + "/static/app.js", "utf8");
const tick = () => new Promise(setImmediate);
const deferred = () => {let resolve, reject; const promise = new Promise((a,b) => {resolve=a;reject=b;}); return {promise,resolve,reject};};

test("JSON body abort/transport failure and malformed success never become snapshots", async () => {
  for (const bodyFailure of [new SyntaxError("invalid JSON"), new TypeError("body connection lost")]) {
    const request = createRequest(async () => ({ok:true,status:200,json:async () => {throw bodyFailure;}}));
    await assert.rejects(request("/fixture"), error => error === bodyFailure);
  }
  for (const rejectBody of [false,true]) {
    const body = deferred(), abort = new AbortController();
    const request = createRequest(async () => ({ok:true,status:200,json:() => body.promise}));
    const reading = request("/fixture", {signal:abort.signal}); await tick();
    abort.abort(); if (rejectBody) body.reject(abort.signal.reason); else body.resolve({late:true});
    await assert.rejects(reading, error => error.name === "AbortError");
  }
  await assert.rejects(createRequest(async () => ({ok:false,status:503,json:async () => {throw new SyntaxError();}}))("/fixture"), /Request failed \(503\)/);
  assert.deepEqual(await createRequest(async () => ({ok:true,status:204,json:() => {throw Error("bodyless");}}))("/fixture"), {});
});

test("limits pause retires the old slot before its transport settles", async () => {
  for (const fails of [false,true]) {
    const reads=[], applied=[];
    const limits=createCodexLimitsReader((_url, options) => {const read=deferred(); reads.push({...read,signal:options.signal}); return read.promise;}, value=>applied.push(value));
    const old=limits.refresh(); limits.pause(); const fresh=limits.refresh();
    assert.equal(reads.length,2); assert(reads[0].signal.aborted);
    if (fails) reads[0].reject(Error("old")); else reads[0].resolve({old:true});
    await old; assert.equal(limits.refresh(),fresh); assert.equal(applied.length,0);
    reads[1].resolve({fresh:true}); await fresh; assert.deepEqual(applied,[{fresh:true}]);
  }
});

test("deadline failure stays visible and access failures keep their status",async()=>{
  const failures=[];
  const limits=createCodexLimitsReader((_url,{signal})=>new Promise((_resolve,reject)=>signal.addEventListener("abort",()=>reject(signal.reason))),
    (_snapshot,failed,error)=>{if(failed)failures.push(error);},{deadlineMs:5});
  await limits.refresh();assert.equal(failures[0].name,"TimeoutError");assert.equal(failures[0].message,"The request timed out. Try again.");
  const denied=Object.assign(Error("Access denied"),{status:403});
  const restricted=createCodexLimitsReader(async()=>{throw denied;},(_snapshot,failed,error)=>{if(failed)failures.push(error);},{retryInitialMs:5});
  await restricted.refresh();assert.equal(failures[1],denied);restricted.pause();limits.pause();
});

// Mount the shipped lane functions, preserving their actual slot/finally wiring.
// The transport deliberately ignores AbortSignal to exercise late settlements.
function lane(name, typed = false) {
  const reads=[], applied=[], failures=[], timers=new Map(); let timerID=0,time=100000;
  const request=(_url, options) => {const read=deferred(); reads.push({...read,signal:options.signal}); return read.promise;};
  const element={hidden:true,textContent:"",querySelector:()=>({textContent:""}),querySelectorAll:()=>[],classList:{contains:()=>true},addEventListener(){}};
  const notice={begin(){},success(){applied.push("success");},failure(error){failures.push(error);applied.push(error.message);},cancel(){}};
  const context={createReadScope,Date:class extends Date {static now(){return time;}},document:{hidden:false,getElementById:()=>element,querySelector:()=>null,querySelectorAll:()=>[]},
    setTimeout:(fn,delay)=>{timers.set(++timerID,{fn,delay});return timerID;},clearTimeout:id=>timers.delete(id),
    refreshPolicy:{detailsDeadlineMs:10000,pendingDeadlineMs:10000,queueDeadlineMs:10000,detailsMs:5000,retryInitialMs:2000,retryMaxMs:30000,activityIdleMs:5000,activityActiveMs:1000},
    pendingStatus:element,pageReads:createReadScope(),client:{details:options=>request("details",options),pending:options=>request("pending",options),reconcileQueue:async()=>{},queue:options=>request("queue",options),activity:options=>request("activity",options)},request,
    slug:"example",interactive:true,executionHeld:false,artifactList:{},teamSettingsGeneration:0,teamSettingsGenerationRead:0,
    conversationAssets:{createRefreshNotice:()=>notice},reconcileCompletedArchive:()=>false,refreshLifecycleTarget(){},applyRecoveryStatus(){},
    repositoryReviewLoading:null,repositoryReview:null,selectedMember:"",updateTeamWork(){},refreshTeamLeadSettings(){},teamActivityPresentation:()=>({}),
    renderPendingEntries:entries=>applied.push(entries),sync:{pendingInvalidationSupported:typed},
    threadActive:false,liveSettingsSaving:false,renderQueue:entries=>applied.push(entries),activitySnapshot:null,activityAvailable:false,activityRead:null,lastActivityReadAt:-Infinity,
    timingClock:{received:()=>applied.push("received"),failed:()=>applied.push("failed")},scheduleActivity(){},updateCodexWork(){}};
  let start,end,expression;
  if(name==="details") {start=source.indexOf("  let detailsTimer =");end=source.indexOf("  const suspendPageReads",start);expression="refreshSessionDetails";}
  else if(name==="team") {start=source.indexOf("  let teamStatsRead =");end=source.indexOf("  document.querySelector('[data-session-tab=\"team\"]')",start);expression="refreshTeamStats";}
  else if(name==="activity") {start=source.indexOf("  const refreshActivity =");end=source.indexOf("  pauseTiming =",start);expression="refreshActivity";}
  else if(name==="pending") {start=source.indexOf("  const pendingRetry =");end=source.indexOf("  pendingRetry?.addEventListener",start);expression="refreshPending";}
  else {start=source.indexOf("  const queueStatus =");end=source.indexOf("  queueRetry?.addEventListener",start);expression="refreshQueue";}
  assert(start>0 && end>start, name);
  const api=vm.runInNewContext(source.slice(start,end)+`\n({refresh:${expression},${name==="team"?"payload:()=>teamStatsPayload,":""}})`,context);
  return {api,context,reads,applied,failures,timers,setTime:value=>{time=value;},retire(){context.pageReads.pause();context.pageReads.resume();}};
}
for(const name of ["details","team","activity","pending","queue"]) {
  test(`${name} starts a resumed read before old success/error/finally settles`,async()=>{
    for(const fails of [false,true]) {
      const h=lane(name), old=h.api.refresh(); await tick(); h.retire(); const fresh=h.api.refresh(true); await tick();
      assert.equal(h.reads.length,2,name); assert(h.reads[0].signal.aborted);
      if(fails) h.reads[0].reject(Error("old failure")); else h.reads[0].resolve(name==="pending"||name==="queue"?[]:{});
      await old; assert.equal(h.applied.length,0,name);
      void h.api.refresh(); await tick(); assert.equal(h.reads.length,2,"old finally cannot clear the successor");
      h.reads[1].resolve(name==="pending"||name==="queue"?[]:{}); await fresh;
      h.context.pageReads.pause();
    }
  });
}

test("typed pending hints respect successful freshness and force one invalidation rerun",async()=>{
  const h=lane("pending",true); const first=h.api.refresh();h.reads[0].resolve([]);await first;
  for(let i=0;i<300;i++) {h.setTime(100000+i*40);await h.api.refresh();}
  assert.equal(h.reads.length,1);
  h.setTime(115001);const due=h.api.refresh();assert.equal(h.reads.length,2);
  for(let i=0;i<20;i++) void h.api.refresh(true);
  h.reads[1].resolve([]);await due;await tick();assert.equal(h.reads.length,3);
  h.reads[2].resolve([]);await tick();assert.equal(h.reads.length,3);
  h.context.pageReads.pause();
  const legacy=lane("pending",false);let read=legacy.api.refresh();legacy.reads[0].resolve([]);await read;
  read=legacy.api.refresh();assert.equal(legacy.reads.length,2);legacy.reads[1].resolve([]);await read;legacy.context.pageReads.pause();
});

test("typed pending failure after recent success retries on backoff",async(t)=>{
  const h=lane("pending",true), questions=[{id:"retained-question"}];
  t.after(()=>h.context.pageReads.pause());
  let reading=h.api.refresh();h.reads[0].resolve(questions);await reading;
  let time=100100;h.setTime(time);
  reading=h.api.refresh(true);h.reads[1].reject(Object.assign(Error("pending 503"),{status:503}));await reading;
  assert.equal(h.applied.filter(Array.isArray).at(-1),questions,"failure must retain displayed questions");
  assert.equal(h.failures.at(-1).status,503);
  for(let i=0;i<100;i++){h.setTime(time+i);await h.api.refresh();}
  assert.equal(h.reads.length,2,"ordinary hints must respect retry backoff");
  const fireRetry=async(delay)=>{
    const entry=[...h.timers].find(([_id,timer])=>timer.delay===delay);
    assert(entry,`expected retry delay ${delay}`);
    h.timers.delete(entry[0]);h.setTime(time+=delay);entry[1].fn();await tick();
  };
  await fireRetry(2000);
  assert.equal(h.reads.length,3,"the 2s retry must dispatch inside the previous success window");
  void h.api.refresh();void h.api.refresh();
  assert.equal(h.reads.length,3,"retry transport must remain single flight");
  h.reads[2].reject(Error("pending 503 again"));await tick();
  let count=3;
  for(const [index,delay] of [4000,8000,16000,30000,30000].entries()){
    for(let hint=0;hint<20;hint++)void h.api.refresh();
    assert.equal(h.reads.length,count,"hints must not bypass a scheduled retry");
    await fireRetry(delay);assert.equal(h.reads.length,++count);
    assert.equal(h.applied.filter(Array.isArray).at(-1),questions);
    if(index===4)h.reads.at(-1).resolve(questions);else h.reads.at(-1).reject(Error("pending still unavailable"));
    await tick();
  }
  assert.equal(h.applied.filter(value=>value==="success").length,2,"only confirmed reads establish success");
  for(let i=0;i<300;i++){h.setTime(time+i*40);await h.api.refresh();}
  assert.equal(h.reads.length,count,"recovery must restore the successful 15s freshness floor");
  reading=h.api.refresh(true);assert.equal(h.reads.length,++count,"typed invalidation still forces freshness");
  for(let i=0;i<20;i++)void h.api.refresh(true);
  h.reads.at(-1).resolve(questions);await reading;await tick();
  assert.equal(h.reads.length,++count,"forced invalidations must coalesce to one rerun");
  h.reads.at(-1).resolve(questions);await tick();
  reading=h.api.refresh(true);h.reads.at(-1).reject(Error("new failure"));await reading;
  assert([...h.timers.values()].some(timer=>timer.delay===2000),"successful recovery must reset backoff to 2s");
});

test("retired pending failure preserves successor success freshness",async(t)=>{
  const h=lane("pending",true), questions=[{id:"current-question"}];
  t.after(()=>h.context.pageReads.pause());
  const old=h.api.refresh();h.retire();h.setTime(100100);
  const fresh=h.api.refresh(true);h.reads[1].resolve(questions);await fresh;
  const effects=[...h.applied],timers=[...h.timers];
  h.reads[0].reject(Object.assign(Error("retired access failure"),{status:403}));await old;
  assert.deepEqual(h.applied,effects,"retired failure must not render a warning");
  assert.equal(h.failures.length,0);assert.deepEqual([...h.timers],timers,"retired failure must not change timers");
  for(let i=0;i<300;i++){h.setTime(100100+i*40);await h.api.refresh();}
  assert.equal(h.reads.length,2,"retired failure must not erase successor freshness");
  const forced=h.api.refresh(true);void h.api.refresh();
  assert.equal(h.reads.length,3,"current successor remains single flight");
  h.reads[2].resolve(questions);await forced;
});

test("current pending access failure keeps its status and last questions",async(t)=>{
  const h=lane("pending",true), questions=[{id:"authorized-last-value"}];
  t.after(()=>h.context.pageReads.pause());
  let reading=h.api.refresh();h.reads[0].resolve(questions);await reading;
  const denied=Object.assign(Error("Access denied"),{status:403});
  reading=h.api.refresh(true);h.reads[1].reject(denied);await reading;
  assert.equal(h.failures.at(-1),denied);assert.equal(h.failures.at(-1).status,403);
  assert.equal(h.applied.filter(Array.isArray).at(-1),questions);
});

test("team HTTP200 preserves genuine stale observation until fresh evidence",async()=>{
  const h=lane("team"), now=Date.now();
  let read=h.api.refresh();h.reads[0].resolve({members:[{address:"implementer0",state:"ready",snapshot:{currentState:"working",stale:true,observedAtMs:now-20000}}]});await read;
  assert(teamWorkPresentation(h.api.payload().members,"lead",now).stale);
  read=h.api.refresh();h.reads[1].resolve({members:[{address:"implementer0",state:"ready",snapshot:{currentState:"working",stale:false,observedAtMs:now}}]});await read;
  assert.equal(teamWorkPresentation(h.api.payload().members,"lead",now).stale,false);
  h.context.pageReads.pause();
});

function historyLane({gap = true, citationsPending = false} = {}) {
  const reads = [], applied = [], effects = [], timers = new Map(), handlers = new Map();
  let timerID = 0;
  const elements = new Map(["history-controls", "repair-history", "history-status"].map(id => [id, {
    hidden: true, disabled: false, textContent: "", addEventListener: (event, fn) => handlers.set(id + ":" + event, fn),
  }]));
  const retained = {rows: [{node: {}}], olderCursor: "older-1", repairCursor: "repair-1", repairVersion: 1,
    hasOlder: true, gap, citationsPending};
  const draft = {message: "Unsent draft", attachments: ["upload-1"]};
  const context = {AbortController, DOMException, document: {hidden: false, getElementById: id => elements.get(id)},
    setTimeout: (fn, delay) => {timers.set(++timerID, {fn, delay}); return timerID;}, clearTimeout: id => timers.delete(id),
    refreshPolicy: {transcriptDeadlineMs: 35000, historyPageMs: 40, retryInitialMs: 2000, retryMaxMs: 30000},
    transcriptHistory: retained, pagingUnavailable: false, currentThreadId: "thread-1", draft,
    resetHistoryNavigation: () => effects.push("controls"),
    renderThread: (page, isCurrent, kind) => {assert(isCurrent()); applied.push({page, kind}); retained.gap = false; retained.citationsPending = false;},
    client: {threadPage(options) {const read = deferred(); reads.push({...read, ...options}); return read.promise;}},
    conversationAssets: {
      readTranscriptPage: (client, options) => client.threadPage(options),
      createRefreshNotice: ({render}) => {
        let state = {loading: false, warning: false};
        const publish = kind => {effects.push(kind); render(state);};
        return {
          begin() {state = {...state, loading: true}; publish("begin");},
          success() {state = {loading: false, warning: false}; publish("success");},
          failure(error) {state = {loading: false, warning: true, error}; publish("failure");},
          cancel() {state = {...state, loading: false}; publish("cancel");},
        };
      },
    },
    scheduleRefresh: () => effects.push("refresh"),
    filesReads: {pause() {}, resume() {}}, filesLoaded: true,
    pauseFiles() {}, resumeFiles() {},
    pageLeaving: false, detailsTimer: null, teamStatsReceivedAt: 0, repositoryReview: null,
    pauseTiming() {}, resumeTiming() {}, refreshSessionDetails() {}, refreshTeamStats() {},
    pendingRetryTimer: null, pendingDueTimer: null, queueRetryTimer: null, pendingRerun: false, queueRerun: false,
    pendingNotice: {cancel() {}}, queueNotice: {cancel() {}}, detailsNotice: {cancel() {}},
    refreshPending() {}, refreshQueue() {},
  };
  const slice = (start, end) => {
    const first = source.indexOf(start), last = source.indexOf(end, first);
    assert(first > 0 && last > first, start);
    return source.slice(first, last);
  };
  const api = vm.runInNewContext(
    slice("  const createReadScope =", "  const createTimingClock =") +
    "\nconst pageReads = createReadScope(); let historyRead = null, historyRetryTimer = null, historyError = '', historyFailure = null;\n" +
    "let startOlderHistoryRead, resumeHistory, pauseConversationReads, resumeConversationReads;\n" +
    slice("  const historyControls =", "  const observePageReceipts =") +
    slice("  const runHistoryRead =", "  const showPromptStatus =") +
    slice("  pauseConversationReads =", "  function scheduleRefresh(") +
    slice("  const suspendPageReads =", "  // Firefox can reject") +
    `\n({read: runHistoryRead, suspend: suspendPageReads, resume: resumePageReads, schedule: scheduleHistoryRepair,
      state: () => ({read: historyRead, timer: historyRetryTimer, error: historyError, failure: historyFailure,
        notice: historyNoticeState, retryDelay: historyRetryDelay})})`, context);
  return {api, reads, applied, effects, timers, retained, draft, elements, handlers,
    fire(id) {const timer = timers.get(id); assert(timer, "expected owned timer"); timers.delete(id); timer.fn();}};
}

test("history retires loading and starts one resumed repair before old success or failure settles", async () => {
  for (const kind of ["older", "repair"]) for (const fails of [false, true]) {
    const h = historyLane(), row = h.retained.rows[0], draft = h.draft.attachments;
    const old = h.api.read(kind); await tick();
    h.api.suspend();
    assert(h.reads[0].signal.aborted);
    assert.equal(h.api.state().read, null);
    assert.equal(h.api.state().notice.loading, false);
    assert.equal(h.api.state().timer, null);
    h.api.resume(); h.api.resume();
    const repairTimer = h.api.state().timer;
    assert.equal(h.timers.get(repairTimer).delay, 40);
    h.fire(repairTimer); await tick();
    assert.equal(h.reads.length, 2);
    assert.equal(h.reads[1].cursor, "repair-1");
    assert.equal(h.reads[1].expectedThreadId, "thread-1");
    const successor = h.api.state().read, effects = h.effects.slice(), timers = [...h.timers];
    assert(successor);
    assert.equal(h.elements.get("repair-history").disabled, true);
    h.api.schedule(); await h.api.read("older");
    assert.deepEqual([...h.timers], timers, "in-flight repair creates no 100ms busy loop");
    if (fails) h.reads[0].reject(Error("retired failure")); else h.reads[0].resolve({entries: ["retired"]});
    await old;
    assert.equal(h.api.state().read, successor, "old finally cannot clear the successor");
    assert.equal(h.api.state().notice.loading, true);
    assert.equal(h.elements.get("repair-history").disabled, true);
    assert.deepEqual(h.effects, effects, "old settlement cannot render, cancel notices or schedule work");
    assert.deepEqual([...h.timers], timers);
    assert.equal(h.applied.length, 0);
    assert.equal(h.retained.rows[0], row);
    assert.equal(h.retained.olderCursor, "older-1");
    assert.equal(h.draft.message, "Unsent draft"); assert.equal(h.draft.attachments, draft);
    h.reads[1].resolve({entries: ["current"]}); await tick();
    assert.equal(h.applied.length, 1);
    assert.equal(h.applied[0].kind, "repair");
    assert.equal(h.api.state().read, null);
    assert.equal(h.api.state().notice.loading, false);
    assert.equal(h.api.state().timer, null);
    h.api.suspend();
  }
});

test("canceled manual history stays retryable and retains a prior failure without replay", async () => {
  const h = historyLane({gap: false});
  const first = h.api.read("older"); h.reads[0].reject(Error("current failure")); await first;
  const failure = h.api.state().failure, error = h.api.state().error;
  assert.equal(failure.kind, "older"); assert(error);
  const retry = h.api.read("older"); await tick(); h.api.suspend(); h.api.resume();
  assert.equal(h.api.state().failure, failure); assert.equal(h.api.state().error, error);
  assert.equal(h.elements.get("repair-history").disabled, false);
  assert.equal(h.api.state().timer, null, "manual older reads are not automatically replayed");
  h.handlers.get("repair-history:click")(); await tick();
  assert.equal(h.reads.length, 3); assert.equal(h.reads[2].cursor, "older-1");
  const effects = h.effects.slice(); h.reads[1].reject(Error("late retry failure")); await retry;
  assert.deepEqual(h.effects, effects);
  h.reads[2].resolve({entries: []}); await tick();
  assert.equal(h.api.state().failure, null); assert.equal(h.api.state().error, "");
  h.api.suspend();
});

test("history resume repairs pending citations and retires backoff without losing current errors", async () => {
  const h = historyLane({gap: false, citationsPending: true});
  const first = h.api.read("repair"); h.reads[0].reject(Error("current failure")); await first;
  const failure = h.api.state().failure, timer = h.api.state().timer;
  assert.equal(h.timers.get(timer).delay, 2000);
  h.api.suspend(); assert(!h.timers.has(timer)); assert.equal(h.api.state().timer, null);
  h.api.resume(); h.api.resume(); const resumed = h.api.state().timer;
  assert(resumed); h.fire(resumed); await tick();
  assert.equal(h.reads.length, 2); assert.equal(h.api.state().failure, failure);
  h.reads[1].resolve({entries: []}); await tick();
  assert.equal(h.api.state().failure, null); assert.equal(h.api.state().timer, null);
  h.api.suspend();
});

test("history current deadline and access failures remain visible", async () => {
  for (const status of [401, 403, "deadline"]) {
    const h = historyLane(), reading = h.api.read("repair");
    let expected;
    if (status === "deadline") {
      const deadline = [...h.timers].find(([, timer]) => timer.delay === 35000)[0];
      h.fire(deadline); expected = h.reads[0].signal.reason;
      h.reads[0].resolve({entries: ["past deadline"]});
    } else {
      expected = Object.assign(Error("Access denied"), {status}); h.reads[0].reject(expected);
    }
    await reading;
    assert.equal(h.applied.length, 0);
    assert.equal(h.api.state().notice.warning, true);
    assert.equal(h.api.state().notice.error, expected);
    assert.equal(h.elements.get("repair-history").disabled, false);
    assert.equal(h.api.state().failure.kind, "repair");
    if (status === "deadline") {assert.equal(expected.name, "TimeoutError"); assert(h.api.state().timer);}
    else assert.equal(h.api.state().timer, null, "access errors do not auto retry");
    h.api.suspend();
  }
});
