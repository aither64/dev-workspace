// Browser acceptance for failed submissions and the saved initialization request.
// Use the same PLAYWRIGHT_MODULE and CHROMIUM_EXECUTABLE as repository_browser.cjs.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE);
const http = require("node:http"), fs = require("node:fs"), path = require("node:path"), assert = require("node:assert/strict");
const root = process.cwd(), provider = process.env.CODEX_WEB_SOURCE;
const prompt = "Keep this request <script>literal</script>\nSecond line.";
let failure = "http", submissions = [], retries = [];
const receipt = {slug:"2026-09-13-example", url:"/2026-09-13-example/", receiptId:"receipt", attempt:1, state:"failed", phase:"Initialization stopped", error:"context deadline exceeded", initialRequest:prompt, startedAt:new Date().toISOString()};
const index = `<!doctype html><body data-index><form id="new-session-form"><input name="creation_date" value="2026-09-13"><input name="name" required><textarea name="goal" required></textarea><select name="model" data-model-select></select><select name="effort" data-effort-select></select><input name="uploadScope"><div id="creation-uploads"></div><span id="creation-upload-controls"></span><p id="new-session-progress" hidden></p><button type="submit">Create session</button></form><script src="/static/app.js"></script>`;
const creation = `<!doctype html><body data-creation="2026-09-13-example"><h1 id="creation-title"></h1><p id="creation-phase"></p><p id="creation-error"></p><button id="creation-retry">Retry initialization</button><p id="creation-elapsed"></p><a id="creation-source"></a><section id="creation-request-panel"><span id="creation-request-copy"></span><pre id="creation-request"></pre></section><script src="/static/creation.js"></script>`;
const server = http.createServer(async (req,res) => {
 const url = new URL(req.url,"http://fixture");
 const send = (type,data,status=200) => {res.writeHead(status,{"Content-Type":type});res.end(data);};
 const json = (data,status=200) => send("application/json",JSON.stringify(data),status);
 if (url.pathname === "/") return send("text/html",index);
 if (url.pathname === receipt.url) return send("text/html",creation);
 if (url.pathname.startsWith("/static/")) return send("text/javascript",fs.readFileSync(path.join(root,"portal/internal/web/static",path.basename(url.pathname))));
 if (url.pathname.startsWith("/codex/assets/")) return send("text/javascript",fs.readFileSync(path.join(provider,"conversation/assets",path.basename(url.pathname))));
 if (url.pathname === "/sessions") {
  let body=""; for await (const chunk of req) body += chunk;
  submissions.push(new URLSearchParams(body).get("goal"));
  if (failure === "network") return req.socket.destroy();
  if (failure === "http") return json({error:"Temporary initialization service failure"},503);
  return json(receipt,202);
 }
 if (url.pathname.endsWith("/creation/retry")) {
  let body=""; for await (const chunk of req) body += chunk;
  retries.push(JSON.parse(body)); receipt.attempt++; return json(receipt);
 }
 if (url.pathname.endsWith("/creation")) return json(receipt);
 if (url.pathname === "/api/models") return json([{model:"test-model",displayName:"Test model",supportedReasoningEfforts:[{reasoningEffort:"xhigh"}]}]);
 return json({error:"Fixture endpoint unavailable"},503);
});
(async () => {
 await new Promise(resolve => server.listen(0,"127.0.0.1",resolve));
 const origin = "http://127.0.0.1:"+server.address().port;
 const browser = await chromium.launch({executablePath:process.env.CHROMIUM_EXECUTABLE,headless:true,args:["--no-sandbox"]});
 try {
  const page = await browser.newPage(); const errors=[]; page.on("pageerror",e=>errors.push(e.message));
  await page.addInitScript(() => Object.defineProperty(navigator,"clipboard",{value:{writeText:async text=>{window.copiedText=text;}}}));
  await page.goto(origin); await page.waitForTimeout(100);
  await page.locator('[name="name"]').fill("example"); await page.locator('[name="goal"]').fill(prompt);
  await page.locator('[name="model"]').selectOption("test-model"); await page.locator('[name="effort"]').selectOption("xhigh");
  await page.getByRole("button",{name:"Create session"}).click();
  await page.waitForFunction(()=>document.querySelector("#new-session-progress").textContent.includes("Temporary initialization"));
  assert.equal(await page.locator('[name="goal"]').inputValue(),prompt);
  await page.reload(); assert.equal(await page.locator('[name="goal"]').inputValue(),prompt);
  await page.waitForFunction(()=>document.querySelector('[name="effort"]').value==="xhigh");
  assert.equal(await page.locator('[name="model"]').inputValue(),"test-model");
  failure="network"; await page.getByRole("button",{name:"Create session"}).click();
  await page.waitForFunction(()=>!document.querySelector('button[type="submit"]').disabled);
  await page.reload(); assert.equal(await page.locator('[name="goal"]').inputValue(),prompt);
  await page.waitForFunction(()=>document.querySelector('[name="effort"]').value==="xhigh");
  assert.equal(await page.locator('[name="model"]').inputValue(),"test-model");
  failure="none"; await page.getByRole("button",{name:"Create session"}).click();
  await page.waitForURL(origin+receipt.url);
  await page.waitForFunction(expected=>document.querySelector("#creation-request").textContent===expected,prompt);
  await page.getByRole("button",{name:"Copy initial request"}).click();
  assert.equal(await page.evaluate(()=>window.copiedText),prompt);
  await page.getByRole("button",{name:"Retry initialization"}).click();
  await page.waitForFunction(()=>!document.querySelector("#creation-retry").disabled);
  assert.equal(await page.locator("#creation-request").textContent(),prompt);
  assert.deepEqual(retries,[{receiptId:"receipt",attempt:1}]);
  assert(submissions.length>=3); assert(submissions.every(value=>value===prompt));
  assert.equal(await page.evaluate(()=>sessionStorage.getItem("workspace-portal.creation-draft")),null);
  assert.deepEqual(errors,[]);
  console.log("Creation browser acceptance passed");
 } finally {await browser.close();server.close();}
})().catch(error=>{console.error(error);server.close();process.exitCode=1;});
