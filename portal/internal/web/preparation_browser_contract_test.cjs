"use strict";
const assert = require("node:assert/strict");
const helpers = require("./static/preparation.js");
const {preparationProgress} = require("./static/creation.js");
const {creationCLICommand, planSessionDraftKey, storeCreationDraft, loadCreationDraft} = require("./static/app.js");
const id = "00000000-0000-4000-8000-000000000001";
const otherID = "00000000-0000-4000-8000-000000000002";
const scope = {id: otherID, url: `/uploads/d-${otherID}`};
const attachment = "00000000-0000-4000-8000-000000000003";
const fields = {name: "", goal: "  Literal <script>request</script>\n", date: "2026-10-03", team: "delegated",
  catalogDigest: "a".repeat(64), model: "", effort: ""};
const status = {requestId: id, receiptId: "f".repeat(64), attempt: 1, url: `/creations/${id}/`,
  state: "running", phase: "naming", initialRequest: fields.goal, startedAt: "2026-10-03T10:00:00Z"};
const storage = () => {
  const values = new Map();
  return {values, getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), removeItem: key => values.delete(key)};
};
const response = (value, code = 200) => ({ok: code >= 200 && code < 300, status: code, json: async () => value});
module.exports = async () => {
  let cases = 0;
  const test = async (name, fn) => { await fn(); cases++; };
  await test("blank and custom names freeze before nonblocking POST", async () => {
    for (const name of ["", "Custom_name"]) {
      const saved = storage(); let posted;
      const request = helpers.createRequestDraft({storage: saved, fields: {...fields, name}, randomUUID: () => id,
        fetch: async (url, options) => {
          posted = options.body;
          assert.equal(url, "/sessions");
          assert.equal(JSON.parse(saved.getItem(helpers.draftKey)).body, posted);
          return response(status, 202);
        }});
      await request.submit([], true);
      const body = new URLSearchParams(posted);
      assert.equal(body.get("name"), name); assert.equal(body.get("goal"), fields.goal);
      assert.deepEqual(body.getAll("model"), [""]); assert.deepEqual(body.getAll("effort"), [""]);
      assert.equal(request.clearAccepted(), status.url); assert.equal(saved.getItem(helpers.draftKey), null);
    }
  });
  await test("missing storage, denied reads, no-op writes and readback failures fail closed", async () => {
    for (const saved of [undefined, {...storage(), getItem() { throw Error("denied"); }},
      {...storage(), setItem() {}}, {...storage(), setItem() {}, getItem() { return null; }}]) {
      assert.throws(() => helpers.createRequestDraft({storage: saved, fields, randomUUID: () => id, fetch: () => assert.fail("POST")}), /storage/);
    }
    const saved = storage(); let calls = 0;
    const request = helpers.createRequestDraft({storage: saved, fields, randomUUID: () => id, fetch: () => {calls++;}});
    saved.setItem = () => {};
    await assert.rejects(request.submit([], true), /storage/); assert.equal(calls, 0);
    assert.notEqual(request.draft.body, null);
  });
  await test("lost response reload recovers before catalog or uploads", async () => {
    const saved = storage(), calls = [];
    let originalBody;
    const first = helpers.createRequestDraft({storage: saved, fields: {...fields, model:"exact-model", effort:"xhigh"}, randomUUID: () => id,
      fetch: async (url, options) => { originalBody = options.body; throw Error("response lost"); }});
    first.bindScope(scope);
    await assert.rejects(first.submit([attachment], true), /response lost/);
    const second = helpers.createRequestDraft({storage: saved, fields: {...fields, goal:"edited", catalogDigest:"b".repeat(64)}, randomUUID: () => assert.fail("new ID"),
      legacyStorage: () => assert.fail("attempted recovery consulted shared uploads"),
      fetch: async (url, options) => { calls.push([url, options]); return response(status); }});
    assert.equal(second.draft.body, originalBody);
    assert.throws(() => second.update({goal:"different"}), /Recover/);
    assert.throws(() => second.bindScope(scope), /bind/);
    await second.submit([otherID], true);
    assert.deepEqual(calls.map(([url]) => url), [`/api/session-creations/${id}`]);
    assert.equal(second.draft.goal, fields.goal); assert.equal(second.draft.model, "exact-model");
    assert.equal(second.draft.catalogDigest, "a".repeat(64));
    assert.deepEqual(second.draft.attachmentIds, [attachment]);
  });
  await test("404 retries exact serialized body and never unlocks attempted ID", async () => {
    const saved = storage(); let firstBody;
    let request = helpers.createRequestDraft({storage: saved, fields, randomUUID: () => id,
      fetch: async (_, options) => { firstBody = options.body; throw Error("lost"); }});
    await assert.rejects(request.submit([], true), /lost/);
    const calls = [];
    request = helpers.createRequestDraft({storage: saved, fields: {...fields, team:"different"}, randomUUID: () => otherID,
      fetch: async (url, options) => {
        calls.push([url, options]);
        return url === "/sessions" ? response({error:"still unavailable"}, 503) : response({}, 404);
      }});
    await assert.rejects(request.submit(), /still unavailable/);
    assert.deepEqual(calls.map(([url]) => url), [`/api/session-creations/${id}`, "/sessions"]);
    assert.equal(calls[1][1].body, firstBody); assert.equal(request.draft.requestId, id);
    assert.throws(() => request.update({name:"new"}), /Recover/);
  });
  await test("only an exact request-bound persistence 503 permits frozen POST repair", async () => {
    for (const failure of [
      {requestId:otherID,code:"preparation_persistence_unconfirmed"},
      {requestId:id,code:"different"}, {code:"preparation_persistence_unconfirmed"},
      {requestId:id,code:"preparation_persistence_unconfirmed",http:409},
    ]) {
      const saved=storage();let calls=[];
      const request=helpers.createRequestDraft({storage:saved,fields,randomUUID:()=>id,
        fetch:async(url,options)=>{calls.push([url,options]);if(url==="/sessions")throw Error("lost");return response(failure,failure.http||503);}});
      await assert.rejects(request.submit([],true),/lost/);calls=[];
      const old=saved.getItem(helpers.draftKey);
      await assert.rejects(request.submit());
      assert.deepEqual(calls.map(([url])=>url),[`/api/session-creations/${id}`]);
      assert.equal(saved.getItem(helpers.draftKey),old);assert.throws(()=>request.update({goal:"new"}),/Recover/);
    }
    const saved=storage();let calls=[],healthy=false;
    const pending={requestId:id,code:"preparation_persistence_unconfirmed",error:"not yet confirmed"};
    const request=helpers.createRequestDraft({storage:saved,fields,randomUUID:()=>id,fetch:async(url,options)=>{
      calls.push([url,options]);return healthy ? response(status,202) : response(pending,503);
    }});
    request.bindScope(scope);await assert.rejects(request.submit([attachment],true),/not yet/);
    const frozen=request.draft.body;
    await assert.rejects(request.submit(),/not yet/);
    assert.deepEqual(calls.map(([url])=>url),["/sessions",`/api/session-creations/${id}`,"/sessions"]);
    assert.equal(calls[2][1].body,frozen);assert.deepEqual(request.draft.attachmentIds,[attachment]);
    healthy=true;await request.submit();assert.equal(request.draft.accepted.receiptId,status.receiptId);
  });
  await test("stale 409 and missing status preserve the original envelope and selection", async () => {
    const saved=storage(),calls=[];
    saved.setItem(`workspace-portal.upload-draft.${scope.id}`,JSON.stringify([{id:attachment,name:"original.txt",state:"ready"}]));
    const request=helpers.createRequestDraft({storage:saved,fields,randomUUID:()=>id,fetch:async(url,options)=>{
      calls.push([url,options]);return url==="/sessions" ? response({error:"catalog digest is stale; reload the form"},409) : response({},404);
    }});
    request.bindScope(scope);await assert.rejects(request.submit([attachment],true),/catalog/);
    const bytes=[...saved.values.entries()],frozen=request.draft.body;
    await assert.rejects(request.submit(),/catalog/);
    assert.deepEqual([...saved.values.entries()],bytes);
    assert.equal(calls.at(-1)[1].body,frozen);assert.equal(request.draft.requestId,id);
    assert.throws(()=>request.update({catalogDigest:"b".repeat(64)}),/Recover/);
    assert.throws(()=>request.bindScope({id:attachment,url:`/uploads/d-${attachment}`}),/bind/);
  });
  await test("a separate tab uses current fields and new scope without adopting old bytes", async () => {
    const oldStorage=storage(),newStorage=storage();let complete=false;
    oldStorage.setItem(`workspace-portal.upload-draft.${scope.id}`,"original file selection");
    const original=helpers.createRequestDraft({storage:oldStorage,fields,randomUUID:()=>id,
      fetch:async(url)=>{if(complete)return response(status);throw Error("unknown outcome");}});
    original.bindScope(scope);await assert.rejects(original.submit([attachment],true),/unknown/);
    const oldBytes=[...oldStorage.values.entries()],oldBody=original.draft.body;
    const fresh=helpers.createRequestDraft({storage:newStorage,fields:{...fields,goal:"",catalogDigest:"b".repeat(64)},randomUUID:()=>otherID,
      legacyStorage:()=>assert.fail("separate tab read old uploads"),fetch:()=>assert.fail("separate draft auto submitted")});
    fresh.bindScope({id:attachment,url:`/uploads/d-${attachment}`});
    assert.notEqual(fresh.draft.requestId,original.draft.requestId);assert.notEqual(fresh.draft.scope.id,original.draft.scope.id);
    assert.equal(fresh.draft.catalogDigest,"b".repeat(64));assert.equal(fresh.draft.goal,"");assert.equal(fresh.draft.body,null);
    assert.deepEqual(fresh.draft.attachmentIds,[]);assert.deepEqual([...oldStorage.values.entries()],oldBytes);
    const freshBytes=[...newStorage.values.entries()];complete=true;await original.submit();
    assert.deepEqual([...newStorage.values.entries()],freshBytes);assert.equal(original.draft.body,oldBody);
    assert.equal(oldStorage.getItem(`workspace-portal.upload-draft.${scope.id}`),"original file selection");
  });
  await test("a restored attempted draft is preserved rather than forced into a new identity", async () => {
    const saved=storage();const original=helpers.createRequestDraft({storage:saved,fields,randomUUID:()=>otherID,fetch:async()=>{throw Error("lost");}});
    await assert.rejects(original.submit([],true),/lost/);const bytes=saved.getItem(helpers.draftKey);
    const restored=helpers.createRequestDraft({storage:saved,fields:{...fields,catalogDigest:"b".repeat(64)},randomUUID:()=>assert.fail("forced UUID"),fetch:()=>assert.fail("auto submission")});
    assert.equal(restored.draft.requestId,otherID);assert.equal(saved.getItem(helpers.draftKey),bytes);
  });
  await test("double-click shares one request and acceptance", async () => {
    const saved = storage(); let complete, count = 0;
    const request = helpers.createRequestDraft({storage: saved, fields, randomUUID: () => id,
      fetch: async () => { count++; return new Promise(resolve => {complete = resolve;}); }});
    const a = request.submit([], true), b = request.submit([], true);
    assert.equal(a, b); assert.equal(count, 1);
    complete(response(status, 202)); await a; await b;
  });
  await test("legacy unsubmitted drafts migrate and separate tabs own scopes", async () => {
    const a = storage(), b = storage(), old = storage();
    old.setItem("workspace-portal.creation-upload-scope",JSON.stringify(scope));
    old.setItem(`workspace-portal.upload-draft.${scope.id}`,JSON.stringify([{name:"previous.txt"}]));
    const oldBytes = [...old.values.entries()];
    a.setItem(helpers.draftKey, JSON.stringify({...fields, name:"saved", model:"retained", effort:"high"}));
    const first = helpers.createRequestDraft({storage:a, fields, randomUUID:()=>id,legacyStorage:()=>old});
    const second = helpers.createRequestDraft({storage:b, fields, randomUUID:()=>otherID,
      legacyStorage:()=>assert.fail("fresh draft consulted shared uploads")});
    assert(first.draft.migratedUploads); assert(!first.draft.scope);
    assert(!second.draft.migratedUploads);
    assert.deepEqual([...old.values.entries()],oldBytes);
    const reloaded = helpers.createRequestDraft({storage:a,fields,legacyStorage:()=>assert.fail("migrated draft consulted shared uploads")});
    assert(reloaded.draft.migratedUploads);
    const empty = storage();empty.setItem(helpers.draftKey,JSON.stringify(fields));
    assert(!helpers.createRequestDraft({storage:empty,fields,randomUUID:()=>id,legacyStorage:()=>storage()}).draft.migratedUploads);
    first.bindScope({id:attachment, url:`/uploads/d-${attachment}`}); second.bindScope({id, url:`/uploads/d-${id}`});
    assert.equal(first.draft.name,"saved"); assert.equal(first.draft.model,"retained");
    assert.notEqual(first.draft.requestId,second.draft.requestId);
    assert.notEqual(first.draft.scope.id,second.draft.scope.id);
    assert.notEqual(first.draft.scope.id,scope.id); assert.notEqual(second.draft.scope.id,scope.id);
    assert(!a.values.has("workspace-portal.creation-upload-scope"));
  });
  await test("attachment-only body keeps ordered IDs and per-request scope", async () => {
    const saved = storage(); let body;
    const request = helpers.createRequestDraft({storage:saved, fields:{...fields, goal:""}, randomUUID:()=>id,
      fetch:async (_, options)=>{body = new URLSearchParams(options.body); return response(status,202);}});
    request.bindScope(scope); await request.submit([attachment,id],true);
    assert.equal(body.get("goal"),""); assert.equal(body.get("uploadScope"),scope.id);
    assert.deepEqual(body.getAll("attachmentIds"),[attachment,id]);
    assert.equal(JSON.parse(saved.getItem(helpers.draftKey)).scope.id,scope.id);
  });
  await test("acceptance and storage cleanup must both be verified", async () => {
    const saved = storage(); let count = 0;
    const request = helpers.createRequestDraft({storage:saved, fields, randomUUID:()=>id,
      fetch:async()=>{count++;return response(status,202);}});
    assert.throws(()=>request.clearAccepted(),/not been confirmed/);
    request.bindScope(scope);
    saved.setItem(`workspace-portal.upload-draft.${scope.id}`,"selected files");
    await request.submit([attachment],true);
    const remove = saved.removeItem; saved.removeItem = () => {};
    assert.throws(()=>request.clearAccepted(),/storage/);
    assert.equal(request.draft.requestId,id); assert.equal(request.draft.accepted.receiptId,status.receiptId);
    assert(saved.getItem(helpers.draftKey)); assert(saved.getItem(`workspace-portal.upload-draft.${scope.id}`));
    saved.removeItem = remove; await request.submit(); assert.equal(count,1);
    assert.equal(request.clearAccepted(),status.url);
    assert.equal(saved.getItem(`workspace-portal.upload-draft.${scope.id}`),null);
  });
  await test("removal readback failure retains recoverable identity", async () => {
    const saved = storage();
    const request = helpers.createRequestDraft({storage:saved, fields, randomUUID:()=>id, fetch:async()=>response(status,202)});
    await request.submit([],true);
    const get = saved.getItem; let removed = false;
    saved.removeItem = key => {saved.values.delete(key);removed=true;};
    saved.getItem = key => {if(removed) throw Error("blocked"); return get(key);};
    assert.throws(()=>request.clearAccepted(),/storage/);
    assert.equal(request.draft.requestId,id);
    assert.equal(JSON.parse(saved.values.get(helpers.draftKey)).requestId,id);
  });
  await test("wrong receipts and unsafe preparation URLs cannot be accepted", async () => {
    for (const changes of [{requestId:otherID},{receiptId:"wrong"},{attempt:0},
      ...["https://evil.test/",`//evil.test/creations/${id}/`,`/creations/${otherID}/`,`/creations/${id}/?next=evil`,`/creations/%30${id.slice(1)}/`,`javascript:alert(1)`].map(url=>({url}))]) {
      const saved = storage();
      const request = helpers.createRequestDraft({storage:saved, fields, randomUUID:()=>id, fetch:async()=>response({...status,...changes},202)});
      await assert.rejects(request.submit([],true),/identity/);
      assert.equal(request.draft.accepted,null); assert(saved.getItem(helpers.draftKey));
    }
  });
  await test("malformed attempted draft is never replaced or resubmitted", async () => {
    const saved = storage();
    const request = helpers.createRequestDraft({storage:saved, fields, randomUUID:()=>id,fetch:async()=>{throw Error("lost");}});
    await assert.rejects(request.submit([],true));
    const bad = request.draft; bad.body += "&goal=second";
    saved.setItem(helpers.draftKey,JSON.stringify(bad));
    assert.throws(()=>helpers.createRequestDraft({storage:saved,fields,randomUUID:()=>otherID}),/unreadable/);
    assert.equal(JSON.parse(saved.getItem(helpers.draftKey)).requestId,id);
  });
  await test("explicit settings remain exact and half-pairs fail before POST", async () => {
    const saved = storage(); let called = false;
    const request = helpers.createRequestDraft({storage:saved,fields:{...fields,model:"exact",effort:""},randomUUID:()=>id,
      fetch:()=>{called=true;}});
    await assert.rejects(request.submit([],true),/both/); assert(!called); assert.equal(request.draft.body,null);
    request.update({effort:"low"});
    assert.equal(request.draft.model,"exact");
  });
  await test("progress phases, attempts, raw text and accepted time", async () => {
    const identity = {requestId:id,receiptId:status.receiptId,startedAt:status.startedAt};
    for (const [phase,label] of [["naming","Choosing"],["reserving","Reserving"],["initializing","Initializing"]]) {
      assert(preparationProgress({...status,phase},identity,helpers).phase.startsWith(label));
    }
    for (const state of ["failed","paused"]) assert(preparationProgress({...status,state,attempt:2},identity,helpers).retry);
    for (const state of ["gone","conflict","cancelled"]) {
      const view = preparationProgress({...status,state,canonicalUrl:"/2026-10-03-unrelated/",slug:"2026-10-03-unrelated"},identity,helpers);
      assert(view.stopped); assert(!view.destination); assert(!view.retry);
    }
    assert.throws(()=>preparationProgress({...status,startedAt:"later"},identity,helpers),/identity/);
    assert.throws(()=>preparationProgress({...status,receiptId:"0".repeat(64)},identity,helpers),/identity/);
    const hostile = '<img src=x onerror="alert(1)">';
    assert.equal(preparationProgress({...status,phase:"initializing",detail:hostile},identity,helpers).phase,hostile);
  });
  await test("canonical navigation requires ready exact receipt and exact dated path", async () => {
    const identity = {requestId:id,receiptId:status.receiptId,startedAt:status.startedAt};
    const ready = {...status,state:"ready",slug:"2026-10-03-example",canonicalUrl:"/2026-10-03-example/"};
    assert.equal(preparationProgress(ready,identity,helpers).destination,ready.canonicalUrl);
    const custom = {...ready,slug:"2026-10-03-My_custom-Work",canonicalUrl:"/2026-10-03-My_custom-Work/"};
    assert.equal(preparationProgress(custom,identity,helpers).destination,custom.canonicalUrl);
    assert(!helpers.sessionURL({...ready,slug:`2026-10-03-${"A".repeat(49)}`,canonicalUrl:`/2026-10-03-${"A".repeat(49)}/`}));
    for (const canonicalUrl of ["https://evil.test/","//evil.test/","/api/","/2026-10-03-example/?x=1","/2026-10-03-other/","/2026-10-03-example/../evil/"]) {
      assert.throws(()=>preparationProgress({...ready,canonicalUrl},identity,helpers),/destination/);
    }
    assert(!helpers.sessionURL({...ready,state:"running"}));
  });
  await test("plan drafts and explicit CLI naming retain their contracts", async () => {
    const saved = storage(), plan = {...fields,name:"plan"};
    const key = planSessionDraftKey("source",{planTurnId:"turn",planSha256:"digest"});
    assert(storeCreationDraft(saved,key,plan,"plan"));
    assert.equal(loadCreationDraft(saved,key,"plan").name,"plan");
    assert(!saved.getItem(helpers.draftKey));
    assert.equal(creationCLICommand({...fields,name:""}),"");
    assert(creationCLICommand({...fields,name:"explicit",model:"exact",effort:"high"}).startsWith("dev-session start 'explicit'"));
  });
  console.log(`Preparation browser contracts passed: ${cases} cases`);
};
if (require.main === module) module.exports().catch(error => {console.error(error);process.exitCode=1;});
