"use strict";
// Real Firefox tabs via native WebDriver. This deliberately does not use
// Playwright pages or synthetic visibility properties as tab evidence.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const os = require("node:os");
const path = require("node:path");
const net = require("node:net");
const https = require("node:https");
const {spawn} = require("node:child_process");
const {waitForRestoredStream} = require("./firefox_tabs_readiness.cjs");
const readBudget = process.argv[3] === "--read-budget";
assert(process.argv.length === 3 || (process.argv.length === 4 && readBudget), "Unknown Firefox fixture mode");
const origin = new URL(process.argv[2]);
assert(["127.0.0.1", "localhost", "[::1]"].includes(origin.hostname) && origin.protocol === "https:", "Requires the disposable loopback HTTPS fixture");
assert(process.env.DISPLAY, "A real headful display or reviewed Xvfb environment is required");
const firefox = process.env.FIREFOX_BINARY, gecko = process.env.GECKODRIVER_BINARY;
assert(firefox?.startsWith("/nix/store/") && gecko?.startsWith("/nix/store/"), "Supply exact pinned Firefox and Geckodriver executables");
const wait = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(check, label, budget = 20000) {
  const deadline = Date.now() + budget;
  while (Date.now() < deadline) {if (await check()) return; await wait(100);}
  throw Error(`Condition did not complete: ${label}`);
}
function fixture(endpoint, method = "GET") {
  return new Promise((resolve, reject) => {
    // The only TLS exception is for this already verified disposable origin.
    const request = https.request(new URL(endpoint, origin), {method, rejectUnauthorized:false, timeout:10000}, response => {
      const chunks=[];let length=0;
      response.on("data", chunk => {length+=chunk.length;if(length>1024*1024)request.destroy(Error("Fixture response bound exceeded"));else chunks.push(chunk);});
      response.on("end", () => {
        if (response.statusCode >= 300) return reject(Error(`Fixture status ${response.statusCode}`));
        try {resolve(response.statusCode===204?null:JSON.parse(Buffer.concat(chunks).toString()));}catch(_){reject(Error("Invalid fixture response"));}
      });
    });
    request.on("timeout",()=>request.destroy(Error("Fixture request deadline")));request.on("error",()=>reject(Error("Fixture transport failed")));request.end();
  });
}
function assertBackgroundUnchanged(before, after, active) {
  for (const [document, lanes] of Object.entries(before.counts)) if (document !== String(active)) {
    for (const [lane, count] of Object.entries(lanes)) assert.equal(after.counts[document]?.[lane]?.Count,count.Count,`Hidden document ${document} emitted ${lane}`);
  }
}

(async()=>{
  const handshake=await fixture("/fixture/firefox/status");assert.equal(handshake.fixture,"portal-firefox-tabs-v1");
  await fs.access(firefox);await fs.access(gecko);
  const root=await fs.mkdtemp(path.join(os.tmpdir(),"portal-firefox-tabs-")), profile=path.join(root,"profile");await fs.mkdir(profile);
  const reservation=net.createServer();await new Promise(resolve=>reservation.listen(0,"127.0.0.1",resolve));
  const port=reservation.address().port;await new Promise(resolve=>reservation.close(resolve));
  const child=spawn(gecko,["--host","127.0.0.1","--port",String(port),"--log","error"],{stdio:"ignore"});
  let launchFailed=false;child.on("error",()=>{launchFailed=true;});
  const driver=`http://127.0.0.1:${port}`;let session;
  async function command(endpoint, value, method=value===undefined?"GET":"POST") {
    const response=await fetch(driver+(session?`/session/${session}`:"")+endpoint,{method,signal:AbortSignal.timeout(20000),
      ...(value===undefined?{}:{headers:{"Content-Type":"application/json"},body:JSON.stringify(value)})});
    const decoded=await response.json();
    if(!response.ok || decoded.value?.error)throw Error(`WebDriver ${method} failed (${decoded.value?.error||response.status})`);
    return decoded.value;
  }
  const execute=(script,args=[])=>command("/execute/sync",{script,args});
  const switchTo=handle=>command("/window",{handle});
  const element=async(using,value)=>{const found=await command("/element",{using,value});return found["element-6066-11e4-a52e-4f735466cecf"];};
  const click=async(using,value)=>command(`/element/${await element(using,value)}/click`,{});
  const visibleLog=[];
  let stage="driver-start", lastVisibility=null, beforeDiscard=null, nativeDiscard=null, nativeRestore=null, lastCounts=null;
  let beforeSendStatus=null, beforeSendControls=null;
  const readSendControls=()=>execute(`
    const form=document.getElementById("message-form"), send=document.getElementById("message-send"), textarea=form?.elements.message;
    if (!window.portalSendEvents) {
      window.portalSendEvents={submit:0,invalid:0};
      form?.addEventListener("submit",()=>{portalSendEvents.submit=Math.min(1000,portalSendEvents.submit+1);},true);
      form?.addEventListener("invalid",()=>{portalSendEvents.invalid=Math.min(1000,portalSendEvents.invalid+1);},true);
    }
    const receipts={sending:0,unknown:0,accepted:0,observed:0,other:0};
    for (const receipt of document.querySelectorAll("#message-receipts [data-receipt-state]")) {
      const category=Object.hasOwn(receipts,receipt.dataset.receiptState)?receipt.dataset.receiptState:"other";
      receipts[category]=Math.min(1000,receipts[category]+1);
    }
    return {sendPresent:!!send,sendDisabled:send?.disabled??null,textareaNonempty:!!textarea?.value.trim(),
      expectedDraft:textarea?.value==="Disposable draft retained across native tab switches",formValid:form?.matches(":valid")??null,
      submitting:!!form?.dataset.submitting,submitEvents:portalSendEvents.submit,invalidEvents:portalSendEvents.invalid,receipts};`);
  async function ready(index) {
    await until(()=>execute('return !!document.getElementById("message-send") && !document.getElementById("message-send").disabled;'),`tab ${index} composer ready`,60000);
    const state=await execute(`
      window.portalVisibility ||= [];
      if (!window.portalVisibilityStarted) {
        window.portalVisibilityStarted=true;
        const record=()=>portalVisibility.push({hidden:document.hidden,state:document.visibilityState});
        document.addEventListener("visibilitychange",record);record();
      }
      return {hidden:document.hidden,state:document.visibilityState};`);
    lastVisibility={document:index,...state};
    assert.equal(state.hidden,false);assert.equal(state.state,"visible");
  }
  async function settledBackground(active) {
    await until(async()=>{
      const stats=await fixture("/fixture/firefox/status");
      return Object.entries(stats.counts).every(([document,lanes])=>document===String(active)||Object.values(lanes).every(lane=>lane.Active===0));
    },"background requests and streams canceled");
  }
  try {
    await until(async()=>{
      if(launchFailed || child.exitCode!==null)throw Error("Geckodriver exited before readiness");
      try {const response=await fetch(driver+"/status",{signal:AbortSignal.timeout(1000)});return response.ok;}catch(_){return false;}
    },"Geckodriver readiness",15000);
    const created=await command("/session",{capabilities:{alwaysMatch:{browserName:"firefox",acceptInsecureCerts:true,
      "moz:firefoxOptions":{binary:firefox,args:["--remote-allow-system-access","-profile",profile]}}}});
    session=created.sessionId;
    assert.equal(created.capabilities.browserName,"firefox");
    if (readBudget) {
      stage="read-budget-initialize";
      const handles=[await command("/window")];
      for(let index=0;index<2;index++) {
        if(index)handles.push((await command("/window/new",{type:"tab"})).handle);
        await switchTo(handles[index]);await command("/url",{url:new URL(`/example/?fixtureTab=${index}`,origin).href});await ready(index);
        if(index===0)await command(`/element/${await element("css selector",'#message-form textarea[name="message"]')}/value`,{text:"Disposable draft retained across native tab switches"});
        await until(async()=>{
          const stats=await fixture("/fixture/firefox/status");lastCounts=stats.counts;
          return stats.counts[index]?.thread?.Count>0&&stats.counts[index]?.pending?.Count>0&&stats.counts[index]?.events?.Active===1;
        },`tab ${index} transcript, pending and single SSE ready`);
      }
      assert.equal((await command("/window/handles")).length,2);
      await settledBackground(1);
      stage="read-budget-observation";
      const observationStart=Date.now(), before=await fixture("/fixture/firefox/status");
      assert.deepEqual(Object.keys(before.counts).sort(),["0","1"]);
      await wait(5000);
      const observed=await fixture("/fixture/firefox/status"), observedWindowMs=Date.now()-observationStart;
      lastCounts=observed.counts;
      for(const lane of new Set([...Object.keys(before.counts[0]),...Object.keys(observed.counts[0]??{})])) {
        assert.equal(observed.counts[0]?.[lane]?.Count??0,before.counts[0][lane]?.Count??0,`Hidden document 0 emitted ${lane}`);
        assert.equal(before.counts[0][lane]?.Active??0,0);assert.equal(observed.counts[0]?.[lane]?.Active??0,0);
      }
      assert(observed.counts[1].thread.Count>before.counts[1].thread.Count,"Foreground transcript did not continue refreshing");
      assert.equal(observed.counts[1].events.Active,1,"Foreground requires exactly one SSE");
      assert(observed.counts[1].pending.Count-before.counts[1].pending.Count<=1+Math.floor(observedWindowMs/15000),"Generic 200ms hints bypassed pending freshness");

      stage="read-budget-switch-back";
      await switchTo(handles[0]);await ready(0);
      const transitions=await execute("return portalVisibility;");
      const hiddenIndex=transitions.findIndex(value=>value.hidden&&value.state==="hidden");
      assert(hiddenIndex>=0&&transitions.slice(hiddenIndex+1).some(value=>!value.hidden&&value.state==="visible"),"Tab 0 must observe real hidden then visible transitions");
      visibleLog.push({document:0,transitions});
      assert.equal(await execute('return document.querySelector("#message-form textarea").value;'),"Disposable draft retained across native tab switches");
      await until(async()=>{
        const stats=await fixture("/fixture/firefox/status");lastCounts=stats.counts;
        return stats.counts[0].events.Count>observed.counts[0].events.Count&&stats.counts[0].events.Active===1&&stats.counts[0].pending.Count>observed.counts[0].pending.Count;
      },"fresh pending and single SSE after switch back");
      await settledBackground(0);
      const returned=await fixture("/fixture/firefox/status");
      stage="read-budget-typed-question";
      const questionStart=Date.now();await fixture("/fixture/firefox/control?action=question","POST");
      await until(()=>execute('return document.getElementById("pending").textContent.includes("Disposable Firefox question");'),"typed question invalidation",5000);
      const questionLatencyMs=Date.now()-questionStart;
      await fixture("/fixture/firefox/control?action=clear-question","POST");
      await until(()=>execute('return !document.getElementById("pending").textContent.includes("Disposable Firefox question");'),"question removal",5000);
      stage="read-budget-pending-retry";
      const failureBefore=await fixture("/fixture/firefox/status");await fixture("/fixture/firefox/control?action=fail-pending","POST");
      await until(async()=>{
        const stats=await fixture("/fixture/firefox/status");lastCounts=stats.counts;
        return stats.counts[0].pending.Failed===1&&stats.counts[0].pending.Count>=failureBefore.counts[0].pending.Count+2;
      },"pending retry without reload",10000);
      const final=await fixture("/fixture/firefox/status");
      for(const lane of new Set([...Object.keys(returned.counts[1]),...Object.keys(final.counts[1]??{})])) {
        assert.equal(final.counts[1]?.[lane]?.Count??0,returned.counts[1][lane]?.Count??0,`Hidden document 1 emitted ${lane}`);
        assert.equal(final.counts[1]?.[lane]?.Active??0,0);
      }
      assert.equal(final.counts[0].events.Active,1);assert.equal(final.sendAttempts,0);assert.equal(final.dispatches,0);assert.equal(final.journalEntries,0);
      console.log(JSON.stringify({fixture:"portal-firefox-read-budget-v1",mode:"read-budget",browser:created.capabilities.browserVersion,tabs:2,
        observedWindowMs,questionLatencyMs,visibility:visibleLog,counts:final.counts,sendAttempts:final.sendAttempts,dispatches:final.dispatches,journalEntries:final.journalEntries}));
      return;
    }
    stage="initialize-tabs";
    const handles=[await command("/window")];
    for(let index=0;index<50;index++) {
      if(index)handles.push((await command("/window/new",{type:"tab"})).handle);
      await switchTo(handles[index]);await command("/url",{url:new URL(`/example/?fixtureTab=${index}`,origin).href});await ready(index);
      if(index===0)await command(`/element/${await element("css selector",'#message-form textarea[name="message"]')}/value`,{text:"Disposable draft retained across native tab switches"});
    }
    assert.equal((await command("/window/handles")).length,50);
    stage="native-visibility-switches";
    // Selecting real tabs records both actual hidden and visible transitions.
    for(let index=0;index<50;index++) {
      await switchTo(handles[index]);await ready(index);
      const transitions=await execute("return portalVisibility;");
      assert(transitions.some(value=>value.hidden&&value.state==="hidden"),`tab ${index} never observed real hiding`);
      assert(transitions.some(value=>!value.hidden&&value.state==="visible"));
      visibleLog.push({document:index,transitions});
    }
    await switchTo(handles[0]);await ready(0);await settledBackground(0);
    assert.equal(await execute('return document.querySelector("#message-form textarea").value;'),"Disposable draft retained across native tab switches");
    const initialized=await fixture("/fixture/firefox/status");
    assert.equal(Object.keys(initialized.counts).length,50,"All 50 documents must perform actual reads");
    for(const lanes of Object.values(initialized.counts)) {assert(lanes.pending?.Count>0);assert(lanes.events?.Count>0);}
    stage="sustained-foreground";
    const before=await fixture("/fixture/firefox/status");await wait(35000);const sustained=await fixture("/fixture/firefox/status");
    assertBackgroundUnchanged(before,sustained,0);
    assert(sustained.counts[0].pending.Count-before.counts[0].pending.Count<=4,"Generic 200ms hints bypassed pending freshness");
    assert(sustained.counts[0].thread.Count>before.counts[0].thread.Count,"Foreground transcript did not continue refreshing");
    assert.equal(sustained.counts[0].events.Active,1,"Foreground requires exactly one SSE");

    // Firefox's own tabbrowser discards a background document in this isolated
    // profile. The pending attribute is evidence of native unload, not navigation.
    stage="native-discard";
    beforeDiscard=await fixture("/fixture/firefox/status");
    await command("/moz/context",{context:"chrome"});
    let discarded=await execute(`
      const browser=Services.wm.getMostRecentWindow("navigator:browser").gBrowser;
      const tab=Array.from(browser.tabs).find(tab=>new URL(tab.linkedBrowser.currentURI.spec).searchParams.get("fixtureTab")==="5");
      if(!tab||tab.selected)throw Error("background fixture tab missing");
      tab.setAttribute("portal-fixture-discard","5");
      browser.discardBrowser(tab,true);return {pending:tab.hasAttribute("pending"),selected:tab.selected};`);
    nativeDiscard=discarded;
    if (!discarded.pending) {
      await until(()=>execute('const browser=Services.wm.getMostRecentWindow("navigator:browser").gBrowser;return Array.from(browser.tabs).some(tab=>tab.getAttribute("portal-fixture-discard")==="5"&&tab.hasAttribute("pending"));'),"native discard completion",5000);
      discarded={pending:true,selected:false};
    }
    nativeDiscard=discarded;
    assert.equal(discarded.pending,true,"Native Firefox discard was not established");assert.equal(discarded.selected,false);
    await command("/moz/context",{context:"content"});
    stage="native-restoration";
    await switchTo(handles[5]);await ready(5);await settledBackground(5);
    const restoredDocument=await execute('return {fixtureTab:new URL(location.href).searchParams.get("fixtureTab"),hidden:document.hidden,state:document.visibilityState};');
    lastVisibility={document:5,...restoredDocument};
    assert.equal(restoredDocument.fixtureTab,"5");assert.equal(restoredDocument.hidden,false);assert.equal(restoredDocument.state,"visible");
    await command("/moz/context",{context:"chrome"});
    nativeRestore=await execute(`
      const browser=Services.wm.getMostRecentWindow("navigator:browser").gBrowser;
      const tab=Array.from(browser.tabs).find(tab=>tab.getAttribute("portal-fixture-discard")==="5");
      return {found:!!tab,pending:tab?.hasAttribute("pending")??null,selected:tab?.selected??null};`);
    assert.equal(nativeRestore.found,true);assert.equal(nativeRestore.pending,false);assert.equal(nativeRestore.selected,true);
    await command("/moz/context",{context:"content"});
    stage="restored-stream";
    const restored=await waitForRestoredStream({baseline:beforeDiscard.counts[5].events.Count,
      status:async()=>{const snapshot=await fixture("/fixture/firefox/status");lastCounts=snapshot.counts;return snapshot;}});
    assert(restored.counts[5].events.Count>beforeDiscard.counts[5].events.Count);
    assert.equal(restored.counts[5].events.Active,1);
    await settledBackground(5);
    stage="restore-original-foreground";
    await switchTo(handles[0]);await ready(0);await settledBackground(0);
    assert.equal(await execute('return document.querySelector("#message-form textarea").value;'),"Disposable draft retained across native tab switches");

    stage="typed-question";
    const questionStart=Date.now();await fixture("/fixture/firefox/control?action=question","POST");
    await until(()=>execute('return document.getElementById("pending").textContent.includes("Disposable Firefox question");'),"typed question invalidation",5000);
    const questionLatencyMs=Date.now()-questionStart;
    await fixture("/fixture/firefox/control?action=clear-question","POST");
    await until(()=>execute('return !document.getElementById("pending").textContent.includes("Disposable Firefox question");'),"question removal",5000);
    stage="pending-read-retry";
    const failureBefore=await fixture("/fixture/firefox/status");await fixture("/fixture/firefox/control?action=fail-pending","POST");
    await until(async()=>{const stats=await fixture("/fixture/firefox/status");return stats.counts[0].pending.Failed===1&&stats.counts[0].pending.Count>=failureBefore.counts[0].pending.Count+2;},"pending retry without reload",10000);
    stage="durable-original-retry";
    const {fixture:sendFixture,counts:sendCounts,...sendStatus}=await fixture("/fixture/firefox/status");
    beforeSendStatus=sendStatus;
    beforeSendControls=await readSendControls();
    await click("css selector","#message-send");
    await until(()=>execute('return Array.from(document.querySelectorAll("button")).some(button=>button.textContent.trim()==="Retry original");'),"lost-response durable receipt");
    assert.equal(await execute('return document.querySelector("#message-form textarea").value;'),"Disposable draft retained across native tab switches");
    await click("xpath","//button[normalize-space()='Retry original']");
    await until(()=>execute('return document.querySelector("#message-form textarea").value === "";'),"accepted original retry");
    const final=await fixture("/fixture/firefox/status");assert.equal(final.dispatches,1);assert.equal(final.journalEntries,1);assert.equal(final.sendAttempts,2);
    console.log(JSON.stringify({fixture:"portal-firefox-tabs-v1",browser:created.capabilities.browserVersion,tabs:50,nativeDiscard:true,restored:true,foregroundMs:35000,
      questionLatencyMs,visibility:visibleLog,counts:final.counts,dispatches:final.dispatches,journalEntries:final.journalEntries,sendAttempts:final.sendAttempts}));
  } catch(error) {
    // Preserve fixed stage/counter and observed native/visibility evidence before
    // cleanup. Never include browser contents, URLs or the disposable profile.
    let counts=lastCounts??beforeDiscard?.counts??null, sendStatus=null, sendControls=null;
    try {
      const {fixture:statusFixture,counts:currentCounts,...mutationStatus}=await fixture("/fixture/firefox/status");
      counts=currentCounts;sendStatus=mutationStatus;
    }catch(_){/* Retain prior observations if status is unavailable. */}
    if (beforeSendControls) {try {sendControls=await readSendControls();}catch(_){/* Never replace the original failure. */}}
    console.error(JSON.stringify({fixture:readBudget?"portal-firefox-read-budget-v1":"portal-firefox-tabs-v1",mode:readBudget?"read-budget":"native-tabs",stage,visibility:lastVisibility,
      nativeDiscard,nativeRestore,beforeDiscard:beforeDiscard?.counts?.[5]??null,counts,
      beforeSendStatus,beforeSendControls,sendStatus,sendControls}));
    throw error;
  } finally {
    if(session) {try {await command("",undefined,"DELETE");}catch(_){/* Stop only this owned driver below. */}}
    if(child.pid && child.exitCode===null && child.signalCode===null) {child.kill("SIGTERM");await Promise.race([new Promise(resolve=>child.once("exit",resolve)),wait(5000)]);}
    if(child.pid && child.exitCode===null && child.signalCode===null) {child.kill("SIGKILL");await new Promise(resolve=>child.once("exit",resolve));}
    await fs.rm(root,{recursive:true,force:true});
  }
})().catch(error=>{console.error(error.message);process.exitCode=1;});
