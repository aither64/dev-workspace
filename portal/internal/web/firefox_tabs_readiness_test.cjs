"use strict";
const assert = require("node:assert/strict");
const {test} = require("node:test");
const {waitForRestoredStream} = require("./firefox_tabs_readiness.cjs");

function fixture(states) {
  let time = 0, reads = 0, sleeps = 0;
  const status = async () => {
    const events = states[Math.min(reads++, states.length - 1)];
    return {counts: {0: {events: {Count: 8, Active: 0}}, 5: {events}}};
  };
  return {
    options: {baseline: 3, status, now: () => time,
      sleep: async ms => { assert.equal(ms, 100); time += ms; sleeps++; }},
    observed: () => ({time, reads, sleeps}),
  };
}

test("composer ready and hidden tabs quiescent do not establish restored SSE", async () => {
  // This is exactly the state admitted by the old composer/background gates.
  // Its immediate Count assertion fails even though restoration soon succeeds.
  const before = {Count: 3, Active: 0};
  const run = fixture([before, {Count: 4, Active: 0}, {Count: 4, Active: 1}]);
  const restored = await waitForRestoredStream(run.options);
  assert.equal(restored.counts[5].events.Count, 4);
  assert.deepEqual(run.observed(), {time: 200, reads: 3, sleeps: 2});
});

test("an unchanged counter cannot certify a new stream even if active", async () => {
  const run = fixture([{Count: 3, Active: 1}, {Count: 4, Active: 1}]);
  await waitForRestoredStream(run.options);
  assert.deepEqual(run.observed(), {time: 100, reads: 2, sleeps: 1});
});

test("two active streams cannot satisfy restoration", async () => {
  const run = fixture([{Count: 4, Active: 2}, {Count: 5, Active: 1}]);
  const restored = await waitForRestoredStream(run.options);
  assert.equal(restored.counts[5].events.Active, 1);
  assert.deepEqual(run.observed(), {time: 100, reads: 2, sleeps: 1});
});

for (const [label, states] of [
  ["no new stream", [{Count: 3, Active: 0}]],
  ["new request never remains active", [{Count: 4, Active: 0}]],
  ["missing restored-document counters", [undefined]],
]) {
  test(`${label} fails at the existing 20-second deadline`, async () => {
    const run = fixture(states);
    await assert.rejects(waitForRestoredStream(run.options),
      /Native restored tab 5 did not establish one new stream within 20000ms/);
    assert.deepEqual(run.observed(), {time: 20000, reads: 200, sleeps: 200});
  });
}

test("status transport failure stays a failure", async () => {
  await assert.rejects(waitForRestoredStream({baseline: 3,
    status: async () => { throw Error("Fixture transport failed"); }}),
  /Fixture transport failed/);
});


test("a stream observed after the deadline cannot satisfy restoration", async () => {
  let time = 0;
  await assert.rejects(waitForRestoredStream({baseline: 3, now: () => time,
    status: async () => { time = 20001; return {counts: {5: {events: {Count: 4, Active: 1}}}}; }}),
  /Native restored tab 5 did not establish one new stream within 20000ms/);
});
