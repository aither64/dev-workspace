// Post-review browser acceptance. This uses the shipped scripts with a local
// HTTP fixture; it never starts Codex or sends a production naming prompt.
// Use PLAYWRIGHT_MODULE, CHROMIUM_EXECUTABLE and CODEX_WEB_SOURCE as documented.
const http = require("node:http"), fs = require("node:fs"), path = require("node:path"), assert = require("node:assert/strict");
const {randomUUID, createHash} = require("node:crypto");
const root = process.cwd(), provider = process.env.CODEX_WEB_SOURCE;
for (const key of ["PLAYWRIGHT_MODULE", "CHROMIUM_EXECUTABLE", "CODEX_WEB_SOURCE"]) {
 assert(process.env[key], `Missing explicit browser fixture prerequisite: ${key}`);
}
const {chromium} = require(process.env.PLAYWRIGHT_MODULE);
assert(fs.existsSync(path.join(provider,"conversation/assets/conversation.js")),"Pinned provider assets are missing");
assert(fs.existsSync(process.env.CHROMIUM_EXECUTABLE),"Explicit Chromium executable is missing");
const prompt = "Keep this request <script>window.promptInjected=true</script>\nSecond line.";
const catalog = {current: "a".repeat(64)};
let failure = "network-before", modelsUnavailable = true, submissions = [], retries = [], statusReads = [];
const operations = new Map(), scopes = new Map(), removableScopes = new Set();
let holdCompletions = false;
const completionWaiters = [];
const conversationFiles = Array.from({length:50}, (_,i) => ({id:randomUUID(), clientId:randomUUID(), name:`conversation-${i}-${"long-name-".repeat(20)}.txt`, size:1024, state:"ready"}));
const limits = {fileBytes: 1048576, promptBytes: 10485760, files: 50, chunkBytes: 4096};
const legacy = {slug:"2026-09-13-example",url:"/2026-09-13-example/",receiptId:"legacy-receipt",attempt:1,state:"failed",phase:"Initialization stopped",error:"context deadline exceeded",initialRequest:prompt,startedAt:new Date().toISOString()};
const escape = text => text.replaceAll("&","&amp;").replaceAll("<","&lt;").replaceAll(">","&gt;").replaceAll('"',"&quot;");
const managedPolicy = (digest, lead = true) => `<input type="hidden" name="catalogDigest" value="${digest}">
<label>Starting team<select name="team" data-team-select required><option value="solo" selected data-description="Solo team" data-roles="team lead" ${lead ? 'data-lead-model="test-model" data-lead-effort="xhigh"' : ""}>solo</option><option value="delivery" data-description="Delivery team" data-roles="team lead, implementer" ${lead ? 'data-lead-model="alternate-model" data-lead-effort="xhigh"' : ""}>delivery</option></select></label>
<p data-team-description></p><p data-team-catalog-changed role="status" aria-live="polite" hidden>Available teams changed.</p>
<label data-team-catalog-acknowledgement hidden><input type="checkbox" data-team-catalog-acknowledge> I checked the current team and lead settings.</label>`;
const settings = `<label>Lead model<select name="model" data-model-select><option value="">Selected team default</option></select></label><label>Lead reasoning effort<select name="effort" data-effort-select><option value="">Selected team default</option></select></label>`;
const styles = (conversation = false) => `<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">${conversation ? '<link rel="stylesheet" href="/codex/assets/conversation.css?v=5">' : ""}<link rel="stylesheet" href="/static/style.css?v=5"><link rel="stylesheet" href="/codex/assets/uploads.css?v=3"></head>`;
const index = () => `<!doctype html>${styles()}<body data-index><div class="workspace-shell index-shell"><aside class="workspace-sidebar index-sidebar"><h1>Workspace</h1></aside><main class="page index-page"><section class="panel new-session"><h2>New session</h2><form id="new-session-form" class="stack"><input name="creation_date" type="hidden" value="2026-09-13"><textarea name="goal" required></textarea>${managedPolicy(catalog.current)}<details><summary>Options</summary><input name="name" maxlength="48" pattern="[A-Za-z0-9][A-Za-z0-9_-]*">${settings}<div><input data-cli-command readonly><button type="button" data-copy disabled>Copy</button></div></details><p id="creation-draft-notice" hidden>Files from an older draft need to be attached again to this request.</p><input name="uploadScope" type="hidden"><div id="creation-uploads"></div><p id="new-session-progress" hidden></p><div id="new-session-recovery" hidden><p data-request-identity></p><button type="button" id="new-session-recover">Recover saved request</button><div id="new-session-separate" hidden><p>The original request may still finish.</p><div><textarea data-saved-request-text readonly></textarea><button type="button" data-copy>Copy saved text</button></div><a href="/" target="_blank" rel="noopener noreferrer">Start a separate request</a></div></div><div class="creation-actions"><span id="creation-upload-controls"></span><button type="submit" disabled>Create session</button></div></form></section></main></div><script src="/static/preparation.js?v=3"></script><script src="/static/app.js?v=25"></script>`;
const planPage = (turnID, digest) => `<!doctype html>${styles(true)}<body data-session="source" data-thread-id="thread-1" data-interactive="true"><div id="conversation-connection"></div><div id="codex-status"></div><div id="transcript"></div>
<p id="pending-status" class="lane-status" role="status"><span>Loading requests…</span> <button id="pending-retry" type="button" class="quiet" hidden>Retry</button></p><div id="pending" class="pending"></div>
<p id="queue-status" class="lane-status" role="status"><span>Checking queued messages…</span> <button id="queue-retry" type="button" class="quiet" hidden>Retry</button></p>
<button id="new-output" type="button" hidden>New output</button><section id="plan-actions" data-plan-turn-id="${turnID}" data-plan-sha256="${digest}"><button id="plan-implement-new" type="button">Implement in a new session</button></section><form id="message-form"><textarea name="message"></textarea><div id="message-uploads"></div><span id="message-upload-controls"></span><button id="message-send" type="submit">Send</button><button id="message-queue" type="button"></button><button id="interrupt" type="button">Interrupt</button></form><dialog id="plan-session-dialog"><form id="plan-session-form"><input name="creationDate" value="2026-09-13"><input name="name" required>${managedPolicy(catalog.current, false)}${settings}<p id="plan-session-progress" hidden></p><button type="submit">Create session</button></form></dialog><script>document.getElementById("plan-actions").planText="Approved plan";</script><script src="/static/app.js?v=25"></script>`;
const creation = record => `<!doctype html>${styles(true)}<body ${record.requestId ? `data-preparation="${record.requestId}" data-receipt-id="${record.receiptId}" data-accepted-at="${record.startedAt}"` : `data-creation="${record.slug}"`}><h1 id="creation-title"></h1><p id="creation-phase"></p><p id="creation-error"></p><button id="creation-retry" hidden>${record.requestId ? "Retry creation" : "Retry initialization"}</button><p id="creation-elapsed"></p><p><a id="creation-source" hidden>Return to the source session</a></p><p id="creation-leave-note">You can leave this page while initialization continues.</p><section id="creation-request-panel"><span id="creation-request-copy"></span><pre id="creation-request">${escape(record.initialRequest)}</pre></section><script src="/static/preparation.js?v=3"></script><script src="/static/creation.js?v=3"></script>`;
const server = http.createServer(async (req,res) => {
 const url = new URL(req.url,"http://fixture");
 const send = (type,data,status=200) => {res.writeHead(status,{"Content-Type":type});res.end(data);};
 const json = (data,status=200) => send("application/json",JSON.stringify(data),status);
 const body = async () => {let value="";for await(const chunk of req)value+=chunk;return value;};
 if(url.pathname==="/")return send("text/html",index());
 if(url.pathname==="/source/")return send("text/html",planPage("plan-one","1".repeat(64)));
 if(url.pathname==="/source-other/")return send("text/html",planPage("plan-two","2".repeat(64)));
 if(url.pathname===legacy.url)return send("text/html",creation(legacy));
 if([...operations.values()].some(record=>record.state==="ready"&&record.canonicalUrl===url.pathname))return send("text/html","<!doctype html><h1>Created custom session</h1>");
 if(url.pathname.startsWith("/creations/")) {
  const record=operations.get(url.pathname.split("/")[2]);
  return record ? send("text/html",creation(record)) : json({},404);
 }
 if(url.pathname.startsWith("/static/"))return send(url.pathname.endsWith(".css") ? "text/css" : "text/javascript",fs.readFileSync(path.join(root,"portal/internal/web/static",path.basename(url.pathname))));
 if(url.pathname.startsWith("/codex/assets/"))return send(url.pathname.endsWith(".css") ? "text/css" : "text/javascript",fs.readFileSync(path.join(provider,"conversation/assets",path.basename(url.pathname))));
 if(url.pathname==="/sessions") {
  const raw=await body(),fields=Object.fromEntries(new URLSearchParams(raw));submissions.push({method:req.method,raw,fields});
  if(failure==="network-before")return req.socket.destroy();
  if(failure==="http")return json({error:"Temporary initialization service failure"},503);
  if(failure==="stale")return json({error:"catalog digest is stale; reload the form"},409);
  const requestId=fields.clientRequestId;
  const record={requestId,receiptId:createHash("sha256").update(requestId).digest("hex"),attempt:1,url:`/creations/${requestId}/`,
   state:"running",phase:"naming",initialRequest:fields.goal,startedAt:new Date().toISOString()};
  operations.set(requestId,record);
  if(failure==="network-after")return req.socket.destroy();
  return json(record,202);
 }
 if(url.pathname.startsWith("/api/session-creations/")) {
  const id=url.pathname.split("/")[3],record=operations.get(id);
  if(!record)return json({},404);
  if(url.pathname.endsWith("/retry")) {
   const offered=JSON.parse(await body());retries.push(offered);
   assert.deepEqual(offered,{receiptId:record.receiptId,attempt:record.attempt});
   record.attempt++;record.state="running";record.phase="naming";
  } else statusReads.push(id);
  return json(record);
 }
 if(url.pathname.endsWith("/creation/retry")) {retries.push(JSON.parse(await body()));legacy.attempt++;return json(legacy);}
 if(url.pathname.endsWith("/creation"))return json(legacy);
 if(url.pathname==="/api/models")return modelsUnavailable ? json({error:"Unavailable"},503) : json([
  {model:"test-model",displayName:"Test model",defaultReasoningEffort:"xhigh",supportedReasoningEfforts:[{reasoningEffort:"xhigh"}]},
  {model:"alternate-model",displayName:"Alternate model",defaultReasoningEffort:"xhigh",supportedReasoningEfforts:[{reasoningEffort:"xhigh"}]},
 ]);
 if(url.pathname==="/api/upload-drafts") {
  const id=randomUUID();scopes.set(id,[]);return json({id,url:`/uploads/d-${id}`},201);
 }
 if(url.pathname.startsWith("/uploads/d-")) {
  const [part,fileID]=url.pathname.slice("/uploads/d-".length).split("/"),files=scopes.get(part);
  if(!files)return json({},404);
  if(req.method==="GET")return fileID ? json(files.find(file=>file.id===fileID)) : json({files,limits});
  if(req.method==="POST"&&!fileID) {
   const input=JSON.parse(await body()),file={...input,id:randomUUID(),state:"uploading",offset:0,checksums:[]};files.push(file);return json(file,201);
  }
  const file=files.find(file=>file.id===fileID);
  if(req.method==="PATCH") {const chunk=await body();file.offset+=Buffer.byteLength(chunk);file.checksums.push(req.headers["upload-checksum"]);return json(file);}
  if(req.method==="POST") {if(holdCompletions)await new Promise(resolve=>completionWaiters.push(resolve));file.state="ready";return json(file);}
  if(req.method==="DELETE") {if(!removableScopes.has(part))return json({error:"No fixture deletion authorized"},409);files.splice(files.indexOf(file),1);return json({});}
 }
 if(url.pathname==="/uploads/s-source" && req.method==="GET")return json({files:conversationFiles,limits});
 if(url.pathname==="/api/sessions/source/operation")return json({state:"idle"});
 if(url.pathname==="/api/sessions/source/thread")return;
 return json({error:"Fixture endpoint unavailable"},503);
});
(async () => {
 await new Promise(resolve=>server.listen(0,"127.0.0.1",resolve));
 const origin="http://127.0.0.1:"+server.address().port;
 const browser=await chromium.launch({executablePath:process.env.CHROMIUM_EXECUTABLE,headless:true,args:["--no-sandbox"]});
 try {
  const context=await browser.newContext(),page=await context.newPage(),errors=[];
  page.on("pageerror",error=>errors.push(error.message));
  await context.addInitScript(()=>{
   const originalFetch=window.fetch;
   window.sessionPostFetches=[];
   window.fetch=function(input,options){
    const url=new URL(typeof input==="string"||input instanceof URL ? input : input.url,window.location.href);
    const method=String(options?.method||(input instanceof Request ? input.method : "GET")).toUpperCase();
    if(url.origin===window.location.origin&&url.pathname==="/sessions"&&method==="POST") {
     window.sessionPostFetches.push(Object.freeze({method,body:String(options?.body??"")}));
    }
    return originalFetch.apply(this,arguments);
   };
  });
  await context.addInitScript(()=>Object.defineProperty(navigator,"clipboard",{value:{writeText:async text=>{window.copiedText=text;}}}));
  const ready=page=>page.waitForFunction(()=>!document.querySelector('#new-session-form button[type="submit"]').disabled);
  const draft=page=>page.evaluate(()=>JSON.parse(sessionStorage.getItem("workspace-portal.creation-draft")));
  await page.goto(origin);await ready(page);
  assert.equal(await page.locator('[name="name"]').evaluate(input=>input.required),false);
  assert.match(await page.locator('[data-cli-command]').inputValue(),/custom short name/);
  await page.locator('[name="goal"]').fill(prompt);
  const before=await draft(page),beforeCount=submissions.length;
  const beforeFetchCount=await page.evaluate(()=>window.sessionPostFetches.length);
  assert.match(before.requestId,/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab]/);
  // Rapid duplicate events make one application fetch. Chromium may replay
  // that fetch after TCP loss; every wire attempt must keep its frozen identity.
  await page.evaluate(()=>{const form=document.querySelector("#new-session-form");for(let n=0;n<2;n++)form.dispatchEvent(new SubmitEvent("submit",{cancelable:true}));});
  await page.waitForFunction(()=>!document.querySelector("#new-session-recovery").hidden&&!document.querySelector("#new-session-recover").disabled);
  const applicationPosts=await page.evaluate(()=>window.sessionPostFetches);
  assert.equal(applicationPosts.length,beforeFetchCount+1);
  const frozen=(await draft(page)).body;
  assert.deepEqual(applicationPosts.at(-1),{method:"POST",body:frozen});
  const wirePosts=submissions.slice(beforeCount);
  assert(wirePosts.length>0,"The application submission must reach the TCP-loss fixture");
  assert(wirePosts.every(item=>item.method==="POST"&&item.raw===frozen&&item.fields.clientRequestId===before.requestId));
  assert.equal(submissions.at(-1).fields.name,"");
  assert.equal(submissions.at(-1).fields.model,"");assert.equal(submissions.at(-1).fields.effort,"");
  assert(await page.locator('[name="goal"]').isDisabled());
  catalog.current="b".repeat(64);
  await page.reload();await page.waitForFunction(()=>!document.querySelector("#new-session-recovery").hidden&&!document.querySelector("#new-session-recover").disabled);
  assert.equal((await draft(page)).body,frozen);
  assert.equal(await page.locator('[name="catalogDigest"]').inputValue(),"a".repeat(64));
  assert(await page.locator('[data-team-catalog-changed]').isHidden());
  assert(await page.locator('[name="goal"]').isDisabled());
  failure="none";await page.getByRole("button",{name:"Recover saved request"}).click();
  await page.waitForURL(origin+`/creations/${before.requestId}/`);
  assert(submissions.slice(beforeCount).every(item=>item.method==="POST"&&item.raw===frozen&&item.fields.clientRequestId===before.requestId));
  assert.equal(await page.locator("#creation-request").textContent(),prompt);
  assert(!await page.evaluate(()=>window.promptInjected));
  assert.equal(await page.evaluate(()=>sessionStorage.getItem("workspace-portal.creation-draft")),null);
  await page.getByRole("button",{name:"Copy initial request"}).click();assert.equal(await page.evaluate(()=>window.copiedText),prompt);
  const record=operations.get(before.requestId),acceptedAt=record.startedAt;
  record.state="failed";record.phase="stopped";record.error='<img src=x onerror="window.errorInjected=true">';
  await page.waitForFunction(()=>!document.querySelector("#creation-retry").hidden);
  await page.getByRole("button",{name:"Retry creation"}).click();
  await page.waitForFunction(()=>document.querySelector("#creation-phase").textContent.includes("Choosing"));
  assert.deepEqual(retries.at(-1),{receiptId:record.receiptId,attempt:1});assert.equal(record.startedAt,acceptedAt);
  assert(!await page.evaluate(()=>window.errorInjected));
  // Ready with an unsafe URL cannot navigate, nor can terminal replaced state.
  record.state="ready";record.slug="2026-09-13-example";record.canonicalUrl="https://evil.test/";
  await page.waitForFunction(()=>document.querySelector("#creation-error").textContent.includes("destination"));
  assert.equal(new URL(page.url()).pathname,record.url);
  record.state="conflict";record.canonicalUrl="/2026-09-13-example/";
  await page.waitForFunction(()=>document.querySelector("#creation-title").textContent.includes("attention"));
  assert.equal(new URL(page.url()).pathname,record.url);
  // Two fresh tabs share an origin but allocate distinct request/file scopes.
  const a=await context.newPage(),b=await context.newPage();
  await a.goto(origin);await b.goto(origin);await ready(a);await ready(b);
  assert.notEqual((await draft(a)).requestId,(await draft(b)).requestId);
  assert.notEqual((await draft(a)).scope.id,(await draft(b)).scope.id);
  await a.evaluate(()=>localStorage.setItem("workspace-portal.creation-upload-scope",JSON.stringify({id:"00000000-0000-4000-8000-000000000099",url:"/uploads/d-00000000-0000-4000-8000-000000000099"})));
  const fresh=await context.newPage();await fresh.goto(origin);await ready(fresh);
  assert.notEqual((await draft(fresh)).scope.id,"00000000-0000-4000-8000-000000000099");
  assert(await fresh.locator("#creation-draft-notice").isHidden());
  // Actual upload component permits a request containing only an attachment.
  await a.locator('input[type="file"]').setInputFiles({name:"fixture.txt",mimeType:"text/plain",buffer:Buffer.from("attachment")});
  await a.waitForFunction(()=>!document.querySelector('[name="goal"]').required&&!document.querySelector('button[type="submit"]').disabled);
  const attachmentScope=(await draft(a)).scope.id;
  await a.getByRole("button",{name:"Create session"}).click();await a.waitForURL(/\/creations\//);
  assert.equal(submissions.at(-1).fields.goal,"");assert.equal(submissions.at(-1).fields.uploadScope,attachmentScope);
  assert(submissions.at(-1).fields.attachmentIds);
  // Lost acceptance response: status recovery observes it without another POST.
  failure="network-after";await b.locator('[name="goal"]').fill("Lost accepted response");
  const acceptedID=(await draft(b)).requestId;
  await b.getByRole("button",{name:"Create session"}).click();
  await b.waitForFunction(()=>!document.querySelector("#new-session-recovery").hidden&&!document.querySelector("#new-session-recover").disabled);
  const acceptedPosts=submissions.length;await b.reload();await b.waitForURL(origin+`/creations/${acceptedID}/`);
  assert.equal(submissions.length,acceptedPosts);assert(statusReads.includes(acceptedID));
  // Stale rejection and an ambiguous accepted response both keep the old
  // identity recoverable while the explicit route opens a current, separate tab.
  for (const outcome of ["stale","network-after"]) {
   failure=outcome;
   const original=await context.newPage();await original.goto(origin);await ready(original);
   await original.locator('[name="goal"]').fill(prompt);
   await original.locator('input[type="file"]').setInputFiles({name:"keep-original.txt",mimeType:"text/plain",buffer:Buffer.from("retained original")});
   await original.waitForFunction(()=>!document.querySelector('#new-session-form button[type="submit"]').disabled);
   await original.getByRole("button",{name:"Create session"}).click();
   await original.waitForFunction(()=>!document.querySelector('#new-session-separate').hidden&&!document.querySelector('#new-session-recover').disabled);
   const old=await draft(original),oldSelection=await original.evaluate(id=>sessionStorage.getItem(`workspace-portal.upload-draft.${id}`),old.scope.id);
   catalog.current=outcome==="stale" ? "c".repeat(64) : "d".repeat(64);
   if(outcome==="stale") {
    await original.reload();await original.waitForFunction(()=>!document.querySelector('#new-session-separate').hidden&&!document.querySelector('#new-session-recover').disabled);
    assert.deepEqual(await draft(original),old);
   }
   await original.getByRole("button",{name:"Copy saved text"}).click();
   assert.equal(await original.evaluate(()=>window.copiedText),prompt);
   const count=submissions.length;
   const opened=context.waitForEvent("page");
   await original.getByRole("link",{name:"Start a separate request"}).click();
   const separate=await opened;await separate.waitForLoadState();await ready(separate);
   assert.equal(await separate.evaluate(()=>window.opener),null);
   assert.equal(new URL(separate.url()).search,"");
   const fresh=await draft(separate);
   assert.notEqual(fresh.requestId,old.requestId);assert.notEqual(fresh.scope.id,old.scope.id);
   assert.equal(fresh.goal,"");assert.equal(fresh.catalogDigest,catalog.current);
   assert.equal(fresh.body,null);assert.deepEqual(fresh.attachmentIds,[]);
   assert.equal(scopes.get(fresh.scope.id).length,0);assert.equal(submissions.length,count);
   assert.deepEqual(await draft(original),old);
   assert.equal(await original.evaluate(id=>sessionStorage.getItem(`workspace-portal.upload-draft.${id}`),old.scope.id),oldSelection);
   assert.equal(scopes.get(old.scope.id).length,1);
   // A delayed old acceptance is installed only in the original request page.
   failure="";
   if(!operations.has(old.requestId)) operations.set(old.requestId,{requestId:old.requestId,receiptId:createHash("sha256").update(old.requestId).digest("hex"),attempt:1,url:`/creations/${old.requestId}/`,state:"running",phase:"naming",initialRequest:prompt,startedAt:new Date().toISOString()});
   const oldRecord=operations.get(old.requestId);
   await original.getByRole("button",{name:"Recover saved request"}).click();await original.waitForURL(origin+oldRecord.url);
   assert.equal(submissions.length,count);assert.deepEqual(await draft(separate),fresh);
   oldRecord.state="ready";oldRecord.slug="2026-09-13-original-"+outcome;oldRecord.canonicalUrl=`/${oldRecord.slug}/`;
   await original.waitForURL(origin+oldRecord.canonicalUrl);
   assert.equal(new URL(separate.url()).pathname,"/");assert.deepEqual(await draft(separate),fresh);
   // Reattachment to the new scope is deliberate and leaves the old files.
   await separate.locator('input[type="file"]').setInputFiles({name:"keep-original.txt",mimeType:"text/plain",buffer:Buffer.from("retained original")});
   await separate.waitForFunction(()=>!document.querySelector('#new-session-form button[type="submit"]').disabled);
   assert.equal(submissions.length,count);assert.equal(scopes.get(fresh.scope.id).length,1);assert.equal(scopes.get(old.scope.id).length,1);
   await original.close();await separate.close();
  }
  // Unsubmitted legacy draft can change catalogs after explicit acknowledgement.
  const legacyCatalog=catalog.current;
  modelsUnavailable=false;
  await page.goto(origin);await ready(page);
  await page.evaluate(({prompt})=>{
   sessionStorage.setItem("workspace-portal.creation-draft",JSON.stringify({name:"My_custom-Work",goal:prompt,date:"2026-09-13",team:"delivery",catalogDigest:"a".repeat(64),model:"alternate-model",effort:"xhigh"}));
   localStorage.setItem("workspace-portal.upload-draft.00000000-0000-4000-8000-000000000099",JSON.stringify([{name:"previous.txt"}]));
  },{prompt});
  await page.reload();await page.waitForFunction(()=>!document.querySelector('[data-team-catalog-changed]').hidden);
  assert(await page.getByRole("button",{name:"Create session"}).isDisabled());
  assert(await page.locator("#creation-draft-notice").isVisible());
  assert.equal((await draft(page)).migratedUploads,true);
  await page.locator('[data-team-catalog-acknowledge]').check();await ready(page);
  assert.equal((await draft(page)).catalogDigest,legacyCatalog);
  await page.getByText("Options",{exact:true}).click();
  await page.locator('[name="model"]').selectOption("alternate-model");await page.locator('[name="effort"]').selectOption("xhigh");
  modelsUnavailable=true;await page.reload();await ready(page);
  assert.equal(await page.locator('[name="model"]').inputValue(),"alternate-model");
  assert.equal(await page.locator('[name="effort"]').inputValue(),"xhigh");
  assert(await page.locator("#creation-draft-notice").isVisible());
  await page.getByText("Options",{exact:true}).click();
  assert.match(await page.locator('[data-cli-command]').inputValue(),/dev-session start 'My_custom-Work'/);
  failure="none";await page.getByRole("button",{name:"Create session"}).click();await page.waitForURL(/\/creations\//);
  assert.equal(submissions.at(-1).fields.name,"My_custom-Work");assert.equal(submissions.at(-1).fields.model,"alternate-model");
  const custom=operations.get(submissions.at(-1).fields.clientRequestId);
  custom.state="ready";custom.slug="2026-09-13-My_custom-Work";custom.canonicalUrl="/2026-09-13-My_custom-Work/";
  await page.waitForURL(origin+custom.canonicalUrl);
  // Storage refusal admits no request, even if a submit event bypasses HTML.
  const blocked=await context.newPage();await blocked.addInitScript(()=>{Storage.prototype.setItem=function(){throw Error("storage disabled");};});
  const count=submissions.length;await blocked.goto(origin);
  await blocked.waitForFunction(()=>document.querySelector("#new-session-progress").textContent.includes("storage"));
  await blocked.evaluate(()=>document.querySelector("#new-session-form").dispatchEvent(new SubmitEvent("submit",{cancelable:true})));
  assert.equal(submissions.length,count);assert(await blocked.getByRole("button",{name:"Create session"}).isDisabled());
  // Confirmed acceptance still cannot navigate through unverified removal.
  const cleanup=await context.newPage();
  await cleanup.addInitScript(()=>{window.restoreRemove=Storage.prototype.removeItem;Storage.prototype.removeItem=function(){};});
  await cleanup.goto(origin);await ready(cleanup);await cleanup.locator('[name="goal"]').fill("Storage cleanup failure");
  await cleanup.getByRole("button",{name:"Create session"}).click();
  await cleanup.waitForFunction(()=>document.querySelector("#new-session-progress").textContent.includes("storage")&&!document.querySelector("#new-session-recover").disabled);
  assert.equal(new URL(cleanup.url()).pathname,"/");
  const cleanupID=(await draft(cleanup)).requestId,cleanupPosts=submissions.length;
  assert((await draft(cleanup)).accepted);
  await cleanup.evaluate(()=>{Storage.prototype.removeItem=window.restoreRemove;});
  await cleanup.getByRole("button",{name:"Recover saved request"}).click();
  await cleanup.waitForURL(origin+`/creations/${cleanupID}/`);assert.equal(submissions.length,cleanupPosts);

  // Load the actual host/provider CSS in production order. The existing page
  // scroll container owns the expanded creation list, including narrow layouts.
  for(const width of [1280,375]) {
   const layout=await context.newPage();layout.on("pageerror",error=>errors.push(error.message));
   await layout.setViewportSize({width,height:800});await layout.goto(origin);await ready(layout);
   assert(await layout.locator("#creation-uploads").isHidden());
   const files=Array.from({length:50},(_,i)=>({name:`file-${String(i).padStart(2,"0")}-${"long-name-".repeat(20)}.txt`,mimeType:"text/plain",buffer:Buffer.alloc(1024,65)}));
   const selection=(await draft(layout)).scope.id;removableScopes.add(selection);
   holdCompletions=true;
   await layout.locator('input[type="file"]').setInputFiles(files);
   await layout.waitForFunction(()=>document.querySelector("#creation-uploads .codex-upload-summary")?.textContent==="50 files · 50 KiB total · 0 of 50 complete");
   assert(await layout.getByRole("button",{name:"Create session"}).isDisabled());
   await layout.waitForFunction(()=>[...document.querySelectorAll("#creation-uploads progress")].some(progress=>progress.value===progress.max));
   holdCompletions=false;completionWaiters.splice(0).forEach(resolve=>resolve());
   await layout.waitForFunction(()=>document.querySelector("#creation-uploads .codex-upload-summary")?.textContent==="50 files · 50 KiB total"&&!document.querySelector('#new-session-form button[type="submit"]').disabled);
   const dimensions=await layout.locator("#creation-uploads").evaluate(root=>({maxHeight:getComputedStyle(root).maxHeight,overflowY:getComputedStyle(root).overflowY,clientHeight:root.clientHeight,scrollHeight:root.scrollHeight}));
   assert.equal(dimensions.maxHeight,"none");assert.equal(dimensions.overflowY,"visible");
   assert(dimensions.clientHeight>192);assert(dimensions.scrollHeight<=dimensions.clientHeight+1);
   assert(await layout.locator(".index-page").evaluate(page=>page.scrollHeight>page.clientHeight));
   assert.equal(await layout.locator("#creation-uploads .codex-attachment").count(),50);
   const removals=layout.locator('#creation-uploads button[aria-label^="Remove "]');
   assert.equal(await removals.count(),50);
   for(let i=0;i<50;i++) {
    const remove=removals.nth(i);await remove.scrollIntoViewIfNeeded();
    const box=await remove.boundingBox();assert(box&&box.y>=0&&box.y+box.height<=800,`unreachable file action ${i} at width ${width}`);
    assert(await remove.isEnabled());
   }
   const submit=layout.getByRole("button",{name:"Create session"});await submit.scrollIntoViewIfNeeded();
   const submitBox=await submit.boundingBox();assert(submitBox&&submitBox.y>=0&&submitBox.y+submitBox.height<=800);
   await layout.locator('input[type="file"]').setInputFiles({name:"file-51.txt",mimeType:"text/plain",buffer:Buffer.alloc(1024)});
   assert.match(await layout.locator("#creation-uploads .codex-upload-notice").textContent(),/exceeds/);
   assert.equal(await layout.locator("#creation-uploads .codex-upload-summary").textContent(),"50 files · 50 KiB total");
   assert.equal(scopes.get(selection).length,50);
   await removals.first().click();
   await layout.waitForFunction(()=>document.querySelector("#creation-uploads .codex-upload-summary")?.textContent==="49 files · 49 KiB total");
   await layout.locator('input[type="file"]').setInputFiles(files[0]);
   await layout.waitForFunction(()=>document.querySelector("#creation-uploads .codex-upload-summary")?.textContent==="50 files · 50 KiB total");
   await layout.reload();await ready(layout);
   assert.equal(await layout.locator("#creation-uploads .codex-upload-summary").textContent(),"50 files · 50 KiB total");
   failure="network-before";await layout.getByRole("button",{name:"Create session"}).click();
   await layout.waitForFunction(()=>!document.querySelector("#new-session-recovery").hidden&&!document.querySelector("#new-session-recover").disabled);
   const attempted=await draft(layout);assert.equal(attempted.attachmentIds.length,50);
   assert.equal(new URLSearchParams(attempted.body).getAll("attachmentIds").length,50);
   failure="none";await layout.reload();await layout.waitForURL(origin+`/creations/${attempted.requestId}/`);
   const attempts=submissions.filter(submission=>submission.fields.clientRequestId===attempted.requestId);
   assert(attempts.length>=2);assert(attempts.every(submission=>submission.raw===attempted.body));
   assert(statusReads.includes(attempted.requestId));
   assert.equal(await draft(layout),null);
   assert.equal(await layout.evaluate(id=>sessionStorage.getItem(`workspace-portal.upload-draft.${id}`),selection),null);
   assert.equal(scopes.get(selection).length,50);await layout.close();

   const conversation=await context.newPage();conversation.on("pageerror",error=>errors.push(error.message));
   await conversation.setViewportSize({width,height:800});
   await conversation.addInitScript(files=>localStorage.setItem("workspace-portal.upload-draft.source.thread-1",JSON.stringify(files)),conversationFiles);
   await conversation.goto(origin+"/source/");
   await conversation.waitForFunction(()=>document.querySelectorAll("#message-uploads .codex-attachment").length===50);
   const capped=await conversation.locator("#message-uploads").evaluate(root=>({maxHeight:getComputedStyle(root).maxHeight,overflowY:getComputedStyle(root).overflowY,clientHeight:root.clientHeight,scrollHeight:root.scrollHeight,rem:parseFloat(getComputedStyle(document.documentElement).fontSize)}));
   assert.equal(capped.maxHeight,`${12*capped.rem}px`);assert.equal(capped.overflowY,"auto");
   assert(capped.clientHeight<=12*capped.rem+1);assert(capped.scrollHeight>capped.clientHeight);
   assert.equal(await conversation.locator("#message-uploads .codex-upload-summary").textContent(),"50 files · 50 KiB total");
   await conversation.close();
  }
  modelsUnavailable=false;
  // Plan drafts use an immutable plan identity, so unrelated plan proposals
  // never restore or overwrite this selected team and lead override.
  await page.goto(origin+"/source/"); await page.waitForFunction(()=>document.querySelector('#plan-session-form [name="model"] option[value="test-model"]'));
  await page.getByRole("button",{name:"Implement in a new session"}).click();
  await page.waitForFunction(()=>document.querySelector("#plan-session-dialog").open);
  const planForm = page.locator("#plan-session-form");
  await planForm.locator('[name="name"]').fill("plan-one");
  await planForm.locator('[name="team"]').selectOption("delivery");
  await planForm.locator('[name="model"]').selectOption("alternate-model"); await planForm.locator('[name="effort"]').selectOption("xhigh");
  await page.reload(); await page.waitForFunction(()=>document.querySelector('#plan-session-form [name="model"] option[value="alternate-model"]'));
  await page.getByRole("button",{name:"Implement in a new session"}).click();
  await page.waitForFunction(()=>document.querySelector("#plan-session-dialog").open && document.querySelector('#plan-session-form [name="effort"]').value === "xhigh");
  assert.equal(await planForm.locator('[name="name"]').inputValue(),"plan-one");
  assert.equal(await planForm.locator('[name="team"]').inputValue(),"delivery");
  assert.equal(await planForm.locator('[name="model"]').inputValue(),"alternate-model");
  await page.goto(origin+"/source-other/"); await page.waitForFunction(()=>document.querySelector('#plan-session-form [name="model"] option[value="test-model"]'));
  await page.getByRole("button",{name:"Implement in a new session"}).click();
  await page.waitForFunction(()=>document.querySelector("#plan-session-dialog").open);
  assert.equal(await page.locator('#plan-session-form [name="name"]').inputValue(),"");
  assert.equal(await page.locator('#plan-session-form [name="team"]').inputValue(),"solo");
  assert.equal(await page.locator('#plan-session-form [name="model"]').inputValue(),"");
  await page.locator('#plan-session-form [name="name"]').fill("plan-two");
  await page.locator('#plan-session-form [name="team"]').selectOption("solo");
  assert.equal(await page.evaluate(() => Object.keys(sessionStorage).filter((key) => (
    key.startsWith("workspace-portal.plan-session-draft.source.")
  )).length),2);
  await page.goto(origin+"/source/"); await page.waitForFunction(()=>document.querySelector('#plan-session-form [name="model"] option[value="alternate-model"]'));
  await page.getByRole("button",{name:"Implement in a new session"}).click();
  await page.waitForFunction(()=>document.querySelector("#plan-session-dialog").open);
  assert.equal(await page.locator('#plan-session-form [name="name"]').inputValue(),"plan-one");


  // Legacy slug-based creation and plan cancellation retain their routes.
  await page.goto(origin+legacy.url);
  await page.waitForFunction(()=>!document.querySelector("#creation-retry").hidden);
  await page.getByRole("button",{name:"Retry initialization"}).click();
  await page.waitForFunction(()=>!document.querySelector("#creation-retry").disabled);
  assert.deepEqual(retries.at(-1),{receiptId:"legacy-receipt",attempt:1});
  legacy.state="running";legacy.error="";legacy.phase="Initializing team member architect0…";
  await page.waitForFunction(()=>document.querySelector("#creation-phase").textContent.includes("architect0"));
  legacy.state="cancelled";legacy.error="Submit the current plan again.";legacy.sourceUrl="/source/";
  await page.reload();await page.waitForFunction(()=>document.querySelector("#creation-title").textContent==="Session was not created");
  assert.equal(new URL(page.url()).pathname,legacy.url);
  assert.equal(await page.locator("#creation-source").getAttribute("href"),"/source/");
  assert(await page.locator("#creation-retry").isHidden());assert(await page.locator("#creation-leave-note").isHidden());
  assert.deepEqual(errors,[]);
  console.log("Creation browser acceptance passed: generated/custom names, immutable 50-file recovery, desktop/narrow creation CSS, conversation cap, full-size summary, 51-file rejection, tab scopes, attachment-only, storage, progress and legacy plans/receipts");
 } finally {await browser.close();server.close();}
})().catch(error=>{console.error(error);server.close();process.exitCode=1;});
