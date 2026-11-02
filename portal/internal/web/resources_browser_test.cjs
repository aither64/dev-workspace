"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const {resourceCountPresentation: present, sessionTabFromLocation: tab} = require("./static/app.js");
test("only confirmed zero hides a resource indicator", () => {
 const zero = present({known: true, value: 0, observedAt: "2026-11-02T12:00:00Z"});
 assert.equal(zero.hidden, true);
 assert.equal(present({known: false}).text, "?");
 const stale = present({known: false}, zero);
 assert.equal(stale.hidden, false); assert.equal(stale.text, "0~");
 assert.equal(stale.observedAt, zero.observedAt);
 assert.equal(present({known: true, value: 2}).text, "2");
 assert.equal(present({known: true, value: 2, stale: true}).text, "2~");
 assert.equal(present({known: true, value: -1}).known, false);
 assert.equal(present({known: true, value: "0"}).known, false);
});
test("resource child links and legacy queries select the parent", () => {
 const panels = ["codex", "team", "resources", "files", "settings"];
 for (const child of ["resources", "clusters", "reservations", "services"]) {
  assert.equal(tab({hash: `#${child}`, search: "?member=implementer1"}, panels, "codex"), "resources");
  assert.equal(tab({hash: "", search: `?tab=${child}&member=implementer1`}, panels, "codex"), "resources");
 }
 assert.equal(tab({hash: "#team", search: "?tab=clusters"}, panels, "codex"), "team");
});
