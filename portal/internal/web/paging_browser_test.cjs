"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");

const source = fs.readFileSync(require.resolve("./static/app.js"), "utf8");
assert.match(source, /conversationAssets\.createTranscriptHistory\(\{keyFor: transcriptDisplayKey\}\)/);
assert.match(source, /const pagingHelpersAvailable = hasTranscriptPagingHelpers\(conversationAssets\)/);
assert.match(source, /conversationAssets\.readTranscriptPage\(client,/);
assert.match(source, /const entryKey = transcriptDisplayKey\(entry, index, entries\)/);
assert.doesNotMatch(source, /const createTranscriptHistory\s*=/);
assert.doesNotMatch(source, /const readTranscriptPage\s*=/);
assert.match(source, /renderThread\(page, \(\) => historyRead === read && read\.isCurrent\(\) && threadID === currentThreadId/);
const historyRead = source.slice(source.indexOf("const runHistoryRead = async"),
  source.indexOf("const scheduleHistoryRepair =", source.indexOf("const runHistoryRead = async")));
assert.match(historyRead, /const readVersion = transcriptHistory\.repairVersion/);
assert.equal((historyRead.match(/readVersion !== transcriptHistory\.repairVersion/g) || []).length, 2);
assert.match(source, /else refreshLegacyTranscriptView\(transcript, view, follow,/);
assert.match(source, /const nativeTranscriptKey = conversationAssets\.transcriptEntryKey \|\| legacyTranscriptEntryKey/);
assert.match(source, /changed: legacyTranscriptChanges\(transcriptEntries, projected\.entries, transcriptDisplayKey\)/);
assert.match(source, /updateTranscriptUnread\(\{views: transcriptViews/);
assert.match(source, /renderMessageReceipts\(\)/);
assert.match(source, /observePageReceipts\(transcriptEntries, currentThreadId\)/);
assert.match(source, /sendAcknowledgementCandidates\(transcriptEntries,/);
assert.doesNotMatch(source, /transcript_reset_required"\) transcriptHistory\.clear\(/);
console.log("portal shared-pagination integration contracts passed");

// Visible projections exclude closed bodies, timestamps and hidden content.
const {transcriptVisibleContent, updateTranscriptUnread} = require("./static/app.js");
const text = value => ({nodeType: 3, textContent: value});
const node = (tag, children = [], extra = {}) => ({nodeType: 1, tagName: tag.toUpperCase(), childNodes: children,
  children: children.filter(child => child.nodeType === 1), hidden: false, open: false,
  classList: {contains: name => name === extra.className}, getAttribute: name => extra[name] || null, ...extra});
const body = (value, opened = false) => node("div", [
  node("details", [node("summary", [text("Summary")]), node("pre", [text(value)])], {open: opened}),
  node("div", [text(value)], {className: "message-footer"}),
]);
assert.equal(transcriptVisibleContent(body("old")), transcriptVisibleContent(body("new")));
assert.notEqual(transcriptVisibleContent(body("old", true)), transcriptVisibleContent(body("new", true)));
assert.notEqual(transcriptVisibleContent(node("a", [text("file")], {href: "/old"})),
  transcriptVisibleContent(node("a", [text("file")], {href: "/new"})));
const views = new Map(["messages", "activity"].map(filter => [filter, {unread: false, disclosures: new Map()}]));
const message = {id: "message", kind: "message", text: "old", turnId: "live"};
const activity = {id: "activity", kind: "activity", text: "hidden"};
const update = (previous, entries, kind = "newest") => updateTranscriptUnread({views, previous, entries,
  changed: new Set(entries.map(entry => entry.id)), kind, keyFor: entry => entry.id,
  visible: (entry, filter) => entry.kind === (filter === "messages" ? "message" : "activity"),
  content: (entry, _index, _entries, disclosures) => JSON.stringify([entry.text, disclosures.get(entry.id) ? entry.details : ""]),
  liveTurnID: "live"});
update([message, activity], [message, {...activity, text: "tool update"}]);
assert.equal(views.get("messages").unread, false); assert.equal(views.get("activity").unread, true);
update([message], [{...message, text: "streamed"}]); assert.equal(views.get("messages").unread, true);
update([message, activity], [message, {...activity, text: "another tool update"}]);
assert.equal(views.get("messages").unread, true, "hidden change preserves existing unread");
views.get("messages").unread = false;
update([message], [{...message, timestamp: "new", receipt: "new", details: "closed body"}]);
assert.equal(views.get("messages").unread, false);
views.get("messages").disclosures.set(message.id, true);
update([message], [{...message, details: "open body"}]); assert.equal(views.get("messages").unread, true);
views.get("messages").unread = false;
update([], [message], "older"); assert.equal(views.get("messages").unread, false);
update([], [message], "repair"); assert.equal(views.get("messages").unread, false);
update([{...message, turnId: "old"}], [{...message, turnId: "old", text: "historical repair"}], "repair");
assert.equal(views.get("messages").unread, false);
update([message], [{...message, text: "final stream"}], "repair"); assert.equal(views.get("messages").unread, true);
console.log("portal visible unread projection and history contracts passed");

// A page-local turn can change without changing the retained native item.
const {piTranscriptEntryKey, splitPiTranscriptEntries, legacyTranscriptEntryKey} = require("./static/app.js");
const native = {kind: "agentMessage", itemId: "native-a", turnId: "user-before-page", text: "same"};
assert.equal(piTranscriptEntryKey(native), piTranscriptEntryKey({...native, turnId: ""}));
assert.notEqual(piTranscriptEntryKey(native), piTranscriptEntryKey({...native, itemId: "native-b"}));
assert.notEqual(piTranscriptEntryKey(native), piTranscriptEntryKey({...native, itemId: "native-a:pi-block:0"}));
for (const entry of [{kind:"sessionMessage",itemId:"session-message:one"}, {...native, itemId:"pi-plan:one",piPlanId:"one"}, {kind:"error",turnId:"turn"}]) {
  assert.equal(piTranscriptEntryKey(entry), legacyTranscriptEntryKey(entry, 0, []), "existing projection/fallback namespace");
}
const rows = [native, {itemId:"pi-stream"}, {itemId:"pi-stream:pi-block:0"}, {itemId:"pi-stream:pi-block:12"},
  {itemId:"pi-stream:pi-block:01"}, {itemId:"pi-stream-other"}];
const split = splitPiTranscriptEntries(rows);
assert.deepEqual(split.live, rows.slice(1,4));
assert.deepEqual(split.entries, [rows[0],...rows.slice(4)]);
assert.equal(rows.length,6,"projection leaves native rows untouched");
assert.match(source, /if \(piBackend && kind === "newest"\) piLiveEntries = projected.live/);
assert.match(source, /piLiveEntries = \[\]; clearPiLiveDisplay\(\)/);
console.log("Pi item identity and exact live projection contracts passed");
