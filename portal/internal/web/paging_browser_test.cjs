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
assert.match(source, /renderThread\(page, \(\) => read\.isCurrent\(\)/);
const historyRead = source.slice(source.indexOf("const runHistoryRead = async"),
  source.indexOf("const scheduleHistoryRepair =", source.indexOf("const runHistoryRead = async")));
assert.match(historyRead, /const readVersion = transcriptHistory\.repairVersion/);
assert.equal((historyRead.match(/readVersion !== transcriptHistory\.repairVersion/g) || []).length, 2);
assert.match(source, /else refreshLegacyTranscriptView\(transcript, view, follow,/);
assert.match(source, /legacyTranscriptEntryKey\(entry, index, entries\)/);
assert.match(source, /changed: legacyTranscriptChanges\(transcriptEntries, payload\.entries \|\| \[\]\)/);
assert.match(source, /kind === "newest" && transcriptInitialized && update\.changed\.size/);
assert.match(source, /renderMessageReceipts\(\)/);
assert.match(source, /observePageReceipts\(transcriptEntries, currentThreadId\)/);
assert.match(source, /sendAcknowledgementCandidates\(transcriptEntries,/);
assert.doesNotMatch(source, /transcript_reset_required"\) transcriptHistory\.clear\(/);
console.log("portal shared-pagination integration contracts passed");
