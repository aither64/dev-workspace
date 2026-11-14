"use strict";
const assert = require("node:assert/strict"), fs = require("node:fs"), vm = require("node:vm");
const {chromium, expect} = require("@playwright/test");
const baseURL = process.argv[2], origin = new URL(baseURL).origin;
const acceptance = fs.readFileSync("../../../test/creation_acceptance_browser.cjs", "utf8");
const start = "  async function settings(form) {", end = "  async function followup(marker) {";
assert.equal(acceptance.split(start).length, 2); assert.equal(acceptance.split(end).length, 2);
assert(acceptance.indexOf(end) > acceptance.indexOf(start));
const settingsSource = acceptance.slice(acceptance.indexOf(start), acceptance.indexOf(end));
const timestamp = "2026-11-14T12:00:00Z";
const provider = id => ({id, label: id === "primary" ? "Primary" : "Alternate", kind: id === "primary" ? "subscription" : "codex-lb"});
const model = (id, isDefault = false) => ({id, model:id, displayName:id, isDefault,
  defaultReasoningEffort:"medium", supportedReasoningEfforts:["low","medium","high"].map(reasoningEffort => ({reasoningEffort}))});
const catalog = marker => [model("gpt-5.5", true), model("gpt-5.4"), model(marker)];
function hold() {
  let release, arrived;
  return {seen:new Promise(resolve => {arrived=resolve;}), response:new Promise(resolve => {release=resolve;}), arrived, release};
}
(async () => {
  console.log("FIXTURE_ORIGIN " + baseURL);
  const browser = await chromium.launch({headless:true, channel:"chromium"});
  const totals = {pages:0, forks:0, reconciliations:0, businessWrites:0};
  try {
    async function run(name, check) {
      const page = await browser.newPage({ignoreHTTPSErrors:true});
      const errors = [], unexpected = [], expectedHTTP = [], posts = [], held = [], readsByPath = {};
      let token = 1, reads = 0, requestCount = 0, phase = "bootstrap";
      const pendingRequests = new Map(), failedRequests = [];
      page.on("request", request => pendingRequests.set(request, {path:new URL(request.url()).pathname,method:request.method()}));
      page.on("requestfinished", request => pendingRequests.delete(request));
      page.on("requestfailed", request => {const item=pendingRequests.get(request);pendingRequests.delete(request);if(failedRequests.length<12)failedRequests.push({...item,failure:(request.failure()?.errorText||"unknown").slice(0,160)});});
      const models = new Map(), queues = new Map();
      const configure = (id, response) => models.set(id, response);
      const holdModels = id => {const item=hold();held.push(item);queues.set(id,item);return item;};
      page.on("pageerror", error => errors.push(error.message));
      page.on("console", message => {
        if (message.type() !== "error") return;
        const url = message.location().url, text = message.text();
        if (expectedHTTP.some(item => item.url === url && text.includes(String(item.status)))) {
          console.log("EXPECTED_HTTP_CONSOLE " + JSON.stringify({name,url,text})); return;
        }
        errors.push(text);
      });
      await page.addInitScript(() => {
        window.forkFixtureFresh = {keys:Object.keys(localStorage), permission:globalThis.Notification?.permission};
        window.forkFixtureEvents = [];
        window.EventSource = class {
          constructor(url) {this.url=url;this.listeners={};window.forkFixtureEvents.push(this);queueMicrotask(() => this.onopen?.());}
          addEventListener(name,fn) {(this.listeners[name] ||= []).push(fn);}
          removeEventListener() {} close() {}
        };
      });
      await page.route("**/*", async route => {
        const request=route.request(), url=new URL(request.url()), path=url.pathname, method=request.method();
        const scope=request.headers()["x-workspace-conversation-scope"];
        if (requestCount++ < 110) console.log("FIXTURE_REQUEST " + JSON.stringify({name,path,query:url.search,method,scopePresent:scope!==undefined}));
        try {
          assert.equal(url.origin,origin,"external request"); assert.equal(scope,undefined,"Codex request must be unscoped");
          if (method === "POST" && path === "/api/sessions/example/queue/reconcile") {
            assert.equal(url.search,"");assert.equal(request.postData(),"{}");assert.equal(request.headers()["content-type"],"application/json");
            totals.reconciliations++; return route.fulfill({json:{ok:true}});
          }
          if (method === "POST" && path === "/api/sessions/example/fork") {
            assert.equal(url.search,"");assert.equal(request.headers()["content-type"],"application/json");
            const body=request.postDataJSON();posts.push(body);totals.forks++;
            assert.equal(posts.length,1,"one fork attempt");
            expectedHTTP.push({url:request.url(),status:409});
            return route.fulfill({status:409,json:{error:"Controlled fork refusal"}});
          }
          if (method !== "GET") {totals.businessWrites++;throw Error("unexpected business write: "+method+" "+path);}
          if (path === "/example/" || path.startsWith("/static/") || path.startsWith("/codex/assets/")) return route.continue();
          if (path === "/favicon.ico") return route.fulfill({status:204});
          assert.equal(request.postData(),null,"GET body");
          readsByPath[path]=(readsByPath[path]||0)+1;
          const json=data=>route.fulfill({json:data});
          if (path === "/api/models") {
            const query=[...url.searchParams];
            assert(query.length===0 || JSON.stringify(query)===JSON.stringify([["session","example"]]) ||
              query.length===1&&query[0][0]==="provider"&&["primary","alternate"].includes(query[0][1]),"model query");
            const id=url.searchParams.get("provider")||"initial";
            const pending=queues.get(id);let response=models.get(id)||catalog(id+"-installed");
            if (pending) {queues.delete(id);pending.arrived();response=await pending.response;}
            if (response === "fail") {expectedHTTP.push({url:request.url(),status:500});return route.fulfill({status:500,json:{error:"Controlled catalog failure"}});}
            return json(response);
          }
          if (path === "/api/codex-providers" || path === "/api/codex-limits") {
            const query=[...url.searchParams],session=JSON.stringify(query)===JSON.stringify([["session","example"]]);
            const selected=url.searchParams.get("provider");
            assert(query.length===0||session||path==="/api/codex-limits"&&query.length===1&&query[0][0]==="provider"&&["primary","alternate"].includes(selected),"provider query");
            return json(path==="/api/codex-providers" ? {providers:[provider("primary"),provider("alternate")],active:"primary",default:"primary",inherited:true,...(session?{session:"example"}:{})} :
              {provider:provider(selected||"primary"),windows:[],updatedAt:timestamp,canReset:false});
          }
          if (path === "/api/collaboration-modes") return json([{mode:"default",name:"Default"},{mode:"plan",name:"Plan"}]);
          if (path === "/uploads/s-example") return json({files:[],limits:{chunkBytes:1024,fileBytes:1024,promptBytes:2048,files:1}});
          if (path === "/api/browser-notifications") {
            assert.deepEqual([...url.searchParams],[["registrationId",""]]);
            return json({version:1,deliveryEpoch:1,publicKey:"",registered:false,result:"",available:false});
          }
          const relative=path.startsWith("/api/sessions/example/")?path.slice("/api/sessions/example/".length):null;
          assert(relative,"unknown global route: "+path);
          if (relative==="thread" || relative==="thread/page") {
            reads++;return json({threadId:"thread-1",latestTurnId:"source-turn",status:"idle",collaborationMode:"default",model:"gpt-5.5",reasoningEffort:"medium",hasOlder:false,
              entries:[{kind:"agentMessage",itemId:"source-marker",turnId:"source-turn",text:"SOURCE_MEDIUM_"+token,timestamp}]});
          }
          if (["pending","queue"].includes(relative)) return json([]);
          if (relative==="activity") return json({currentState:"idle",observedAtMs:Date.now(),stateSinceMs:Date.now(),coverageComplete:true});
          if (relative==="retry/activity") return json({serverNowMs:Date.now(),activity:null});
          if (relative==="details") return json({repositoriesHTML:"",artifactsHTML:"",sessionBackend:"codex",conversationScope:"",conversationInteractive:true,conversationHeld:false,readyMembers:[]});
          if (relative==="operation") return json({state:"idle",options:{}});
          if (relative==="attention") return json({version:1,identity:await page.locator("body").getAttribute("data-session-observation"),rootThread:"thread-1",enabled:false,revision:0,event:null,fresh:true});
          if (relative==="retry-policy") return json({revision:"fixture",capacity:false,cybersecurity:false});
          if (relative==="auto-archive") return json({enabled:false,hold:true,blockers:[]});
          if (relative==="progress") return json({available:false});
          if (relative==="coordination") return json({workspaces:[],reservationHolders:[],jobs:[]});
          if (relative==="communication") return json({candidates:[],grants:[],messages:[]});
          throw Error("unhandled route: "+path);
        } catch (error) {
          unexpected.push({path,method,error:error.message});console.error("FIXTURE_ROUTE_ERROR "+JSON.stringify(unexpected.at(-1)));
          return route.fulfill({status:500,json:{error:"Fixture contract failure"}});
        }
      });
      try {
        totals.pages++;
        const time=new Date(timestamp);await page.clock.install({time});await page.clock.pauseAt(time);
        await page.goto(baseURL+"/example/#codex");
        phase="module/catalog barrier";
        await expect(page.locator('[data-codex-mode="default"]')).toHaveCount(1);
        await page.clock.runFor(0);
        phase="initial transcript";
        await expect(page.locator("#transcript")).toContainText("SOURCE_MEDIUM_1");
        await expect(page.locator('[data-codex-mode="default"]')).toHaveAttribute("aria-pressed","true");
        await expect(page.locator("#fork-open")).toBeEnabled();await page.locator("#fork-open").click();
        const form=page.locator("#fork-form"), select=form.locator("[name=model]"), effort=form.locator("[name=effort]");
        await expect(select.locator('option[value="initial-installed"]')).toHaveCount(1);
        await expect(form.locator("[name=provider]")).toBeEnabled();await expect(select).toBeEnabled();
        const fresh=await page.evaluate(async()=>({...window.forkFixtureFresh,registrations:navigator.serviceWorker?(await navigator.serviceWorker.getRegistrations()).length:0}));
        assert.deepEqual(fresh.keys,[]);assert.notEqual(fresh.permission,"granted");assert.equal(fresh.registrations,0);
        const guard=()=>{assert.deepEqual(unexpected,[],"unaccounted routes");assert.deepEqual(errors,[],"unexpected application errors");assert.equal(totals.businessWrites,0);};
        const pair=async()=>({model:await select.inputValue(),effort:await effort.inputValue()});
        const refresh=async()=>{
          token++;const previous=reads;
          await page.evaluate(()=>{for(const event of window.forkFixtureEvents) event.onmessage?.({data:JSON.stringify({type:"changed"})});});
          await page.clock.runFor(300);await expect.poll(()=>reads).toBeGreaterThan(previous);
          await expect(page.locator("#transcript")).toContainText("SOURCE_MEDIUM_"+token);
        };
        const submit=async(expected)=>{
          await form.locator("[name=name]").fill("fork-"+name);
          await form.locator("[name=sensitive]").check();await form.locator("[name=browserNotificationsEnabled]").uncheck();
          const creationDate=await form.locator("[name=creationDate]").inputValue();
          const selected=await form.evaluate(f=>({provider:f.elements.provider.value,model:f.elements.model.value,reasoningEffort:f.elements.effort.value}));
          assert.deepEqual(selected,{provider:expected.provider,model:expected.model,reasoningEffort:expected.effort});
          await form.locator('[type="submit"]').click();await expect.poll(()=>posts.length).toBe(1);
          await expect(page.locator("#fork-progress")).toContainText("Controlled fork refusal");
          await expect(form).toHaveAttribute("data-request-locked","false");
          assert.deepEqual(posts,[{name:"fork-"+name,creationDate,model:expected.model,reasoningEffort:expected.effort,browserNotificationsEnabled:false,provider:expected.provider,sensitive:true}]);
          await page.clock.runFor(300);assert.equal(posts.length,1,"no automatic second POST");
          console.log("SERIALIZED_FORK "+JSON.stringify({name,selected,posts,controlledStatus:409}));
        };
        phase="fork selection control";
        await check({page,form,select,effort,holdModels,configure,guard,pair,refresh,submit,posts});
        guard();console.log("CASE_PASS "+JSON.stringify({name,posts:posts.length,reads,expectedHTTP,readsByPath}));
      } catch (error) {
        try {
          const dom=await page.evaluate(()=>{
            const view=id=>{const element=document.getElementById(id);return element?{hidden:element.hidden,text:element.textContent.slice(0,600)}:null;};
            const modes=[...document.querySelectorAll('[data-codex-mode="default"]')];
            return {readyState:document.readyState,hidden:document.hidden,visibility:document.visibilityState,
              connection:view("conversation-connection"),modeStatus:view("codex-mode-status"),transcript:view("transcript"),
              modeCount:modes.length,pressed:modes.map(element=>element.getAttribute("aria-pressed")),
              events:(window.forkFixtureEvents||[]).slice(0,6).map(source=>({onopen:typeof source.onopen==="function",onmessage:typeof source.onmessage==="function"}))};
          });
          console.log("FORK_FAILURE_SNAPSHOT "+JSON.stringify({name,phase,original:{name:error.name,message:error.message.slice(0,1800)},errors:errors.slice(0,12),unexpected:unexpected.slice(0,12),expectedHTTP:expectedHTTP.slice(0,12),reads,readsByPath,totals:{...totals},posts:posts.length,dom,pending:[...pendingRequests.values()].slice(0,12),failed:failedRequests.slice(0,12)}));
        } catch (snapshotError) {console.log("FORK_FAILURE_SNAPSHOT_UNAVAILABLE "+JSON.stringify({phase,originalName:error.name,snapshotName:snapshotError.name}));}
        throw error;
      } finally {
        for(const item of held)item.release(catalog("released-in-finally"));
        await page.unrouteAll({behavior:"wait"});await page.close();
      }
    }
    await run("removed",async f=>{
      const pending=f.holdModels("alternate");await f.form.locator("[name=provider]").selectOption("alternate");await pending.seen;
      await f.select.selectOption("gpt-5.4");await f.effort.selectOption("high");
      assert.deepEqual(await f.pair(),{model:"gpt-5.4",effort:"high"});
      const replacement=[model("removed-default",true),model("gpt-5.5")];
      pending.release(replacement);
      await expect(f.select.locator('option[value="removed-default"]')).toHaveCount(1);
      await expect(f.select.locator('option[value="gpt-5.4"]')).toHaveCount(0);
      f.guard();assert.equal(f.posts.length,0);
      console.log("PRE_REMOVED_FALLBACK "+JSON.stringify({pair:await f.pair(),replacementInstalled:true,removedAbsent:true,posts:f.posts.length}));
      assert.deepEqual(await f.pair(),{model:"gpt-5.5",effort:"medium"});
      await f.refresh();assert.deepEqual(await f.pair(),{model:"gpt-5.5",effort:"medium"});
      f.configure("primary",catalog("removed-b-restored"));await f.form.locator("[name=provider]").selectOption("primary");
      await expect(f.select.locator('option[value="removed-b-restored"]')).toHaveCount(1);
      await expect(f.select.locator('option[value="gpt-5.4"]')).toHaveCount(1);
      const automatic=f.holdModels("alternate");await f.form.locator("[name=provider]").selectOption("alternate");await automatic.seen;
      await f.select.selectOption("gpt-5.4");await f.effort.selectOption("");
      assert.deepEqual(await f.pair(),{model:"gpt-5.4",effort:""});
      automatic.release(replacement);
      await expect(f.select.locator('option[value="removed-default"]')).toHaveCount(1);
      await expect(f.select.locator('option[value="gpt-5.4"]')).toHaveCount(0);
      f.guard();assert.equal(f.posts.length,0);
      console.log("REMOVED_AUTOMATIC_FALLBACK "+JSON.stringify({pair:await f.pair(),replacementInstalled:true,removedAbsent:true,posts:f.posts.length}));
      assert.deepEqual(await f.pair(),{model:"gpt-5.5",effort:"medium"});
      await f.refresh();assert.deepEqual(await f.pair(),{model:"gpt-5.5",effort:"medium"});
      await f.submit({provider:"alternate",model:"gpt-5.5",effort:"medium"});
    });
    await run("primary",async f=>{
      const pending=f.holdModels("alternate"), retained=[];
      const context=vm.createContext({options:{provider:"alternate",sensitive:true,notifications:false},retainSettings:(stage,values)=>retained.push({stage,values})});
      vm.runInContext(settingsSource+"\nglobalThis.choose=settings;",context);await context.choose(f.form);await pending.seen;
      assert.deepEqual(await f.pair(),{model:"gpt-5.5",effort:"high"});
      console.log("BEFORE_CATALOG_RELEASE "+JSON.stringify({retained,pair:await f.pair()}));
      pending.release(catalog("alternate-installed"));await expect(f.select.locator('option[value="alternate-installed"]')).toHaveCount(1);
      f.guard();console.log("PRE_EXACT_HIGH "+JSON.stringify({pair:await f.pair(),destinationOption:true,posts:f.posts.length}));
      await expect(f.effort).toHaveValue("high");
      const name=f.form.locator("[name=name]");
      for (const [value,valid] of [["A",true],["a-1_b",true],["-bad",false],["_bad",false],["bad/name",false],["bad name",false]]) {
        await name.fill(value);
        assert.equal(await name.evaluate(input=>input.checkValidity()),valid,"native fork name validity: "+value);
      }
      await name.fill("fork-primary");
      console.log("NATIVE_NAME_VALIDITY six controlled values; zero submission");
      await f.refresh();await expect(f.effort).toHaveValue("high");
      await f.submit({provider:"alternate",model:"gpt-5.5",effort:"high"});
    });
    await run("second",async f=>{
      const pending=f.holdModels("alternate");await f.form.locator("[name=provider]").selectOption("alternate");await pending.seen;
      await f.select.selectOption("gpt-5.4");await expect(f.effort).toHaveValue("medium");await f.effort.selectOption("high");
      pending.release(catalog("alternate-installed"));await expect(f.select.locator('option[value="alternate-installed"]')).toHaveCount(1);
      assert.deepEqual(await f.pair(),{model:"gpt-5.4",effort:"high"});await f.refresh();assert.deepEqual(await f.pair(),{model:"gpt-5.4",effort:"high"});
      await f.submit({provider:"alternate",model:"gpt-5.4",effort:"high"});
    });
    await run("automatic",async f=>{
      const pending=f.holdModels("alternate");await f.form.locator("[name=provider]").selectOption("alternate");await pending.seen;
      await f.select.selectOption("gpt-5.5");await f.effort.selectOption("");pending.release(catalog("alternate-installed"));
      await expect(f.select.locator('option[value="alternate-installed"]')).toHaveCount(1);await expect(f.effort).toHaveValue("");
      await f.refresh();await expect(f.effort).toHaveValue("");await f.submit({provider:"alternate",model:"gpt-5.5",effort:""});
    });
    await run("untouched",async f=>{assert.deepEqual(await f.pair(),{model:"gpt-5.5",effort:"medium"});await f.refresh();assert.deepEqual(await f.pair(),{model:"gpt-5.5",effort:"medium"});});
    await run("obsolete",async f=>{
      const old=f.holdModels("alternate");await f.form.locator("[name=provider]").selectOption("alternate");await old.seen;
      f.configure("primary",catalog("primary-current"));await f.form.locator("[name=provider]").selectOption("primary");
      await expect(f.select.locator('option[value="primary-current"]')).toHaveCount(1);await f.effort.selectOption("high");
      const response=f.page.waitForResponse(r=>new URL(r.url()).pathname==="/api/models"&&new URL(r.url()).searchParams.get("provider")==="alternate");
      old.release(catalog("obsolete-response"));await (await response).finished();await f.page.clock.runFor(0);
      await f.refresh();await expect(f.select.locator('option[value="obsolete-response"]')).toHaveCount(0);await expect(f.select.locator('option[value="primary-current"]')).toHaveCount(1);await expect(f.effort).toHaveValue("high");
      await f.submit({provider:"primary",model:"gpt-5.5",effort:"high"});
    });
    await run("failure",async f=>{f.configure("alternate","fail");await f.form.locator("[name=provider]").selectOption("alternate");await expect(f.select).toBeDisabled();await expect(f.effort).toBeDisabled();await expect(f.select.locator("option")).toHaveText("Model catalog unavailable");assert.equal(f.posts.length,0);});
    await run("empty",async f=>{f.configure("alternate",[]);await f.form.locator("[name=provider]").selectOption("alternate");await expect(f.select.locator("option")).toHaveCount(0);await expect(f.select).toBeEnabled();await expect(f.effort).toBeDisabled();await f.form.locator("[name=name]").fill("empty-model");assert(await f.select.evaluate(s=>s.validity.valueMissing&&!s.checkValidity()));await f.form.locator('[type="submit"]').click();assert.equal(f.posts.length,0);});
    console.log("FORK_SETTINGS_COMPLETE "+JSON.stringify(totals));
  } finally {await browser.close();}
})().catch(error=>{console.error(error);process.exitCode=1;});
