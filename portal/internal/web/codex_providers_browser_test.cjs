"use strict";
const assert = require("node:assert/strict");
const {test} = require("node:test");
const {pathToFileURL} = require("node:url");
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
});
