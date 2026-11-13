"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs"), vm = require("node:vm");
const {request} = require("playwright");
const input = JSON.parse(process.argv[2]);
const source = fs.readFileSync("../../../test/creation_acceptance_browser.cjs", "utf8");
function section(start, end) {
  assert.equal(source.split(start).length, 2, "unique source start");
  assert.equal(source.split(end).length, 2, "unique source end");
  const a = source.indexOf(start), b = source.indexOf(end, a);
  assert(a >= 0 && b > a, "ordered source delimiters");
  return source.slice(a, b);
}
const getSource = section("  const get = async ", "  const statusPath =");
const exchangeSource = section("  async function piExchange(marker) {", "  try {\n    if ([\"pi-create\", \"pi-reconnect\"].includes(action))");
const conflict = "The selected Pi conversation changed. Reload the session.";
const header = "X-Workspace-Conversation-Scope";
const clone = value => JSON.parse(JSON.stringify(value));
function contract(context, pageScope, savedScope) {
  const calls = [], pauses = [], saves = [];
  const record = {slug: input.slug, marker: "ORIGINAL_PI"};
  if (savedScope) record.conversationScope = savedScope;
  const locator = selector => ({
    async waitFor() {
      assert([`body[data-session-backend="pi"][data-interactive="true"]`, "#message-send:not([disabled])"].includes(selector));
      calls.push(["wait", selector]);
    },
    async getAttribute(name) {
      assert.equal(selector, "body");
      assert(["data-thread-id", "data-root-thread-id", "data-conversation-scope"].includes(name));
      calls.push(["attribute", name]);
      return name === "data-conversation-scope" ? pageScope : "";
    },
    getByText(text, options) {
      assert.equal(selector, "#transcript"); assert.equal(options.exact, true); assert.equal(Object.keys(options).length, 1);
      assert([record.marker, "CREATION_REPLY_pi"].includes(text));
      return {first: () => ({waitFor: async () => {calls.push(["visible-text", text]);}})};
    },
    async fill(text) {assert.equal(selector, "#message-form textarea[name=message]"); assert.equal(text, "FOLLOWUP_PI"); calls.push(["fill", text]);},
    async click() {assert.equal(selector, "#message-send"); calls.push(["click", selector]);},
  });
  const sandbox = vm.createContext({assert, context, origin: input.origin, record, phase: "", Date,
    page: {locator}, save: () => saves.push(clone(record)),
    pause: async ms => {assert.equal(ms, 200); pauses.push(ms); assert(pauses.length <= 1, "no extra polling/retry");},
  });
  vm.runInContext(getSource + exchangeSource + "\nglobalThis.run=piExchange; globalThis.get=get;", sandbox);
  return {sandbox, record, calls, pauses, saves};
}
(async () => {
  if (input.mode === "unscoped_get_no_leak") {
    const requests = [];
    const context = {request: {get: async (url, options = {}) => {
      requests.push({url, options:clone(options)});
      return {ok: () => true, status: () => 200, text: async () => "{}", json: async () => ({})};
    }}};
    const f = contract(context, input.scope);
    const paths = ["/api/sessions/codex/thread", "/api/session-creations/request-id", "/api/sessions/codex/creation",
      "/api/codex-providers?session=codex", "/api/sessions/codex/attention", "/api/sessions/codex/operation", "/codex/"];
    for (const path of paths) await f.sandbox.get(path);
    await f.sandbox.get(`/api/sessions/${input.slug}/thread`, {[header]:input.scope});
    for (const path of paths) await f.sandbox.get(path);
    assert.equal(requests.length, paths.length * 2 + 1);
    for (let i = 0; i < requests.length; i++) {
      const scoped = i === paths.length;
      assert.equal(requests[i].url, input.origin + (scoped ? `/api/sessions/${input.slug}/thread` : paths[i < paths.length ? i : i - paths.length - 1]));
      assert.deepEqual(requests[i].options, {headers:scoped ? {[header]:input.scope} : {}});
    }
    console.log("NO_LEAK", JSON.stringify(requests)); return;
  }
  const http = await request.newContext({timeout:5000});
  const responses = [];
  try {
    const observed = {request: {get: async (...args) => {
      const response = await http.get(...args);
      responses.push({url:args[0], headers:clone(args[1]?.headers || {}), status:response.status(),
        error:response.ok() ? "" : (await response.text()).trim()});
      return response;
    }}};
    const stale = "pi-workflow-v1:" + "f".repeat(64);
    const f = contract(observed, input.mode === "stale_page_scope" ? stale : input.scope,
      input.mode === "matching_reconnect" ? input.scope : input.mode === "mismatching_saved_scope" ? stale : undefined);
    let failure;
    try {await f.sandbox.run("FOLLOWUP_PI");} catch (error) {failure = error;}
    console.log("SOURCE_BOUNDARY", JSON.stringify({mode:input.mode, calls:f.calls, responses, pauses:f.pauses, saves:f.saves.length,
      syntheticPage:true, syntheticTranscript:true, nativeSendProof:false}));
    if (input.mode === "mismatching_saved_scope") {
      assert(failure); assert.equal(responses.length, 0); assert.equal(f.calls.filter(c => c[0] === "click").length, 0);
      assert.equal(f.calls.filter(c => c[0] === "fill").length, 0); assert.equal(f.saves.length, 0); return;
    }
    assert.equal(f.calls.filter(c => c[0] === "click").length, 1);
    assert.equal(f.calls.filter(c => c[0] === "fill").length, 1);
    assert.deepEqual(f.calls.filter(c => c[0] === "visible-text").map(c => c[1]), ["ORIGINAL_PI", "CREATION_REPLY_pi"]);
    if (input.mode === "stale_page_scope") {
      assert(failure); assert.match(failure.message, /404 .*conversation is unavailable/);
      assert.equal(responses.length, 1); assert.equal(responses[0].headers[header], stale);
      assert.equal(f.pauses.length, 0); assert.equal(f.saves.length, 0); return;
    }
    // OLD must surface the actual first HTTP refusal after recording one click.
    if (failure) {
      assert.equal(responses.length, 1); assert.equal(responses[0].status, 409);
      assert.equal(responses[0].error, conflict); assert.deepEqual(responses[0].headers, {});
      assert.equal(f.saves.length, 0); assert.equal(f.pauses.length, 0); throw failure;
    }
    assert.equal(responses.length, 2);
    for (const response of responses) {
      assert.equal(response.url, input.origin + `/api/sessions/${input.slug}/thread`);
      assert.equal(response.status, 200); assert.deepEqual(response.headers, {[header]:input.scope});
    }
    assert.deepEqual(f.pauses, [200]); assert.equal(f.saves.length, 1);
    assert.equal(f.saves[0].conversationScope, input.scope);
    assert.equal(f.saves[0].piTranscript.status, "idle");
    console.log("COMPLETE", input.mode, "two real HTTP reads; synthetic completion; zero native Send claims");
  } finally {await http.dispose();}
})().catch(error => {console.error(error); process.exitCode = 1;});
