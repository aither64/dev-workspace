"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const helpers = require("./static/preparation.js");
const {preparationProgress, ignorePreparationStatus, createNameSelection} = require("./static/creation.js");

// These are real HTTP projections supplied by the owning Go test, including
// the independent preparation and initialization generations.
const states = JSON.parse(fs.readFileSync(0, "utf8"));
const identity = {requestId: states.failure.requestId, receiptId: states.failure.receiptId, startedAt: states.failure.startedAt};
const initial = {...states.failure, state: "running", phase: "reserving", attempt: 2};
const values = new Map();
const storage = {getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), removeItem: key => values.delete(key)};

(async () => {
  assert.equal(states.failure.attempt, 1);
  assert.equal(states.failure.preparationAttempt, 2);
  assert.equal(ignorePreparationStatus(states.failure, initial), false);
  assert.equal(preparationProgress(states.failure, identity, helpers).retry, true);
  assert.equal(ignorePreparationStatus(states.immediateReady, initial), false);
  assert.equal(preparationProgress(states.immediateReady, identity, helpers).destination, states.immediateReady.canonicalUrl);
  assert.equal(ignorePreparationStatus(states.ready, states.failure), false);
  assert.equal(preparationProgress(states.ready, identity, helpers).destination, states.ready.canonicalUrl);
  assert.equal(ignorePreparationStatus(states.compacted, states.ready), false);
  assert.equal(preparationProgress(states.compacted, identity, helpers).destination, states.ready.canonicalUrl);
  assert.equal(ignorePreparationStatus(initial, states.failure), true);

  let originalBody;
  const choice = createNameSelection({storage, identity, helpers, fetch: async (_url, options) => {
    originalBody = options.body;
    throw new Error("lost response");
  }});
  await assert.rejects(choice.submit(1, "workspace-notifications"), /lost response/);
  for (const response of [states.failureReplay, states.terminalReplay]) {
    const restored = createNameSelection({storage, identity, helpers, fetch: async (_url, options) => {
      assert.equal(options.body, originalBody);
      return {ok: true, status: 202, json: async () => response};
    }});
    await restored.submit(1, "ignored");
    assert.equal(restored.pending, null);
    // Restore the same original uncertain choice to exercise terminal replay.
    storage.setItem(`workspace-portal.creation-name.${identity.requestId}`, originalBody);
  }
  console.log("Naming handoff browser contract passed with actual server projections");
})().catch(error => { console.error(error); process.exitCode = 1; });
