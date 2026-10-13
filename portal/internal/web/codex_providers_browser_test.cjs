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

test("operation status distinguishes waiting, applying and recovery", async () => {
  const {providerOperationText} = await import(moduleURL);
  assert.match(providerOperationText({phase:"waiting",target:"pool",blockers:["Active goal."]},catalog.providers),/when all workspace threads are idle.*Active goal/);
  assert.match(providerOperationText({phase:"applying",target:"pool"},catalog.providers),/Switching workspace threads/);
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
    return {options:[],value:"",textContent:"",hidden:false,disabled:false,
      addEventListener(type,fn) {listeners.set(type,fn);},
      emit(type) {listeners.get(type)?.();},
      replaceChildren(...options) {this.options=options;}};
  };
  const controls = new Map(["controls","select","switch","cancel","status","active"].map(name => [name,control()]));
  const dialog = control(); dialog.open=true;
  dialog.querySelector = selector => controls.get(selector.match(/\[data-provider-(.*)\]/)[1]);
  dialog.close = () => {dialog.open=false;dialog.emit("close");};
  let poll;
  const context = {dialog,...helpers,providers:null,providerBusy:false,providerAttempt:null,observedActiveProvider:"",
    limitsSnapshot:null,limitsLeaving:false,limits:{invalidate(){},refresh:async()=>{}},renderAccountDetails(){},
    crypto:{randomUUID},document:{hidden:false,createElement:control},
    setInterval:fn => {poll=fn;return 1;},clearInterval(){},addEventListener(){},
    request:async (url,options) => {
      if (url === "/api/codex-providers") return {...catalog,operation:backend.operation ? {...backend.operation} : null};
      if (url.startsWith("/api/codex-limits?")) return {provider:{id:new URL(url,"http://fixture").searchParams.get("provider")},windows:[]};
      const body=JSON.parse(options.body);
      if (url.endsWith("/switch")) backend.operation={id:body.idempotencyKey,target:body.providerId,phase:"waiting"};
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
  await first.refresh();assert.match(first.status.textContent,/Waiting to switch/);
  const second=await popup(backend);
  second.cancel.emit("click");await tick();
  assert.equal(second.status.textContent,"Provider switch canceled.");
  await first.poll();assert.equal(first.status.textContent,"");
  const reloaded=await popup(backend);
  assert.equal(reloaded.status.textContent,"");
});

test("accepted switch retains local feedback until close or preview change", async () => {
  const backend={operation:null};
  const ui=await popup(backend);
  await ui.selectProvider("pool");
  ui.switch.emit("click");await tick();
  assert.match(ui.status.textContent,/Waiting to switch/);
  backend.operation={...backend.operation,phase:"canceled"};
  await ui.poll();assert.equal(ui.status.textContent,"Provider switch canceled.");
  ui.dialog.close();assert.equal(ui.status.textContent,"");
  ui.dialog.open=true;
  await ui.poll();assert.equal(ui.status.textContent,"");
  backend.operation={id:"next",target:"pool",phase:"waiting"};
  await ui.refresh();ui.cancel.emit("click");await tick();
  assert.equal(ui.status.textContent,"Provider switch canceled.");
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
