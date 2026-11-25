"use strict";
const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const {createRequest, createReadScope, createCodexLimitsReader, createThreadStatusBadge, resourceCountPresentation, teamWorkPresentation, createLifecycleTargetState, lifecyclePresentation} = require("./static/app.js");
const source = fs.readFileSync(__dirname + "/static/app.js", "utf8");
const tick = () => new Promise(setImmediate);
const deferred = () => {let resolve, reject; const promise = new Promise((a,b) => {resolve=a;reject=b;}); return {promise,resolve,reject};};

test("only entering Conversation schedules an immediate snapshot, independently of layout", () => {
  const slice = (start, end) => {
    const first = source.indexOf(start), last = source.indexOf(end, first + start.length);
    assert(first >= 0 && last > first);
    return source.slice(first, last);
  };
  const sessionPanels = ["codex", "settings"].map(id => {
    let active = id === "codex";
    return {id, classList:{contains:() => active, toggle:(_name, selected) => { active = selected; }}};
  });
  const sessionTabs = sessionPanels.map(({id}) => ({dataset:{sessionTab:id}, classList:{toggle(){}}, setAttribute(){}}));
  const scheduled = [];
  const activate = vm.runInNewContext(
    slice("  const activateSessionTab =", "  sessionTabs.forEach((tab, index)") +
    slice("  if (retryRoot) transcriptResize.observe(retryRoot);", "  document.addEventListener(\"visibilitychange\", resizeTranscript);") +
    slice("  function scheduleRefresh(", "  const markMessageOutcomeUnknown =") + "\nactivateSessionTab",
    {document:new EventTarget(), CustomEvent, sessionPanels, sessionTabs, retryRoot:null,
      transcriptReadable:() => false, sync:{scheduleRefresh:delay => scheduled.push(delay)}},
  );
  activate("codex"); // Click/hash events can reselect the active section.
  activate("settings");
  activate("settings");
  assert.deepEqual(scheduled, []);
  activate("codex");
  activate("codex");
  assert.deepEqual(scheduled, [0]);
  activate("settings");
  activate("codex");
  assert.deepEqual(scheduled, [0, 0]);
});

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
  const context={createReadScope,resourceCountPresentation,Date:class extends Date {static now(){return time;}},document:{hidden:false,getElementById:()=>element,querySelector:()=>null,querySelectorAll:()=>[],addEventListener(){}},
    setTimeout:(fn,delay)=>{timers.set(++timerID,{fn,delay});return timerID;},clearTimeout:id=>timers.delete(id),
    refreshPolicy:{detailsDeadlineMs:10000,pendingDeadlineMs:10000,queueDeadlineMs:10000,detailsMs:5000,retryInitialMs:2000,retryMaxMs:30000,activityIdleMs:5000,activityActiveMs:1000},
    pendingStatus:element,pageReads:createReadScope(),client:{details:options=>request("details",options),pending:options=>request("pending",options),reconcileQueue:async()=>{},queue:options=>request("queue",options),activity:options=>request("activity",options)},request,
    currentThreadId:"thread-1",slug:"example",interactive:true,executionHeld:false,artifactList:{},teamSettingsGeneration:0,teamSettingsGenerationRead:0,
    codexBackend:true,piBackend:false,piPageScope:{epoch:0},
    threadStatusBadge:createThreadStatusBadge(element,"thread-1"),
    conversationAssets:{createRefreshNotice:()=>notice},reconcileCompletedArchive:()=>false,refreshLifecycleTarget(){},applyRecoveryStatus(){},
    repositoryReviewLoading:null,repositoryReview:null,selectedMember:"",updateTeamWork(){},refreshTeamLeadSettings(){},teamActivityPresentation:()=>({}),
    renderPendingEntries:entries=>applied.push(entries),sync:{pendingInvalidationSupported:typed},
    threadActive:false,liveSettingsSaving:false,renderQueue:entries=>applied.push(entries),activitySnapshot:null,activityAvailable:false,activityReadFailed:false,activityRead:null,lastActivityReadAt:-Infinity,activityRequestMs:Infinity,activityReadBinding:"",activityDisplayBinding:()=>"fixture-binding",
    timingClock:{received:()=>applied.push("received"),failed:()=>applied.push("failed")},scheduleActivity(){},updateCodexWork(){}};
  let start,end,expression;
  if(name==="details") {start=source.indexOf("  const countSnapshots =");end=source.indexOf("  const suspendPageReads",start);expression="refreshSessionDetails";}
  else if(name==="team") {start=source.indexOf("  let teamStatsRead =");end=source.indexOf("  document.querySelector('[data-session-tab=\"team\"]')",start);expression="refreshTeamStats";}
  else if(name==="activity") {start=source.indexOf("  const refreshActivity =");end=source.indexOf("  pauseTiming =",start);expression="refreshActivity";}
  else if(name==="pending") {start=source.indexOf("  const pendingRetry =");end=source.indexOf("  pendingRetry?.addEventListener",start);expression="refreshPending";}
  else {start=source.indexOf("  const queueStatus =");end=source.indexOf("  queueRetry?.addEventListener",start);expression="refreshQueue";}
  assert(start>0 && end>start, name);
  const api=vm.runInNewContext(source.slice(start,end)+`\n({refresh:${expression},${name==="team"?"payload:()=>teamStatsPayload,":""}})`,context);
  return {api,context,reads,applied,failures,timers,setTime:value=>{time=value;},retire(){context.pageReads.pause();context.pageReads.resume();}};
}

test("Pi never starts the unsupported pending or queue lane", async () => {
 for(const kind of ["pending","queue"]){
  const h=lane(kind);h.context.codexBackend=false;await h.api.refresh(true);
  assert.equal(h.reads.length,0);assert.equal(h.failures.length,0);assert.equal(h.timers.size,0);
 }
});

function lifecycleDetails(pendingLifecycle = "") {
  const h=lane("details"), elements=new Map(), navigations=[], revivals=[];
  const node=()=>({hidden:false,textContent:"",dataset:{},listeners:{},
    addEventListener(name,handler){this.listeners[name]=handler;},
    toggleAttribute(name,value){this[name]=value;},querySelector:()=>null});
  for(const id of ["lifecycle-operation-status","lifecycle-operation-title","lifecycle-operation-detail",
    "lifecycle-operation-retry","archive-session-open","revive-session-open"]) elements.set(id,node());
  const dialog=node(), form=node(), submit=node();
  dialog.open=false;dialog.showModal=()=>{dialog.open=true;};dialog.close=()=>{dialog.open=false;};
  dialog.querySelector=selector=>selector==="form"?form:dialog.summary;
  form.prepend=summary=>{dialog.summary=summary;};form.querySelectorAll=()=>[submit];
  elements.set("revive-session-dialog",dialog);elements.set("revive-session-form",form);
  const target={targetIdentityVersion:2,targetId:"archived-target",slug:"example",archived:true,
    lifecycle:"archived",location:"archive",threadId:"thread-1"};
  const operation={kind:"archive",state:"complete",phase:"complete",receiptId:"archive-receipt",
    options:{journalExpected:true,journalId:"a".repeat(64)},currentTarget:target};
  const context={...h.context,createLifecycleTargetState,lifecyclePresentation,
    body:{dataset:{pendingLifecycle,lifecycleTargetId:target.targetId,lifecycle:target.lifecycle,
      archived:"true",rootThreadId:target.threadId}},
    document:{...h.context.document,getElementById:id=>elements.get(id)||null,createElement:node},
    location:{reload(){navigations.push("reload");dialog.close();},assign:path=>navigations.push(path)},
    client:{...h.context.client,operation:async()=>operation,revive:async(...args)=>{revivals.push(args);}},
    clearInterval(){}};
  const section=(start,end)=>source.slice(source.indexOf(start),source.indexOf(end,source.indexOf(start)));
  const script=section("  let lifecycleTargetId =","  let interactive =")+
    section("  const lifecycleStatus =","  const deleteDialog =")+
    section("  const countSnapshots =","  const suspendPageReads")+
    section("  const reviveDialog =","  const reviveRetry =");
  const api=vm.runInNewContext(script+"\n({open:openLifecycleConfirmation,refresh:refreshSessionDetails,show:showLifecycle,pending:()=>pendingLifecycle,last:()=>lastLifecycleOperation})",context);
  return {...h,api,context,dialog,form,submit,target,operation,navigations,revivals,
    title:elements.get("lifecycle-operation-title")};
}

test("completed archive history cannot reload away a fresh revival confirmation",async(t)=>{
  const h=lifecycleDetails();t.after(()=>h.context.pageReads.pause());
  await h.api.open("revive");assert(h.dialog.open);
  const reading=h.api.refresh();
  h.reads[0].resolve({currentTarget:h.target,pendingLifecycle:"",repositoriesHTML:""});await reading;
  assert.deepEqual(h.navigations,[],"historical completion must not create an archive reload intent");
  assert.equal(h.api.pending(),"");assert(h.dialog.open);
  assert.equal(h.title.textContent,"Archive complete","history remains displayed");
  assert.equal(h.api.last().options.journalExpected,true,"receipt recovery evidence remains unchanged");
  await h.form.listeners.submit({preventDefault(){},submitter:h.submit});
  assert.deepEqual(h.revivals,[[false,h.target.targetId,""]]);
});

test("a genuinely pending archive still reloads after its details confirm completion",async(t)=>{
  const h=lifecycleDetails("archive");t.after(()=>h.context.pageReads.pause());
  h.api.show(h.operation);assert.equal(h.api.pending(),"archive");
  const reading=h.api.refresh();
  h.reads[0].resolve({currentTarget:h.target,pendingLifecycle:"",repositoriesHTML:""});await reading;
  assert.deepEqual(h.navigations,["reload"]);assert.equal(h.api.pending(),"");
});

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

test("activity deadline retires an ignored abort and a current success recovers", async (t) => {
  t.mock.timers.enable({apis:["setTimeout"]});
  const h=lane("activity"), begin=h.context.pageReads.begin;
  h.context.pageReads.begin=()=>begin(5);
  const old=h.api.refresh(); t.mock.timers.tick(5); await old;
  assert(h.reads[0].signal.aborted);
  assert.equal(h.context.activityReadFailed,true);
  const fresh=h.api.refresh(true); await tick();
  assert.equal(h.reads.length,2);
  h.reads[0].resolve({obsolete:true}); await tick();
  assert.equal(h.context.activitySnapshot,null);
  h.reads[1].resolve({threadId:"thread-1",current:true}); await fresh;
  assert.equal(h.context.activityReadFailed,false);
  assert.equal(h.context.activitySnapshot.current,true);
  t.mock.timers.tick(5);
  assert.equal(h.reads[1].signal.aborted,false,"successful read must clear its deadline");
  h.context.pageReads.pause();
});

test("Pi activity ignores success and error from an old conversation scope", async () => {
  for (const failed of [false,true]) {
    const h=lane("activity");h.context.piBackend=true;h.context.piPageScope.current=()=>true;
    const old=h.api.refresh();h.context.piPageScope.epoch++;
    if(failed)h.reads[0].reject(Error("old scope"));else h.reads[0].resolve({obsolete:true});
    await old;
    assert.equal(h.context.activitySnapshot,null);assert.equal(h.context.activityReadFailed,false);
    const fresh=h.api.refresh(true);assert.equal(h.reads.length,2);
    h.reads[1].resolve({threadId:"thread-1",current:true});await fresh;
    assert.equal(h.context.activitySnapshot.current,true);h.context.pageReads.pause();
  }
});

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

test("activity display reconciles independent feeds without readiness authority", () => {
const {projectActivityDisplay}=require("./static/app.js");
// Independent feed order cannot select a winner or change execution readiness.
const displayActivity = {threadId:"display-thread",currentTurnId:"turn-one",currentState:"working",latestTurnStatus:"inProgress"};
const displayTranscript = {threadId:"display-thread",latestTurnId:"turn-one",status:"active"};
const displayInput = {threadId:"display-thread",transcript:displayTranscript,activity:displayActivity,current:true,timing:{stale:false,elapsed:0},requestMs:12};
assert.equal(projectActivityDisplay(displayInput).available,true);
const completedDisplay = {...displayActivity,currentState:"idle",latestTurnStatus:"completed",completedAtMs:80,observedAtMs:100};
const conflict = projectActivityDisplay({...displayInput,activity:completedDisplay});
assert.equal(conflict.available,false); assert.deepEqual(conflict.completion,["display-thread","turn-one",80]);
assert.equal(projectActivityDisplay({...displayInput,transcript:{...displayTranscript,status:"idle"}}).available,false);
assert.equal(projectActivityDisplay({...displayInput,transcript:{...displayTranscript,status:"idle"},activity:completedDisplay}).available,true);
for (const change of [{activityFailed:true},{transcriptFailed:true},{current:false},{requestMs:16000},{timing:{stale:true,elapsed:0}},{timing:{stale:false,elapsed:14999}},
 {activity:{...displayActivity,threadId:"foreign"}},{activity:{...displayActivity,currentTurnId:"other-turn"}},{activity:{...displayActivity,stale:true}},
 {activity:{...displayActivity,latestTurnStatus:"completed"}},{activity:{...completedDisplay,latestTurnStatus:"inProgress"}}]) {
 assert.equal(projectActivityDisplay({...displayInput,...change}).available,false,JSON.stringify(change));
}
for (const [transcript,activity] of [[{...displayTranscript,latestTurnId:""},{...displayActivity,currentTurnId:""}],
 [{...displayTranscript,status:"idle",latestTurnId:""},{...completedDisplay,currentTurnId:"",latestTurnStatus:""}],
 [{...displayTranscript},{...displayActivity,currentState:"waiting"}], [{...displayTranscript},{...displayActivity,currentState:"unclassified"}]]) {
 assert.equal(projectActivityDisplay({...displayInput,transcript,activity}).available,true);
}
for (const activity of [{...completedDisplay,currentTurnId:""},{...completedDisplay,currentTurnId:"other"}, {...completedDisplay,completedAtMs:101}, {...completedDisplay,completedAtMs:0}]) {
 assert.equal(projectActivityDisplay({...displayInput,activity}).completion,null);
}

});

test("foreign activity cannot replace the selected thread's historical totals", async () => {
 const h=lane("activity");const original={threadId:"thread-1",workingMs:123};h.context.activitySnapshot=original;
 const reading=h.api.refresh(true);h.reads[0].resolve({threadId:"foreign",workingMs:999});await reading;
 assert.equal(h.context.activitySnapshot,original);assert.equal(h.context.activityReadFailed,true);h.context.pageReads.pause();
});

// Mount the actual work-line renderer, clock and activity lane together. The
// deferred transport ignores abort so retirement and late settlements stay real.
function activityPage({pi=false}={}) {
 const helpers=require("./static/app.js"), reads=[], transcriptReads=[], timers=new Map(), elements=new Map();let time=100000,timerID=0,syncOptions;
 const element=()=>({textContent:"",title:"",hidden:false,attributes:{},classes:new Map(),
  setAttribute(k,v){this.attributes[k]=v;},classList:{toggle(k,v){this.owner.classes.set(k,v);}}});
 for(const id of ["codex-work","codex-work-label","codex-work-counts","codex-work-elapsed","codex-duration","session-tab-codex","codex-waiting-indicator"]){
  const e=element();e.classList.owner=e;elements.set(id,e);
 }
 const context={...helpers,Date:class extends Date{static now(){return time;}},
  createTimingClock:()=>helpers.createTimingClock(()=>time),pageReads:helpers.createReadScope(),
  document:{hidden:false,getElementById:id=>elements.get(id),querySelectorAll:()=>[]},
  setTimeout:(fn,delay)=>{timers.set(++timerID,{fn,delay});return timerID;},clearTimeout:id=>timers.delete(id),
  setInterval:()=>0,addEventListener(){},body:{dataset:{sessionBackend:pi?"pi":"codex"}},
  conversationID:"conversation-a",selectedMember:"lead",currentThreadId:"thread-1",
  piPageScope:{scope:"scope-a",epoch:0,current:()=>true,eventsPath:path=>path},codexBackend:!pi,piBackend:pi,
  interactive:true,executionHeld:false,recoveryState:"",currentPrompts:[],answeredOffers:new Set(),
  teamStatsPayload:null,teamStatsFailed:false,teamStatsReceivedAt:0,
  latestThreadPayload:{threadId:"thread-1",latestTurnId:"turn-1",status:"active"},
  sync:{scheduleRefresh(){},destroy(){}},refreshPolicy:{activityIdleMs:5000,activityActiveMs:1000,activityIdleFreshMs:15000,activityActiveFreshMs:15000},
  client:{activity:options=>{const read=deferred();reads.push({...read,signal:options.signal});return read.promise;},
   thread:options=>{const read=deferred();transcriptReads.push({...read,signal:options.signal});return read.promise;},eventsPath:()=>"/events"},
  conversationAssets:{createConversationSync:options=>{syncOptions=options;return context.sync;},renderConnectionStatus(){}},
  pagingHelpersAvailable:false,settingsWriteGeneration:0,piDispatchRevision:0,lastSyncStatus:"",dispatchReadReady:false,
  threadActive:true,messageDispatchInFlight:false,piWorkflows:null,messageDispatcher:null,
  refreshPending(){},refreshQueue(){},scheduleHistoryRepair(){},updateMessageActions(){},reconcilePiPendingMessages(){}};
 const start=source.indexOf("  const codexWork ="),end=source.indexOf("  const loadMessageReceipts =",start);assert(start>0&&end>start);
 const api=vm.runInNewContext(source.slice(start,end)+`\n({refresh:refreshActivity,update:updateCodexWork,
  hide(){pageReads.pause();pauseTiming();},resume(){pageReads.resume();resumeTiming();},
  transcript(payload,completed=true){latestThreadPayload=payload;transcriptDisplayBinding=activityDisplayBinding();if(completed)transcriptReadFailed=false;updateCodexWork(completed);},
  state:()=>({read:activityRead,display:currentActivityDisplay()})})`,context);
 api.transcript(context.latestThreadPayload);
 const snapshot=(extra={})=>({threadId:"thread-1",currentTurnId:"turn-1",currentState:"working",latestTurnStatus:"inProgress",
  observedAtMs:time,stateSinceMs:90000,startedAtMs:90000,workingMs:3000,waitingMs:1000,idleMs:2000,messages:4,toolCalls:2,...extra});
 context.renderThread=(payload,isCurrent)=>{if(isCurrent())api.transcript(payload,false);};
 return {api,context,reads,transcriptReads,elements,snapshot,setTime:value=>{time=value;},
  mountSync(){const start=source.indexOf("  sync = conversationAssets.createConversationSync({",source.indexOf("  const restartConversationSync ="));const end=source.indexOf("\n  void refreshPending();",start);assert(start>0&&end>start);vm.runInNewContext(source.slice(start,end),context);return syncOptions;},
  visible:()=>Object.fromEntries(["codex-work-label","codex-work-counts","codex-work-elapsed","codex-duration"].map(id=>[id,elements.get(id).textContent]))};
}

test("held resume preserves the last successful work line and freezes elapsed values",async(t)=>{
 const h=activityPage();t.after(()=>h.context.pageReads.pause());
 let reading=h.api.refresh(true);h.reads[0].resolve(h.snapshot());await reading;
 h.setTime(102000);h.api.update();const before=h.visible();assert.equal(before["codex-work-label"],"Working");assert.equal(before["codex-work-elapsed"],"12s this turn");
 h.api.hide();assert.deepEqual(h.visible(),before,"hiding must retain the last rendered successful values");
 h.setTime(202000);h.api.resume();reading=h.api.state().read.promise;assert.equal(h.reads.length,2);
 assert.deepEqual(h.visible(),before,"a pending resumed read must retain the same work line");
 assert.equal(h.api.state().display.available,false,"retained presentation cannot restore fresh evidence");
 h.setTime(207000);h.api.update();assert.deepEqual(h.visible(),before,"waiting time cannot extend presumed work");
 h.reads[1].resolve(h.snapshot({workingMs:9000,messages:7}));await reading;
 assert.equal(h.elements.get("codex-work-label").textContent,"Working");assert.match(h.elements.get("codex-work-counts").textContent,/7 messages/);
 assert.notEqual(h.elements.get("codex-duration").textContent,before["codex-duration"],"a current success replaces the retained values");
});

test("first activity read loads without invented values and a failed retry stays unavailable",async(t)=>{
 const h=activityPage();t.after(()=>h.context.pageReads.pause());
 let reading=h.api.refresh(true);assert.equal(h.elements.get("codex-work-label").textContent,"Loading activity…");assert.equal(h.elements.get("codex-work-counts").textContent,"");assert.equal(h.elements.get("codex-work").attributes["aria-busy"],"true");
 h.reads[0].reject(Error("fixture 503"));await reading;assert.equal(h.elements.get("codex-work-label").textContent,"Activity unavailable");
 reading=h.api.refresh(true);h.api.update();assert.equal(h.elements.get("codex-work-label").textContent,"Activity unavailable","retry cannot hide a completed failure");
 h.reads[1].resolve(h.snapshot());await reading;assert.equal(h.elements.get("codex-work-label").textContent,"Working");
});

test("same-target pending refresh freezes idle values for both adapters without changing readiness",async(t)=>{
 for(const pi of [false,true]){
  const h=activityPage({pi});t.after(()=>h.context.pageReads.pause());h.api.transcript({threadId:"thread-1",latestTurnId:"turn-1",status:"idle"});
  let reading=h.api.refresh(true);h.reads[0].resolve(h.snapshot({currentState:"idle",latestTurnStatus:"completed",completedAtMs:99000}));await reading;
  h.setTime(102000);h.api.update();const before=h.visible();h.context.dispatchReadReady=true;reading=h.api.refresh(true);
  assert.deepEqual(h.visible(),before);assert.equal(h.elements.get("codex-work-label").title,"Refreshing activity…");
  h.setTime(107000);h.api.update();assert.deepEqual(h.visible(),before);assert.equal(h.context.dispatchReadReady,true);
  assert.equal(h.api.refresh(true),reading,"same target retains single flight");assert.equal(h.reads.length,2);
  h.reads[1].resolve(h.snapshot({currentState:"idle",latestTurnStatus:"completed",completedAtMs:99000,idleMs:9000}));await reading;
  assert.equal(h.elements.get("codex-work-label").textContent,"Idle");assert.notDeepEqual(h.visible(),before);
 }
});

test("fresh transcript during held resume does not discard retained activity, and transcript failure stays visible on retry",async(t)=>{
 const h=activityPage();t.after(()=>h.context.pageReads.pause());let reading=h.api.refresh(true);h.reads[0].resolve(h.snapshot());await reading;
 const options=h.mountSync(),before=h.visible();h.api.hide();h.setTime(200000);h.api.resume();const activity=h.api.state().read.promise;
 let signal=new AbortController().signal,transcript=options.read(signal);h.transcriptReads[0].resolve({...h.context.latestThreadPayload});const page=await transcript;options.apply(page,{isCurrent:()=>true});
 assert.deepEqual(h.visible(),before,"a coherent transcript cannot make the paused activity clock an error");
 h.reads[1].resolve(h.snapshot({workingMs:8000}));await activity;assert.equal(h.elements.get("codex-work-label").textContent,"Working");
 transcript=options.read(new AbortController().signal);h.transcriptReads[1].reject(Error("fixture transcript 503"));await assert.rejects(transcript);
 options.onStateChange({status:"reconnecting",error:"fixture transcript 503",httpStatus:503});assert.equal(h.elements.get("codex-work-label").textContent,"Activity unavailable");assert.equal(h.context.dispatchReadReady,false);
 transcript=options.read(new AbortController().signal);assert.equal(h.elements.get("codex-work-label").textContent,"Activity unavailable");assert.equal(h.context.dispatchReadReady,false);
 h.transcriptReads[2].resolve({...h.context.latestThreadPayload});options.apply(await transcript,{isCurrent:()=>true});assert.equal(h.elements.get("codex-work-label").textContent,"Working");assert.equal(h.context.dispatchReadReady,true);
});

test("completed mismatches cannot be hidden by another pending feed",async(t)=>{
 const h=activityPage();t.after(()=>h.context.pageReads.pause());let reading=h.api.refresh(true);h.reads[0].resolve(h.snapshot());await reading;
 const options=h.mountSync(),transcript=options.read(new AbortController().signal);reading=h.api.refresh(true);
 h.reads[1].resolve(h.snapshot({currentTurnId:"foreign-turn"}));await reading;
 assert.equal(h.elements.get("codex-work-label").textContent,"Activity unavailable");
 h.transcriptReads[0].resolve({...h.context.latestThreadPayload});options.apply(await transcript,{isCurrent:()=>true});
 assert.equal(h.elements.get("codex-work-label").textContent,"Activity unavailable");
 reading=h.api.refresh(true);h.reads[2].resolve(h.snapshot());await reading;assert.equal(h.elements.get("codex-work-label").textContent,"Working");
});

test("member, conversation, thread and scope changes clear retained display and fence late activity settlements",async(t)=>{
 for(const field of ["selectedMember","conversationID","currentThreadId","scope"]){
  const h=activityPage();t.after(()=>h.context.pageReads.pause());let reading=h.api.refresh(true);h.reads[0].resolve(h.snapshot());await reading;
  const old=h.api.refresh(true);if(field==="scope")h.context.piPageScope.scope="scope-b";else h.context[field]="replacement";
  h.api.update();assert.equal(h.elements.get("codex-work-label").textContent,"Activity unavailable");assert.equal(h.elements.get("codex-duration").textContent,"Timing unavailable");
  reading=h.api.refresh(true);const current=h.api.state().read;h.reads[1].resolve(h.snapshot({messages:999}));await old;
  assert.equal(h.api.state().read,current,"old finally must not release the successor slot");assert.doesNotMatch(h.elements.get("codex-work-counts").textContent,/999/);
  h.reads[2].resolve(h.snapshot({threadId:h.context.currentThreadId}));await reading;
 }
});

test("retired activity failure cannot replace a resumed success and invalid Pi scope clears presentation",async(t)=>{
 const h=activityPage({pi:true});t.after(()=>h.context.pageReads.pause());let reading=h.api.refresh(true);h.reads[0].resolve(h.snapshot());await reading;
 const old=h.api.refresh(true);h.api.hide();h.api.resume();reading=h.api.state().read.promise;h.reads[2].resolve(h.snapshot({messages:8}));await reading;
 const before=h.visible();h.reads[1].reject(Error("obsolete failure"));await old;assert.deepEqual(h.visible(),before);
 h.context.piPageScope.current=()=>false;h.api.update();assert.equal(h.elements.get("codex-work-label").textContent,"Activity unavailable");assert.equal(h.elements.get("codex-duration").textContent,"Timing unavailable");
});

test("mounted work line keeps idle and classified-wait duration coherent beside team status", () => {
 const {activityPresentation,activityStatusPresentation}=require("./static/app.js");
 const begin=source.indexOf("  const updateCodexWork ="),end=source.indexOf("  updateTeamWork =",begin);
 for(const [state, peers, origin] of [["idle",true,true],["waiting",true,true],["idle",false,true],["idle",false,false]]) {
  const attrs=()=>({textContent:"",title:"",setAttribute(k,v){this[k]=v;}});
  const classes=new Map(), label=attrs(), elapsed=attrs(),counts=attrs(),summary=attrs();
  const snapshot={threadId:"thread-1",currentState:state,latestTurnStatus:state==="idle"?"completed":"inProgress",observedAtMs:2000,stateSinceMs:origin?1000:0,openWaitingMs:1000,workingMs:3000,waitingMs:1000,idleMs:1000};
  const display={available:true,active:state!=="idle",snapshot,completion:null};let tabDisplay;
  const context={codexWork:{classList:{toggle:(k,v)=>classes.set(k,v)},setAttribute(){}},codexWorkElapsed:elapsed,codexWorkLabel:label,codexWorkCounts:counts,durationSummary:summary,
   timingClock:{view:()=>({elapsed:0})},activityFreshnessBudget:()=>15000,currentActivityDisplay:()=>display,
   otherTeamWork:()=>({present:peers,label:"peer working",text:"peer is working"}),activityBackendLabel:"Codex",activityStatusPresentation,activityPresentation,
   activitySnapshot:snapshot,interactive:true,updateCodexWaitingIndicator:value=>{tabDisplay=value;},
   activityDisplayBinding:()=>"fixture",activityReadBinding:"fixture",piBackend:false,pageReads:{paused:false},activityRead:null,transcriptActivityRead:null,
   activityReadFailed:false,transcriptReadFailed:false,activityPresentationState:require("./static/app.js").createActivityPresentation()};
  vm.runInNewContext(source.slice(begin,end)+";updateCodexWork();",context);
  if(origin) assert(elapsed.textContent.endsWith(" "+state));assert.equal(elapsed.title,elapsed.textContent);assert.equal(elapsed["aria-label"],elapsed.textContent);
  assert.equal(classes.get("waiting"),state==="waiting");assert.equal(tabDisplay,display);assert.equal(context.codexWork.hidden,!peers&&!origin);
  // This visibility condition is the unchanged pre-D6 rule for idle evidence.
  const oldVisible=display.active || Boolean(origin) || peers;assert.equal(context.codexWork.hidden,!oldVisible);
 }
});

test("all representative team states agree with matching transcript/activity evidence", () => {
 const {projectActivityDisplay,activityStatusPresentation}=require("./static/app.js");
 const team={present:true,label:"peer working",text:"peer is working"};
 for(const [state,reason,expected] of [["idle","","peer working"],["working","","Working"],["unclassified","","In progress"],
  ["waiting","sleep","Sleeping (auto) · peer working"],["waiting","subagents","Waiting for team (auto) · peer working"],
  ["waiting","approval","Approval needed · peer working"],["waiting","userInput","Answer needed · peer working"]]) {
  for(const ids of [true,false]) {
   const activity={threadId:"thread-1",currentTurnId:ids?"turn-1":"",currentState:state,waitReason:reason,latestTurnStatus:state==="idle"?"completed":"inProgress"};
   const transcript={threadId:"thread-1",latestTurnId:ids?"turn-1":"",status:state==="idle"?"idle":"active"};
   const input={threadId:"thread-1",transcript,activity,current:true,timing:{stale:false,elapsed:0},requestMs:10};
   const view=projectActivityDisplay(input);assert.equal(view.available,true,state);assert.equal(activityStatusPresentation(view.snapshot,view.active,team).label,expected,state);
   if(state==="idle") assert.equal(activityStatusPresentation(view.snapshot,view.active,{...team,stale:true}).label,"Team status unavailable");
   else {const conflict=projectActivityDisplay({...input,transcript:{...transcript,status:"idle"}});assert.equal(conflict.available,false,state);}
  }
 }
});
