"use strict";

// Specific to the native tab-5 discard/restoration acceptance fixture. The
// composer can be ready before its asynchronously opened EventSource arrives.
async function waitForRestoredStream({baseline, status, now = Date.now,
  sleep = ms => new Promise(resolve => setTimeout(resolve, ms))}) {
  const deadline = now() + 20000;
  while (now() < deadline) {
    const snapshot = await status();
    if (now() >= deadline) break;
    const events = snapshot.counts?.[5]?.events;
    if (events?.Count > baseline && events.Active === 1) return snapshot;
    await sleep(100);
  }
  throw Error("Native restored tab 5 did not establish one new stream within 20000ms");
}

module.exports = {waitForRestoredStream};
