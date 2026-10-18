"use strict";
const assert = require("node:assert/strict");
const {createIndexSnapshotNotice, createTranscriptTimestampPresentation} = require("./static/app.js");

module.exports = (assets) => {
  let now = 0, sequence = 0;
  const timers = new Map(), listeners = new Map();
  const page = {hidden: false, addEventListener(name, callback) {
    if (!listeners.has(name)) listeners.set(name, new Set());
    listeners.get(name).add(callback);
  }, removeEventListener(name, callback) { listeners.get(name)?.delete(callback); }};
  const advance = (milliseconds) => {
    const target = now + milliseconds;
    while (true) {
      const next = [...timers].filter(([, timer]) => timer.at <= target).sort((a, b) => a[1].at - b[1].at)[0];
      if (!next) break;
      now = next[1].at;
      timers.delete(next[0]);
      next[1].run();
    }
    now = target;
  };
  const visible = (value) => {
    page.hidden = !value;
    listeners.get("visibilitychange")?.forEach(callback => callback());
  };
  const createNotice = (options) => assets.createRefreshNotice({...options, document: page, window: page,
    now: () => now, setTimeout: (run, delay) => { const id = ++sequence; timers.set(id, {run, at: now + delay}); return id; },
    clearTimeout: id => timers.delete(id),
  });

  let index;
  const notice = createNotice({render: value => { index = value; }});
  const update = createIndexSnapshotNotice(notice);
  update({loading: true, warning: "Session status is loading."});
  advance(749); assert.equal(index.loading, false);
  update({loading: true});
  advance(1); assert.equal(index.loading, true);
  update({warning: "Last known state"});
  advance(15_000); update({warning: "Last known state"});
  advance(14_999); assert.equal(index.warning, false);
  advance(1); assert.equal(index.warning, true);
  update({warning: ""}); assert.equal(index.warning, false);
  notice.failure(new Error("Transport failed"));
  advance(15_000); update({warning: "Snapshot failed"});
  advance(15_000); assert.equal(index.warning, true);
  visible(false); advance(60_000); visible(true);
  assert.equal(index.warning, false);
  advance(30_000); assert.equal(index.warning, true);
  update({});
  notice.failure(Object.assign(new Error("Denied"), {status: 403}));
  assert.equal(index.warning, true);
  notice.destroy();

  const times = createTranscriptTimestampPresentation({
    format: entry => assets.formatTranscriptTimestamp(entry, {locales: "en", timeZone: "UTC"}),
    keyFor: assets.transcriptEntryKey, createNotice,
  });
  const estimated = {turnId: "turn-1", itemId: "message-1", timestamp: "2026-10-18T11:00:00Z", timestampApproximate: true};
  const present = entry => times.present(entry, 0, [entry]);
  const observe = (entry, metadataPending = true, threadId = "thread-1") => times.observe({threadId, metadataPending, entries: [entry]});
  observe(estimated);
  assert.equal(present(estimated).text, "");
  assert.equal(present(estimated).dateKey, "");
  advance(15_000); observe(estimated);
  advance(14_999); assert.equal(present(estimated).text, "");
  advance(1); assert.equal(present(estimated).text, "~11:00");
  visible(false); advance(60_000); visible(true);
  assert.equal(present(estimated).text, "");
  const exact = {...estimated, timestamp: "2026-10-18T11:04:00Z", timestampApproximate: false};
  observe(exact, false);
  assert.equal(present(exact).text, "11:04");
  observe(estimated);
  assert.equal(present(estimated).text, "11:04");
  assert.equal(present({...estimated, timestamp: undefined}).text, "11:04");
  const corrected = {...exact, timestamp: "2026-10-19T00:01:00Z"};
  observe(corrected);
  assert.equal(present(estimated).text, "00:01");
  assert.equal(present(estimated).dateKey, "2026-10-19");
  observe(estimated, true, "thread-2");
  assert.equal(present(estimated).text, "");
  observe(estimated, false, "thread-2");
  assert.equal(present(estimated).text, "~11:00");
  times.observe({threadId: "legacy", entries: [estimated]});
  assert.equal(present(estimated).text, "~11:00");
  times.destroy();
  assert.equal(timers.size, 0);
};
