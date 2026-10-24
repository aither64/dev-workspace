"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");

const source = fs.readFileSync(require.resolve("./static/app.js"), "utf8");
assert.match(source, /conversationAssets\.createTranscriptHistory\(\)/);
assert.match(source, /const pagingHelpersAvailable = hasTranscriptPagingHelpers\(conversationAssets\)/);
assert.match(source, /conversationAssets\.readTranscriptPage\(client,/);
assert.match(source, /conversationAssets\.transcriptEntryKey\(entry,/);
assert.doesNotMatch(source, /const createTranscriptHistory\s*=/);
assert.doesNotMatch(source, /const readTranscriptPage\s*=/);
assert.match(source, /renderThread\(page, \(\) => historyRead === read && read\.isCurrent\(\) && threadID === currentThreadId/);
const historyRead = source.slice(source.indexOf("const runHistoryRead = async"),
  source.indexOf("const scheduleHistoryRepair =", source.indexOf("const runHistoryRead = async")));
assert.match(historyRead, /const readVersion = transcriptHistory\.repairVersion/);
assert.equal((historyRead.match(/readVersion !== transcriptHistory\.repairVersion/g) || []).length, 2);
assert.match(source, /else refreshLegacyTranscriptView\(transcript, view, follow,/);
assert.match(source, /legacyTranscriptEntryKey\(entry, index, entries\)/);
assert.match(source, /changed: legacyTranscriptChanges\(transcriptEntries, payload\.entries \|\| \[\]\)/);
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
