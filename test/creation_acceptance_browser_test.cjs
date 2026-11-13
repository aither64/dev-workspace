"use strict";
const assert = require("node:assert/strict");
const {test} = require("node:test");
const vm = require("node:vm");
const {creationHydration, creationRemaining, creationTarget} = require("./creation_acceptance_browser.cjs");
const settle = () => new Promise(resolve => setImmediate(resolve));
const origin = "https://fixture.workspace.example.test";
const record = {slug: "created", threadId: "new-root", marker: "CREATION_INITIAL_case", options: {}};
const payload = {threadId: record.threadId, entries: [{kind: "userMessage", text: record.marker}]};

function fixture(t, scene = {}) {
  t.mock.timers.enable({apis: ["setTimeout", "Date"], now: 100000});
  const pending = new Set(), set = global.setTimeout, clear = global.clearTimeout;
  t.mock.method(global, "setTimeout", (fn, delay) => {
    const id = set(() => { pending.delete(id); fn(); }, delay);
    pending.add(id); return id;
  });
  t.mock.method(global, "clearTimeout", id => { pending.delete(id); clear(id); });
  const observer = creationHydration(origin), calls = [];
  t.after(() => observer.close());
  const page = {waitForFunction: async (predicate, argument, options) => {
    calls.push({argument, timeout: options.timeout});
    const rows = (scene.rows ?? [record.marker]).map(textContent => ({textContent,
      getBoundingClientRect: () => ({width: scene.hidden ? 0 : 100, height: 20})}));
    const matched = vm.runInNewContext(`(${predicate})(argument)`, {argument,
      location: {href: scene.url ?? origin + "/created/"},
      document: {body: {dataset: {threadId: scene.root ?? record.threadId}},
        querySelectorAll: selector => {
          assert.equal(selector, "#transcript-entries [data-transcript-entry-key]"); return rows;
        }}, getComputedStyle: () => ({visibility: "visible"})});
    if (matched) return;
    await new Promise((_, reject) => setTimeout(() => reject(Error("render deadline")), options.timeout));
  }};
  const observe = (overrides = {}, failure) => observer.observe({
    method: () => overrides.method ?? "GET", url: () => overrides.url ?? origin + "/api/sessions/created/thread/page",
    response: async () => ({status: () => overrides.status ?? 200,
      json: async () => { if (overrides.error) throw overrides.error; return Object.hasOwn(overrides, "payload") ? overrides.payload : payload; }}),
  }, failure);
  return {observer, page, observe, pending, calls,
    advance: async ms => { t.mock.timers.tick(ms); await settle(); }};
}

test("creation hydration uses the actual helper with one shared deadline", async t => {
  await t.test("early completed browser snapshot survives until target identity is known", async t => {
    const f = fixture(t); f.observe(); await settle();
    const target = creationTarget(record.slug, {slug: record.slug, location: "work", archived: false,
      threadId: record.threadId, targetId: "retained-target"});
    await f.observer.wait(f.page, {...record, threadId: target.threadId}, Date.now() + 180000);
    assert.equal(f.calls.length, 1); assert.equal(f.pending.size, 0);
    f.observe({error: Error("after close")}); await settle();
    assert.equal(f.calls.length, 1); assert.equal(f.pending.size, 0);
  });
  await t.test("aborted initial snapshot cannot qualify; ordinary replacement can", async t => {
    const f = fixture(t); f.observe({}, "net::ERR_ABORTED");
    const wait = f.observer.wait(f.page, record, Date.now() + 180000);
    await settle(); assert.equal(f.calls.length, 0);
    f.observe(); await settle(); await f.advance(50); await wait;
    assert.equal(f.calls.length, 1); assert.equal(f.pending.size, 0);
  });
  for (const [label, response, error] of [
    ["wrong snapshot root", {payload: {...payload, threadId: "source-root"}}, /snapshot root/],
    ["HTTP failure", {status: 503}, /HTTP status/],
    ["malformed JSON", {error: SyntaxError("invalid JSON")}, /invalid JSON/],
    ["missing entries", {payload: {threadId: record.threadId}}, /snapshot entries/],
    ["null body", {payload: null}, /snapshot body/],
  ]) await t.test(label + " refuses", async t => {
    const f = fixture(t); f.observe(response); await settle();
    await assert.rejects(f.observer.wait(f.page, record, Date.now() + 180000), error);
    assert.equal(f.calls.length, 0); assert.equal(f.pending.size, 0);
  });
  for (const [label, response] of [
    ["body alone", null],
    ["wrong URL", {url: origin + "/api/sessions/source/thread"}],
    ["wrong origin", {url: "https://other.example/api/sessions/created/thread"}],
    ["non-GET", {method: "POST"}],
  ]) await t.test(label + " cannot satisfy hydration", async t => {
    const f = fixture(t); if (response) f.observe(response); await settle();
    const rejected = assert.rejects(f.observer.wait(f.page, record, Date.now() + 50), /deadline expired/);
    await f.advance(50); await rejected;
    assert.equal(f.calls.length, 0); assert.equal(f.pending.size, 0);
  });
  for (const [label, scene] of [
    ["no rendered entries", {rows: []}], ["uncorrelated row", {rows: ["other entry"]}],
    ["hidden row", {hidden: true}], ["wrong document root", {root: "source-root"}],
    ["noncanonical document", {url: origin + "/source/"}],
  ]) await t.test(label + " cannot satisfy hydration", async t => {
    const f = fixture(t, scene); f.observe(); await settle();
    const rejected = assert.rejects(f.observer.wait(f.page, record, Date.now() + 50), /render deadline/);
    await f.advance(50); await rejected;
    assert.equal(f.pending.size, 0);
  });
  await t.test("ready time is consumed from the same absolute hydration deadline", async t => {
    const f = fixture(t, {rows: []}), deadline = Date.now() + 180000;
    await f.advance(179975);
    f.observe(); await settle();
    const rejected = assert.rejects(f.observer.wait(f.page, record, deadline), /render deadline/);
    assert.equal(f.calls[0].timeout, 25);
    await f.advance(25); await rejected;
    assert.throws(() => creationRemaining(deadline), /deadline expired/);
    assert.equal(f.pending.size, 0);
  });
  await t.test("already expired shared budget cannot accept an early valid snapshot", async t => {
    const f = fixture(t), deadline = Date.now(); f.observe(); await settle();
    await assert.rejects(f.observer.wait(f.page, record, deadline), /deadline expired/);
    assert.equal(f.calls.length, 0); assert.equal(f.pending.size, 0);
  });
  await t.test("late body completion after expiry cannot reopen observation", async t => {
    const f = fixture(t); let resolve;
    f.observe({payload: new Promise(done => { resolve = done; })}); await settle();
    const rejected = assert.rejects(f.observer.wait(f.page, record, Date.now() + 50), /deadline expired/);
    await f.advance(50); await rejected;
    resolve(payload); await settle();
    f.observe(); await settle();
    assert.equal(f.calls.length, 0); assert.equal(f.pending.size, 0);
  });
  for (const marker of [record.marker, "CREATION_PLAN_DONE"]) {
    await t.test("fork/plan fixture marker " + marker, async t => {
      const f = fixture(t, {rows: [marker]});
      f.observe({payload: {threadId: record.threadId, entries: [{kind: "agentMessage", text: marker}]}});
      await settle(); await f.observer.wait(f.page, {...record, marker}, Date.now() + 180000);
      assert.equal(f.calls.length, 1); assert.equal(f.pending.size, 0);
    });
  }
  await t.test("missing or changed public target identity refuses", () => {
    const target = {slug: record.slug, location: "work", archived: false, threadId: "new-root", targetId: "target"};
    for (const value of [null, {...target, slug: "source"}, {...target, location: "archive"},
      {...target, archived: true}, {...target, threadId: ""}, {...target, targetId: ""}]) {
      assert.throws(() => creationTarget(record.slug, value));
    }
  });
});
