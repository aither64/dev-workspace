"use strict";
const assert = require("node:assert/strict");
const {test} = require("node:test");
const {pathToFileURL} = require("node:url");
const {readFileSync} = require("node:fs");
const vm = require("node:vm");
const {randomUUID} = require("node:crypto");
const moduleURL = pathToFileURL(__dirname + "/static/codex-providers.js");
const catalog = {active:"openai",providers:[{id:"openai",label:"Subscription"},{id:"pool",label:"Pool"}]};
const tick = () => new Promise(resolve => setImmediate(resolve));

test("20 one-second ticks coalesce catalog reads; pause retires old success/error/finally", async () => {
  const {createProviderPreview}=await import(moduleURL);
  for(const failOld of [false,true]) {
    const reads=[];const preview=createProviderPreview({onChange:()=>{},request:(url,options)=>new Promise((resolve,reject)=>reads.push({url,signal:options.signal,resolve,reject}))});
    const old=preview.refresh();for(let i=0;i<20;i++)void preview.refresh();assert.equal(reads.length,1);
    preview.pause();assert(reads[0].signal.aborted);await preview.refresh();assert.equal(reads.length,1);
    const fresh=preview.resume();assert.equal(reads.length,2);
    if(failOld)reads[0].reject(Error("old"));else reads[0].resolve({...catalog,active:"pool"});await old;
    assert.equal(preview.refresh(),fresh);assert.equal(preview.state().catalog,null);
    reads[1].resolve(catalog);await tick();assert.equal(reads.length,3);
    reads[2].resolve({provider:{id:"openai"},windows:[]});await fresh;
    assert.equal(preview.state().catalog.active,"openai");assert.equal(preview.state().failed,false);preview.stop();
  }
});

test("preview selection makes only reads and ignores an earlier provider response", async () => {
  const {createProviderPreview} = await import(moduleURL);
  const reads = new Map(), calls = [];
  const preview = createProviderPreview({request: url => {
    calls.push(url);
    if (url === "/api/codex-providers") return Promise.resolve(catalog);
    return new Promise(resolve => reads.set(url, resolve));
  },onChange:()=>{}});
  const initial = preview.refresh(); await tick();
  preview.select("pool"); await tick();
  reads.get("/api/codex-limits?provider=pool")({provider:{id:"pool"},windows:[]}); await tick();
  reads.get("/api/codex-limits?provider=openai")({provider:{id:"openai"},windows:[]}); await initial;
  assert.equal(preview.state().selected,"pool");
  assert.equal(preview.state().snapshot.provider.id,"pool");
  assert.equal(preview.state().catalog.active,"openai");
  assert(calls.every(url => !url.includes("switch") && !url.includes("cancel")));
});

test("preview coalesces reads and retains a stale snapshot on failure", async () => {
  const {createProviderPreview} = await import(moduleURL);
  let time = 0, count = 0, fail = false;
  const preview = createProviderPreview({now:()=>time,request: async url => {
    if (url === "/api/codex-providers") return catalog;
    count++; await tick(); if (fail) throw new Error("Unavailable");
    return {provider:{id:"openai"},windows:[],updatedAt:"2026-01-01T00:00:00Z"};
  },onChange:()=>{}});
  await preview.refresh(); assert.equal(count,1);
  await Promise.all([preview.refreshPreview(),preview.refreshPreview()]); assert.equal(count,1);
  time=31000;fail=true;
  await Promise.all([preview.refreshPreview(),preview.refreshPreview()]); assert.equal(count,2);
  assert.equal(preview.state().failed,true);assert.equal(preview.state().snapshot.provider.id,"openai");
});

test("session scope changes ignore an outstanding catalog for the old scope", async () => {
  const {createProviderPreview}=await import(moduleURL);
  let release;
  const preview=createProviderPreview({session:"one",onChange:()=>{},request:async url=>{
    if(url==="/api/codex-providers?session=one") return new Promise(resolve=>{release=resolve;});
    if(url==="/api/codex-providers?session=two") return {...catalog,session:"two",active:"pool"};
    return {provider:{id:new URL(url,"http://fixture").searchParams.get("provider")},windows:[]};
  }});
  const old=preview.refresh();await tick();await preview.setSession("two");
  release({...catalog,session:"one"});await old;
  assert.equal(preview.state().catalog.session,"two");assert.equal(preview.state().selected,"pool");
});

test("operation status distinguishes waiting, applying and recovery", async () => {
  const {providerOperationText} = await import(moduleURL);
  assert.match(providerOperationText({phase:"waiting",target:"pool",blockers:["Session creation."]},catalog.providers),/switching to Pool.*Session creation/);
  assert.match(providerOperationText({phase:"applying",target:"pool",session:"one",stage:"closing",completed:1,total:2},catalog.providers),/Session one.*Closing threads \(1\/2\)/);
  assert.equal(providerOperationText({phase:"recovery-required",target:"pool",message:"Recover this workspace."},catalog.providers),"Recover this workspace.");
  assert.equal(providerOperationText({phase:"failed",target:"pool",message:"Target unavailable."},catalog.providers),"Target unavailable.");
  assert.equal(providerOperationText({phase:"applied",target:"pool",message:"Some terminal clients need dev-session sync."},catalog.providers),"Some terminal clients need dev-session sync.");
});

test("cancellation feedback requires the exact local operation identity", async () => {
  const {providerOperationText} = await import(moduleURL);
  const canceled = {id:"old-operation",target:"pool",phase:"canceled"};
  assert.equal(providerOperationText(canceled,catalog.providers),"");
  assert.equal(providerOperationText(canceled,catalog.providers,"other-operation"),"");
  assert.equal(providerOperationText(canceled,catalog.providers,"old-operation"),"Provider switch canceled.");
});

// Execute the shipped provider event wiring with synthetic DOM controls and API
// responses; these tests never submit a switch to a running workspace.
async function popup(backend) {
  const helpers = await import(moduleURL);
  const control = () => {
    const listeners = new Map();
    return {options:[],children:[],value:"",ownText:"",hidden:false,disabled:false,dataset:{},
      get textContent(){return this.ownText+this.children.map(child=>child.textContent).join("");},
      set textContent(value){this.ownText=value;this.children=[];},
      append(...children){this.children.push(...children);},setAttribute(){},querySelector(){return {disabled:false};},
      addEventListener(type,fn) {listeners.set(type,fn);},
      emit(type) {listeners.get(type)?.();},
      replaceChildren(...options) {this.ownText="";this.options=options;this.children=options;}};
  };
  const controls = new Map(["controls","select","scope","switch","inherit","cancel","recover","dismiss","action-error","status","active"].map(name => [name,control()]));
  const dialog = control(); dialog.open=true;
  dialog.querySelector = selector => controls.get(selector.match(/\[data-provider-(.*)\]/)[1]);
  dialog.close = () => {dialog.open=false;dialog.emit("close");};
  let poll;
  const context = {dialog,...helpers,slug:"",providers:null,providerBusy:false,providerAttempt:null,observedActiveProvider:"",
    limitsSnapshot:null,limitsLeaving:false,limits:{invalidate(){},refresh:async()=>{}},renderAccountDetails(){},
    crypto:{randomUUID},document:{addEventListener(){},hidden:false,createElement:control,dispatchEvent(){}},CustomEvent:class {},
    setInterval:fn => {poll=fn;return 1;},clearInterval(){},addEventListener(){},
    request:async (url,options) => {
      if (url === "/api/codex-providers") return {...catalog,operation:backend.operation ? {...backend.operation} : null};
      if (url.startsWith("/api/codex-limits?")) return {provider:{id:new URL(url,"http://fixture").searchParams.get("provider")},windows:[]};
      const body=JSON.parse(options.body);
      if (url.endsWith("/switch")) backend.operation={id:body.idempotencyKey,target:body.providerId,phase:"waiting"};
      else if(url.endsWith("/dismiss")) backend.operation={...backend.operation,dismissed:true};
      else {
        assert.equal(url,"/api/codex-providers/cancel");
        assert.equal(body.operationId,backend.operation.id);
        if (backend.beforeCancel) await backend.beforeCancel();
        backend.operation={...backend.operation,phase:"canceled"};
      }
      return {...catalog,operation:backend.operation};
    }};
  const source=readFileSync(__dirname+"/static/app.js","utf8");
  const start=source.indexOf('    const providerControls = dialog.querySelector("[data-provider-controls]");');
  const end=source.indexOf('    globalThis.addEventListener("storage"',start);
  assert(start>=0 && end>start);
  const api=vm.runInNewContext(source.slice(start,end)+'\n({refresh:()=>providers.refresh()})',context);
  await tick();
  return {dialog,...Object.fromEntries(controls),refresh:api.refresh,poll:async()=>{poll();await tick();},
    selectProvider:async id=>{controls.get("select").value=id;controls.get("select").emit("change");await tick();}};
}

test("historical and other-tab cancellations stay hidden on load and polling", async () => {
  const backend={operation:{id:"verification",target:"pool",phase:"canceled"}};
  const first=await popup(backend);
  assert.equal(first.status.textContent,"");
  await first.poll();assert.equal(first.status.textContent,"");
  backend.operation={id:"other-tab",target:"pool",phase:"waiting"};
  await first.refresh();assert.match(first.status.textContent,/Switching to/);
  const second=await popup(backend);
  second.cancel.emit("click");await tick();
  assert.match(second.status.textContent,/Provider switch canceled\./);
  await first.poll();assert.equal(first.status.textContent,"");
  const reloaded=await popup(backend);
  assert.equal(reloaded.status.textContent,"");
});

test("accepted switch retains local feedback until close or preview change", async () => {
  const backend={operation:null};
  const ui=await popup(backend);
  await ui.selectProvider("pool");
  ui.switch.emit("click");await tick();
  assert.match(ui.status.textContent,/Switching to/);
  backend.operation={...backend.operation,phase:"canceled"};
  await ui.poll();assert.match(ui.status.textContent,/Provider switch canceled\./);
  ui.dialog.close();assert.equal(ui.status.textContent,"");
  ui.dialog.open=true;
  await ui.poll();assert.equal(ui.status.textContent,"");
  backend.operation={id:"next",target:"pool",phase:"waiting"};
  await ui.refresh();ui.cancel.emit("click");await tick();
  assert.match(ui.status.textContent,/Provider switch canceled\./);
  await ui.selectProvider("openai");assert.equal(ui.status.textContent,"");
  await ui.poll();assert.equal(ui.status.textContent,"");
});

test("closing a popup during cancellation prevents late feedback", async () => {
  let release;
  const backend={operation:{id:"waiting",target:"pool",phase:"waiting"},
    beforeCancel:()=>new Promise(resolve=>{release=resolve;})};
  const ui=await popup(backend);
  ui.cancel.emit("click");await tick();
  ui.dialog.close();release();await tick();
  ui.dialog.open=true;
  await ui.poll();assert.equal(ui.status.textContent,"");
  assert.equal(backend.operation.phase,"canceled");
});

test("sidebar follows its session after a default switch while the dialog previews workspace scope", async () => {
  const {createCodexLimitsReader} = require("./static/app.js");
  const calls = [], events = [];
  const context = {createCodexLimitsReader,slug:"page-session",observedActiveProvider:"openai",
    limits:null,limitsSnapshot:null,notice:{failure(){},success(){},begin(){}},
    refreshPolicy:{limitsMs:0,limitsDeadlineMs:1000,retryInitialMs:0},
    document:{addEventListener(){},hidden:false,dispatchEvent:event=>events.push(event.type)},CustomEvent:class {constructor(type){this.type=type;}},
    // The preview is deliberately a different scope and must not supply the
    // page-session identity when its effective provider changes.
    providers:{refresh:()=>{throw new Error("workspace preview was used for the page session");}},
    request:async url=>{calls.push(url);return url.includes("codex-providers") ? {active:"pool",session:"page-session"} : {provider:{id:"pool"},windows:[]};}};
  const source=readFileSync(__dirname+"/static/app.js","utf8");
  const start=source.indexOf("    limits = createCodexLimitsReader(request,");
  const end=source.indexOf('    const {createResetCreditAction}',start);
  assert(start>=0 && end>start);
  const api=vm.runInNewContext(source.slice(start,end)+'\n({refresh:()=>limits.refresh(true)})',context);
  await api.refresh();await tick();
  assert.equal(context.observedActiveProvider,"pool");
  assert.equal(context.limitsSnapshot.provider.id,"pool");
  assert(calls.includes("/api/codex-providers?session=page-session"));
  assert(calls.filter(url=>url.includes("codex-limits")).every(url=>url==="/api/codex-limits?session=page-session"));
  assert(events.includes("workspace-provider-changed"));
});

test("plan and fork creation load destination catalogs independently of the source session", async () => {
  const makeSelect = form => ({value:"",required:true,dataset:{},options:[],closest:()=>form,
    replaceChildren(){this.options=[];this.value="";},append(option){this.options.push(option);if(!this.value)this.value=option.value;}});
  const plan={id:"plan-session-form",dataset:{},elements:{provider:{value:"openai"}}};
  const source={id:"conversation",dataset:{},elements:{}};
  const fork={id:"fork-form",dataset:{},elements:{}};
  const planModel=makeSelect(plan),sourceModel=makeSelect(source),forkModel=makeSelect(fork),calls=[];
  const context={creationProviderValue:form=>form?.elements.provider?.dataset?.providerIntent??form?.elements.provider?.value??"",slug:"source",models:[],currentModel:"source-model",modelCatalogs:new Map(),selectModelCatalogs:new WeakMap(),selectModelURLs:new WeakMap(),
    document:{addEventListener(){},querySelectorAll:()=>[sourceModel,planModel,forkModel],createElement:()=>({})},
    request:async url=>{calls.push(url);return [{model:url.includes("provider=openai")?"daybreak":url.includes("session=source")?"source-model":"default-model",displayName:"Fixture",isDefault:true}];},
    selectCreationSetting:(select,value)=>{select.value=value;},populateEfforts(){},pairedEffortSelect(){},applyCurrentSettings(){},applyMemberDefaults(){},restoreManagedCreationDrafts(){}};
  const text=readFileSync(__dirname+"/static/app.js","utf8");
  const start=text.indexOf("  const loadModels = async "),end=text.indexOf("\n  const bindModelSelects",start);
  assert(start>=0&&end>start);
  const api=vm.runInNewContext(text.slice(start,end)+'\n({loadModels})',context);
  await api.loadModels();
  assert.equal(planModel.options[0].value,"daybreak");
  assert.equal(sourceModel.options[0].value,"source-model");
  assert.equal(forkModel.options[0].value,"default-model");
  fork.elements.provider={value:"openai",dataset:{providerIntent:"openai"}};
  await api.loadModels({querySelectorAll:()=>[forkModel]});
  assert.equal(forkModel.options[0].value,"daybreak");
  assert.equal(context.models[0].model,"source-model");
  assert(calls.includes("/api/models?provider=openai"));
  plan.elements.provider.value="pool";
  plan.elements.provider.dataset={providerIntent:""};
  planModel.value="";
  await api.loadModels({querySelectorAll:()=>[planModel]});
  assert.equal(planModel.options[0].value,"default-model");
  assert.equal(sourceModel.options[0].value,"source-model");
});


test("restoring a plan provider reloads its catalog and discards the outstanding default", async () => {
  const model={value:"",required:true,dataset:{},options:[],replaceChildren(){this.options=[];this.value="";},append(option){this.options.push(option);if(!this.value)this.value=option.value;}};
  const label={hidden:true};
  const form={id:"plan-session-form",dataset:{},elements:{provider:{value:"",dataset:{}},model},querySelectorAll:()=>[model],querySelector:()=>label};
  model.closest=()=>form;
  let releaseDefault;
  const calls=[];
  const context={creationProviderValue:form=>form?.elements.provider?.dataset?.providerIntent??form?.elements.provider?.value??"",slug:"source",models:[],currentModel:"",modelCatalogs:new Map(),selectModelCatalogs:new WeakMap(),selectModelURLs:new WeakMap(),
    document:{addEventListener(){},querySelectorAll:()=>[model],createElement:()=>({})},
    request:url=>{calls.push(url);return url==="/api/models"?new Promise(resolve=>{releaseDefault=resolve;}):Promise.resolve([{model:"daybreak",isDefault:true}]);},
    selectCreationSetting:(select,value)=>{select.value=value;},populateEfforts(){},pairedEffortSelect(){},applyCurrentSettings(){},applyMemberDefaults(){},restoreManagedCreationDrafts(){},
    restoreDraftSelect:(select,value)=>{if(select)select.value=value;},restoreManagedCatalogRecovery:()=>null,updateCreationProviderHelp(){}};
  const source=readFileSync(__dirname+"/static/app.js","utf8");
  const start=source.indexOf("  const loadModels = async "),end=source.indexOf("\n  const bindModelSelects",start);
  const restoreStart=source.indexOf("  const restoreManagedCreationDraft = "),restoreEnd=source.indexOf("\n  const restoreManagedCreationDrafts = ",restoreStart);
  const api=vm.runInNewContext(source.slice(start,end)+source.slice(restoreStart,restoreEnd)+'\n({loadModels,restoreManagedCreationDraft})',context);
  const old=api.loadModels(form);
  api.restoreManagedCreationDraft(form,{provider:"openai",model:"daybreak",effort:"high"});
  await tick();
  assert.equal(form.elements.provider.value,"openai");assert.equal(label.hidden,false);
  assert(calls.includes("/api/models?provider=openai"));
  assert.equal(model.options[0].value,"daybreak");
  releaseDefault([{model:"default-only",isDefault:true}]);await old;
  assert.equal(model.options[0].value,"daybreak");
  // An inherited draft must also replace the last explicit choice.
  api.restoreManagedCreationDraft(form,{model:"default-only",effort:"high"});await tick();
  assert.equal(form.elements.provider.value,"");assert.equal(model.options[0].value,"default-only");
});


test("failed duration is frozen and structured separately from the member and reason", async t => {
  const {providerOperationView}=await import(moduleURL);
  const operation={phase:"failed",target:"pool",stage:"capturing-settings",thread:"private-id",threadLabel:"architect0",completed:1,total:5,createdAt:"2026-10-18T17:16:58Z",updatedAt:"2026-10-18T17:17:04Z",finishedAt:"2026-10-18T17:17:04Z",reason:"cold_permissions_unsupported"};
  t.mock.method(Date,"now",()=>Date.parse("2026-10-18T18:00:00Z"));
  const first=providerOperationView(operation,catalog.providers);
  assert.equal(first.elapsed,"6s");assert.equal(first.count,"1 of 5");assert.equal(first.threadLabel,"architect0");
  Date.now=()=>Date.parse("2026-10-19T18:00:00Z");
  assert.equal(providerOperationView(operation,catalog.providers).elapsed,"6s");
  assert.match(first.reason,/permissions/);
});

test("failed results are dismissible and canceled preparation retains feedback", async () => {
  const backend={operation:{id:"failed-attempt",phase:"failed",target:"pool",message:"Preparation failed.",canCancel:false}};
  const ui=await popup(backend);
  assert.equal(ui.cancel.hidden,true);assert.equal(ui.dismiss.hidden,false);
  ui.dismiss.emit("click");await tick();
  assert.equal(backend.operation.phase,"failed");assert.equal(backend.operation.dismissed,true);assert.equal(ui.status.textContent,"");
  backend.operation={id:"preparing",phase:"applying",stage:"capturing-settings",canCancel:true,target:"pool"};
  await ui.refresh();assert.equal(ui.cancel.hidden,false);
  backend.operation.cancelRequested=true;backend.operation.canCancel=false;
  await ui.refresh();assert.match(ui.status.textContent,/Canceling provider switch/);assert.equal(ui.cancel.hidden,true);
});


// Exercise the creation initialization and event handlers from the shipped app.
async function creationProviders({locked=false, fail=false}={}) {
  const source=readFileSync(__dirname+"/static/app.js","utf8");
  const listeners=new Map(),help={textContent:""};
  const select={value:"",dataset:{},disabled:true,options:[],
    addEventListener:(name,fn)=>listeners.set("select-"+name,fn),closest:()=>form,
    replaceChildren(...options){this.options=options;this.value=options.find(x=>x.defaultSelected)?.value||"";}};
  const form={elements:{provider:select},dataset:{requestLocked:locked?"true":"false"},
    querySelector:()=>help,addEventListener:(name,fn)=>listeners.set(name,fn)};
  let resolve,reject;const reads=new Promise((yes,no)=>{resolve=yes;reject=no;});
  const reloads=[];
  let nextCatalog={...catalog,default:"openai"},readCount=0;
  const context={document:{addEventListener(){},querySelectorAll:()=>[form],createElement:()=>({})},request:()=>readCount++?Promise.resolve(nextCatalog):reads,
    queueMicrotask,loadModels:async f=>reloads.push(f),modelCatalogs:{clear(){}},
    restoreDraftSelect:(target,value)=>{target.value=value;}};
  const start=source.indexOf("  const creationProviderValue = "),end=source.indexOf('  if (body.hasAttribute("data-index"))',start);
  const api=vm.runInNewContext(source.slice(start,end)+'\n({value:()=>creationProviderValue(form),refresh:()=>refreshCreationProvider(form)})',{...context,form});
  const changeStart=source.indexOf('  document.querySelectorAll("[data-creation-provider]").forEach'),changeEnd=source.indexOf("  let collaborationModes",changeStart);
  vm.runInNewContext(source.slice(start,source.indexOf("  for (const form",start))+source.slice(changeStart,changeEnd),{...context,document:{addEventListener(){},querySelectorAll:()=>[select]}});
  if(fail)reject(new Error("offline"));else resolve({...catalog,default:"openai"});
  await tick();
  return {select,form,help,reloads,value:api.value,changeDefault:async id=>{nextCatalog={...catalog,default:id,active:id};await api.refresh();},change:value=>{select.value=value;listeners.get("select-change")();},reset:async()=>{listeners.get("reset")();await tick();}};
}

test("creation separates workspace inheritance from every explicit provider",async()=>{
  const ui=await creationProviders();
  assert.equal(ui.select.disabled,false);assert.equal(ui.select.value,"");assert.equal(ui.value(),"");
  assert.deepEqual(ui.select.options.map(x=>x.value),["","openai","pool"]);
  assert.equal(ui.select.options[0].textContent,"Workspace default (Subscription)");assert.match(ui.help.textContent,/follows future default/);
  ui.change("pool");assert.equal(ui.value(),"pool");assert.match(ui.help.textContent,/keeps the selected provider/);
  ui.change("openai");assert.equal(ui.value(),"openai");
  ui.change("");assert.equal(ui.value(),"");
  ui.change("pool");await ui.reset();assert.equal(ui.select.value,"");assert.equal(ui.value(),"");
  assert.equal(ui.reloads.at(-1),ui.form);
});

test("creation provider loading respects a locked request and reports unavailable catalogs",async()=>{
  const locked=await creationProviders({locked:true});assert.equal(locked.select.disabled,true);assert.equal(locked.value(),"");
  const offline=await creationProviders({fail:true});assert.equal(offline.select.disabled,true);assert.equal(offline.value(),"");
  assert.match(offline.help.textContent,/Provider list unavailable/);
});


test("creation refreshes the workspace-default label while preserving explicit drafts and locked intent",async()=>{
  const inherited=await creationProviders();await inherited.changeDefault("pool");
  assert.equal(inherited.select.value,"");assert.equal(inherited.value(),"");
  assert.equal(inherited.select.options.find(x=>x.value==="").textContent,"Workspace default (Pool)");
  inherited.change("openai");assert.equal(inherited.value(),"openai");
  // A previously explicit choice stays explicit even if it becomes the default.
  await inherited.changeDefault("openai");assert.equal(inherited.select.value,"openai");assert.equal(inherited.value(),"openai");
  const locked=await creationProviders({locked:true});locked.select.dataset.providerIntent="openai";
  await locked.changeDefault("pool");assert.equal(locked.select.disabled,true);assert.equal(locked.select.value,"openai");assert.equal(locked.value(),"openai");
});

test("structured waiting progress retains the concrete blocker",async()=>{
  const ui=await popup({operation:{id:"waiting",phase:"waiting",stage:"waiting-creation",target:"pool",blockers:["example: initial conversation creation is still running"]}});
  assert.match(ui.status.textContent,/example: initial conversation creation is still running/);
});


test("opening a fork refreshes the default independently of the pinned source", async () => {
  const ui=await creationProviders();
  ui.form.id="fork-form";
  const source=readFileSync(__dirname+"/static/app.js","utf8");
  const start=source.indexOf('  document.getElementById("fork-open")?.addEventListener');
  const end=source.indexOf('  forkDialog?.querySelector',start);
  let open,shown=false,refreshed=false;
  vm.runInNewContext(source.slice(start,end),{
    document:{addEventListener(){},getElementById:()=>({addEventListener:(_,handler)=>{open=handler;}})},
    forkForm:ui.form,forkDialog:{showModal(){shown=true;}},
    refreshCreationProvider:form=>{assert.equal(form,ui.form);refreshed=true;return ui.changeDefault("pool");},
  });
  open();await tick();
  assert(shown&&refreshed);
  assert.equal(ui.select.options[0].textContent,"Workspace default (Pool)");
  assert.equal(ui.value(),"");assert.equal(ui.reloads.at(-1),ui.form);
  ui.change("openai");const reloads=ui.reloads.length;
  await ui.changeDefault("openai");
  assert.equal(ui.value(),"openai");assert.equal(ui.reloads.length,reloads);
  const locked=await creationProviders({locked:true});
  await locked.changeDefault("pool");
  assert.equal(locked.reloads.length,0);assert(locked.select.disabled);
});
