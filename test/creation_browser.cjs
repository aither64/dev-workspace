// Browser acceptance for managed creation drafts and saved initialization requests.
// Use the same PLAYWRIGHT_MODULE and CHROMIUM_EXECUTABLE as repository_browser.cjs.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE);
const http = require("node:http"), fs = require("node:fs"), path = require("node:path"), assert = require("node:assert/strict");
const root = process.cwd(), provider = process.env.CODEX_WEB_SOURCE;
const prompt = "Keep this request <script>literal</script>\nSecond line.";
const catalog = {current: "a".repeat(64)};
let failure = "http", submissions = [], retries = [];
const receipt = {slug:"2026-09-13-example", url:"/2026-09-13-example/", receiptId:"receipt", attempt:1, state:"failed", phase:"Initialization stopped", error:"context deadline exceeded", initialRequest:prompt, startedAt:new Date().toISOString()};

const managedPolicy = (digest) => `<input type="hidden" name="catalogDigest" value="${digest}">
  <label>Starting team<select name="team" data-team-select required><option value="solo" selected data-description="Solo team" data-roles="team lead">solo</option><option value="delivery" data-description="Delivery team" data-roles="team lead, implementer">delivery</option></select></label>
  <p data-team-description></p>
  <p data-team-catalog-changed role="status" aria-live="polite" hidden>The team catalog changed. Review the team and lead settings, then confirm before creating this session.</p>
  <label data-team-catalog-acknowledgement hidden><input type="checkbox" data-team-catalog-acknowledge> I have reviewed the current team and lead settings.</label>
  <label>Lead model<select name="model" data-model-select data-managed-lead-override><option value="">Selected team default</option></select></label>
  <label>Lead reasoning effort<select name="effort" data-effort-select><option value="">Selected team default</option></select></label>`;
const index = () => `<!doctype html><body data-index><form id="new-session-form"><input name="creation_date" value="2026-09-13"><input name="name" required><textarea name="goal" required></textarea>${managedPolicy(catalog.current)}<input name="uploadScope"><div id="creation-uploads"></div><span id="creation-upload-controls"></span><p id="new-session-progress" hidden></p><button type="submit">Create session</button></form><script src="/static/app.js"></script>`;
const planPage = (turnID, digest) => `<!doctype html><body data-session="source" data-thread-id="thread-1" data-interactive="true"><div id="conversation-connection"></div><div id="codex-status"></div><div id="transcript"></div><div id="pending"></div><button id="new-output" type="button" hidden>New output</button><section id="plan-actions" data-plan-turn-id="${turnID}" data-plan-sha256="${digest}"><button id="plan-implement-new" type="button">Implement in a new session</button></section><form id="message-form"><textarea name="message"></textarea><div id="message-uploads"></div><span id="message-upload-controls"></span><button id="message-send" type="submit">Send</button><button id="message-queue" type="button"></button><button id="interrupt" type="button">Interrupt</button></form><dialog id="plan-session-dialog"><form id="plan-session-form"><input name="creationDate" value="2026-09-13"><input name="name" required>${managedPolicy(catalog.current)}<p id="plan-session-progress" hidden></p><button type="submit">Create session</button></form></dialog><script>document.getElementById("plan-actions").planText = "Approved plan";</script><script src="/static/app.js"></script>`;
const creation = `<!doctype html><body data-creation="2026-09-13-example"><h1 id="creation-title"></h1><p id="creation-phase"></p><p id="creation-error"></p><button id="creation-retry">Retry initialization</button><p id="creation-elapsed"></p><p><a id="creation-source" hidden>Return to the source session</a></p><p id="creation-leave-note">You can leave this page while initialization continues.</p><section id="creation-request-panel"><span id="creation-request-copy"></span><pre id="creation-request"></pre></section><script src="/static/creation.js"></script>`;
const server = http.createServer(async (req,res) => {
 const url = new URL(req.url,"http://fixture");
 const send = (type,data,status=200) => {res.writeHead(status,{"Content-Type":type});res.end(data);};
 const json = (data,status=200) => send("application/json",JSON.stringify(data),status);
 if (url.pathname === "/") return send("text/html",index());
 if (url.pathname === "/source/") return send("text/html",planPage("plan-one", "1".repeat(64)));
 if (url.pathname === "/source-other/") return send("text/html",planPage("plan-two", "2".repeat(64)));
 if (url.pathname === receipt.url) return send("text/html",creation);
 if (url.pathname.startsWith("/static/")) return send("text/javascript",fs.readFileSync(path.join(root,"portal/internal/web/static",path.basename(url.pathname))));
 if (url.pathname.startsWith("/codex/assets/")) return send("text/javascript",fs.readFileSync(path.join(provider,"conversation/assets",path.basename(url.pathname))));
 if (url.pathname === "/sessions") {
  let body=""; for await (const chunk of req) body += chunk;
  submissions.push(Object.fromEntries(new URLSearchParams(body)));
  if (failure === "network") return req.socket.destroy();
  if (failure === "http") return json({error:"Temporary initialization service failure"},503);
  return json(receipt,202);
 }
 if (url.pathname.endsWith("/creation/retry")) {
  let body=""; for await (const chunk of req) body += chunk;
  retries.push(JSON.parse(body)); receipt.attempt++; return json(receipt);
 }
 if (url.pathname.endsWith("/creation")) return json(receipt);
 if (url.pathname === "/api/models") return json([
  {model:"test-model",displayName:"Test model",supportedReasoningEfforts:[{reasoningEffort:"xhigh"}]},
  {model:"alternate-model",displayName:"Alternate model",supportedReasoningEfforts:[{reasoningEffort:"xhigh"}]},
 ]);
 if (url.pathname === "/api/sessions/source/operation") return json({state:"idle"});
 // Keep the fixture's conversation transport pending. The creation dialog is
 // intentionally independent of live transcript rendering and this avoids a
 // mock session protocol becoming an authority for managed creation policy.
 if (url.pathname === "/api/sessions/source/thread") return;
 return json({error:"Fixture endpoint unavailable"},503);
});
(async () => {
 await new Promise(resolve => server.listen(0,"127.0.0.1",resolve));
 const origin = "http://127.0.0.1:"+server.address().port;
 const browser = await chromium.launch({executablePath:process.env.CHROMIUM_EXECUTABLE,headless:true,args:["--no-sandbox"]});
 try {
  const page = await browser.newPage(); const errors=[]; page.on("pageerror",e=>errors.push(e.message));
  await page.addInitScript(() => Object.defineProperty(navigator,"clipboard",{value:{writeText:async text=>{window.copiedText=text;}}}));
  await page.goto(origin); await page.waitForFunction(()=>document.querySelector('[name="model"] option[value="test-model"]'));
  await page.locator('[name="name"]').fill("example"); await page.locator('[name="goal"]').fill(prompt);
  await page.locator('[name="team"]').selectOption("delivery");
  await page.locator('[name="model"]').selectOption("test-model"); await page.locator('[name="effort"]').selectOption("xhigh");
  await page.getByRole("button",{name:"Create session"}).click();
  await page.waitForFunction(()=>document.querySelector("#new-session-progress").textContent.includes("Temporary initialization"));
  assert.equal(await page.locator('[name="goal"]').inputValue(),prompt);
  await page.reload(); await page.waitForFunction(()=>document.querySelector('[name="effort"]').value==="xhigh");
  assert.equal(await page.locator('[name="team"]').inputValue(),"delivery");
  assert.equal(await page.locator('[name="model"]').inputValue(),"test-model");
  assert.equal(await page.locator('[name="catalogDigest"]').inputValue(),"a".repeat(64));
  failure="network"; await page.getByRole("button",{name:"Create session"}).click();
  await page.waitForFunction(()=>!document.querySelector('button[type="submit"]').disabled);
  await page.reload(); await page.waitForFunction(()=>document.querySelector('[name="effort"]').value==="xhigh");
  assert.equal(await page.locator('[name="team"]').inputValue(),"delivery");

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

  // A replacement catalog preserves the text fields, but neither replays a
  // saved team nor silently combines its overrides with the new digest.
  catalog.current = "b".repeat(64);
  await page.goto(origin); await page.waitForFunction(()=>!document.querySelector("[data-team-catalog-changed]").hidden);
  assert.equal(await page.locator("[data-team-catalog-changed]").getAttribute("role"),"status");
  assert.equal(await page.locator("[data-team-catalog-changed]").getAttribute("aria-live"),"polite");
  assert.equal(await page.locator('[name="name"]').inputValue(),"example");
  assert.equal(await page.locator('[name="goal"]').inputValue(),prompt);
  assert.equal(await page.locator('[name="creation_date"]').inputValue(),"2026-09-13");
  assert.equal(await page.locator('[name="team"]').inputValue(),"solo");
  assert.equal(await page.locator('[name="model"]').inputValue(),"");
  assert.equal(await page.locator('[name="effort"]').inputValue(),"");
  assert(await page.getByRole("button",{name:"Create session"}).isDisabled());
  const beforeBlockedSubmission = submissions.length;
  await page.evaluate(() => document.querySelector("#new-session-form").dispatchEvent(new SubmitEvent("submit", {cancelable:true, submitter:document.querySelector('button[type="submit"]')})));
  await page.waitForTimeout(25);
  assert.equal(submissions.length,beforeBlockedSubmission);
  await page.locator("[data-team-catalog-acknowledge]").check();
  await page.waitForFunction(()=>!document.querySelector('button[type="submit"]').disabled);
  const acknowledgedDraft = await page.evaluate(() => JSON.parse(sessionStorage.getItem("workspace-portal.creation-draft")));
  assert.deepEqual(acknowledgedDraft,{name:"example",goal:prompt,date:"2026-09-13",model:"",effort:"",team:"solo",catalogDigest:"b".repeat(64)});
  // The stored, current digest means a reload does not replay the stale
  // catalog warning or silently restore its previous policy choices.
  await page.reload(); await page.waitForFunction(()=>document.querySelector('[name="catalogDigest"]').value === "b".repeat(64));
  assert(await page.locator("[data-team-catalog-changed]").isHidden());
  assert(!await page.getByRole("button",{name:"Create session"}).isDisabled());
  assert.equal(await page.locator('[name="name"]').inputValue(),"example");
  assert.equal(await page.locator('[name="goal"]').inputValue(),prompt);
  assert.equal(await page.locator('[name="creation_date"]').inputValue(),"2026-09-13");
  assert.equal(await page.locator('[name="team"]').inputValue(),"solo");
  assert.equal(await page.locator('[name="model"]').inputValue(),"");
  assert.equal(await page.locator('[name="effort"]').inputValue(),"");

  // Plan drafts have a distinct key and use the same acknowledgement flow.
  await page.goto(origin+"/source/"); await page.waitForFunction(()=>document.querySelector('#plan-session-form [name="model"] option[value="test-model"]'));
  await page.getByRole("button",{name:"Implement in a new session"}).click();
  await page.waitForFunction(()=>!document.querySelector("[data-team-catalog-changed]").hidden);
  assert.equal(await planForm.locator('[name="name"]').inputValue(),"plan-one");
  assert(await planForm.getByRole("button",{name:"Create session"}).isDisabled());
  await planForm.locator("[data-team-catalog-acknowledge]").check();
  await page.waitForFunction(()=>!document.querySelector('#plan-session-form button[type="submit"]').disabled);
  await page.reload(); await page.waitForFunction(()=>document.querySelector('#plan-session-form [name="model"] option[value="test-model"]'));
  await page.getByRole("button",{name:"Implement in a new session"}).click();
  await page.waitForFunction(()=>document.querySelector("#plan-session-dialog").open);
  assert(await planForm.locator("[data-team-catalog-changed]").isHidden());
  assert(!await planForm.getByRole("button",{name:"Create session"}).isDisabled());
  assert.equal(await planForm.locator('[name="name"]').inputValue(),"plan-one");
  assert.equal(await planForm.locator('[name="catalogDigest"]').inputValue(),"b".repeat(64));
  assert.equal(await planForm.locator('[name="team"]').inputValue(),"solo");
  assert.equal(await planForm.locator('[name="model"]').inputValue(),"");
  assert.equal(await planForm.locator('[name="effort"]').inputValue(),"");

  await page.goto(origin); await page.waitForFunction(()=>document.querySelector('[name="catalogDigest"]').value === "b".repeat(64));
  failure="http"; await page.getByRole("button",{name:"Create session"}).click();
  await page.waitForFunction(()=>document.querySelector("#new-session-progress").textContent.includes("Temporary initialization"));
  assert(await page.evaluate(()=>sessionStorage.getItem("workspace-portal.creation-draft")),"HTTP failure cleared managed draft");
  failure="none"; await page.getByRole("button",{name:"Create session"}).click();
  await page.waitForURL(origin+receipt.url);
  await page.waitForFunction(expected=>document.querySelector("#creation-request").textContent===expected,prompt);
  await page.getByRole("button",{name:"Copy initial request"}).click();
  assert.equal(await page.evaluate(()=>window.copiedText),prompt);
  await page.getByRole("button",{name:"Retry initialization"}).click();
  await page.waitForFunction(()=>!document.querySelector("#creation-retry").disabled);
  assert.equal(await page.locator("#creation-request").textContent(),prompt);
  assert.deepEqual(retries,[{receiptId:"receipt",attempt:1}]);
  assert(submissions.length>=4); assert(submissions.every(submission=>submission.goal===prompt));
  // The empty effort control is disabled until a lead model is selected, so
  // it is intentionally absent from native FormData: no override is sent.
  assert.deepEqual(submissions.at(-1),{creation_date:"2026-09-13",name:"example",goal:prompt,catalogDigest:"b".repeat(64),team:"solo",model:"",uploadScope:""});
  assert.equal(await page.evaluate(()=>sessionStorage.getItem("workspace-portal.creation-draft")),null);

  // A pre-effect plan failure has no destination session. Its status remains
  // visible, links back to the source, and cannot follow conflict's canonical
  // destination redirect or invite another retry.
  receipt.state="cancelled"; receipt.phase="The source plan changed before a destination session was created.";
  receipt.error="Submit the current plan again to create a new session."; receipt.sourceUrl="/source/";
  await page.goto(origin+receipt.url);
  await page.waitForFunction(()=>document.querySelector("#creation-title").textContent === "Session was not created");
  assert.equal(new URL(page.url()).pathname,receipt.url);
  assert.equal(await page.locator("#creation-source").getAttribute("href"),"/source/");
  assert(!await page.locator("#creation-source").isHidden());
  assert(await page.locator("#creation-retry").isHidden());
  assert(await page.locator("#creation-leave-note").isHidden());
  assert.deepEqual(errors,[]);
  console.log("Creation browser acceptance passed");
 } finally {await browser.close();server.close();}
})().catch(error=>{console.error(error);server.close();process.exitCode=1;});
