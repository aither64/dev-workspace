"use strict";
const fs=require("node:fs"),vm=require("node:vm"),assert=require("node:assert/strict");
let completed=false;process.on("beforeExit",()=>{if(!process.exitCode)assert(completed,"all portal delivery contracts must finish")});
const source=fs.readFileSync("static/app.js","utf8");
const start=source.indexOf("    const submitPortalDelivery ="),end=source.indexOf("    const submitMessage =",start);
assert(start>0&&end>start);
const tick=()=>new Promise(resolve=>setImmediate(resolve));
function fixture(){
  const metadata={version:1,scope:"stable-scope",selection:"a".repeat(64),canEnqueue:true,deliveryModes:["steer","followUp"],settings:{model:"model",reasoningEffort:"high",collaborationMode:"plan"},entries:[]};
  const textarea={value:"original"},calls=[],files={values:["saved-file"],ids(){return this.values},clear(){this.values=[]}},status={hidden:true,textContent:""};
  const context=vm.createContext({portalDeliveryMetadata:metadata,portalDeliveryBaseline:metadata,portalDeliveryReady:()=>true,dispatchCurrent:()=>context.current,
    current:true,currentModel:"model",currentEffort:"high",draftGeneration:0,durableAttempted:false,textarea,composerUploads:files,
    portalDeliveryView:{refresh:async()=>context.metadata,present:value=>{context.presented=value}},metadata,
    document:{querySelector:()=>({dataset:{piMode:context.visibleMode}})},visibleMode:"plan",
    loadSendAttempts:()=>[],sendAttemptStorage:{},slug:"session",dispatchThreadID:"thread",messageDispatcher:{entries:[]},
    conversationAssets:{sameAttachments:(a,b)=>JSON.stringify(a)===JSON.stringify(b),deliveryStatusPresentation:value=>({label:{queued:"Queued",sent:"Sent",unknown:"Outcome unknown",not_sent:"Not sent"}[value.state],description:""})},
    submissionStatus:status,portalDeliverySender:{send:async(text,attachmentIds,selection)=>{calls.push({text,attachmentIds,selection});return {state:"queued"}}},
  });
  vm.runInContext(source.slice(start,end)+"\nglobalThis.submit=submitPortalDelivery;",context);
  return {context,metadata,textarea,files,calls,status};
}
(async()=>{
  const {mountPiPlan}=await import("./static/pi-plan.js");
  for(const outcome of ["exact","rejected","changed"]){
    let snapshot={revision:0,mode:"plan",toolPolicyRevision:1,toolPolicyDigest:"a".repeat(64),canSelectMode:true,proposals:[],acceptances:[]};
    const api={read:async()=>snapshot,setMode:async()=>{if(outcome==="rejected")return {outcome:"rejected"};snapshot={...snapshot,mode:"default",toolPolicyRevision:2,toolPolicyDigest:outcome==="changed"?"c".repeat(64):"b".repeat(64)};return {outcome:"applied",policy:{mode:"default",revision:2,digest:"b".repeat(64)}}}};
    const plan=mountPiPlan({api,namespace:"fixture"});plan.update(snapshot);
    assert.equal(await plan.selectMode("default"),outcome==="exact");plan.destroy();
  }

  {
    const f=fixture();f.context.portalDeliveryMetadata={...f.metadata,selection:"b".repeat(64)};f.context.metadata=f.context.portalDeliveryMetadata;
    await assert.rejects(f.context.submit(false),/changed/);assert.equal(f.calls.length,0);assert.equal(f.textarea.value,"original");
  }
  {
    const begin=source.indexOf("      const retainedDelivery = metadata =>"),end=source.indexOf("      };",begin)+8;
    assert(begin>0&&end>begin);
    const ctx=vm.createContext({durableAttempted:false,portalDeliverySender:{pending:()=>[]}});
    vm.runInContext(source.slice(begin,end)+"\nglobalThis.retained=retainedDelivery;",ctx);
    assert.equal(ctx.retained(null),false);
    assert.equal(ctx.retained({entries:[{state:"sent"}]}),true);
    ctx.portalDeliverySender.pending=()=>[{id:"original"}];assert.equal(ctx.retained(null),true);
    ctx.portalDeliverySender.pending=()=>{throw new Error("storage unavailable")};assert.equal(ctx.retained(null),true);
    ctx.portalDeliverySender={pending:()=>[]};ctx.durableAttempted=true;assert.equal(ctx.retained(null),true);
  }
  {
    const begin=source.indexOf("    const confirmPortalDeliverySelection ="),end=source.indexOf("    rearmPortalDeliverySelection =",begin);
    const f=fixture();let next={...f.metadata,selection:"b".repeat(64)};
    Object.assign(f.context,{deliveryConfirmation:0,renderCodexSettingsSummary(){},updateMessageActions(){},dispatchClient:{deliveries:async()=>next}});
    f.context.conversationAssets.validateDeliveryCollection=(value,scope)=>{if(value.scope!==scope)throw Error("scope changed");return value};
    vm.runInContext(source.slice(begin,end)+"\nglobalThis.confirm=confirmPortalDeliverySelection;",f.context);
    assert.equal(await f.context.confirm(f.metadata.settings),true);assert.equal(f.context.portalDeliveryBaseline.selection,next.selection);
    next={...next,scope:"foreign",selection:"c".repeat(64)};assert.equal(await f.context.confirm(f.metadata.settings),false);assert.equal(f.context.portalDeliveryBaseline.selection,"b".repeat(64));
    f.context.dispatchClient.deliveries=async()=>{throw Error("unavailable")};assert.equal(await f.context.confirm(f.metadata.settings),false);
    let release;f.context.dispatchClient.deliveries=()=>new Promise(resolve=>{release=resolve});const pending=f.context.confirm(f.metadata.settings);await tick();f.context.deliveryConfirmation++;release(f.metadata);assert.equal(await pending,false);
  }
  for(const queue of [false,true]){
    const f=fixture();await f.context.submit(queue);
    assert.equal(f.calls.length,1);assert.equal(f.calls[0].selection.delivery,queue?"followUp":"steer");assert.equal(f.calls[0].selection.selection,f.metadata.selection);
    assert.equal(f.textarea.value,"");assert.equal(f.context.presented.state,"queued");assert.equal(f.status.hidden,true);assert.equal(f.status.textContent,"");
  }
  for(const change of ["selection","scope","model","effort","mode"]){
    const f=fixture();f.context.metadata={...f.metadata,settings:{...f.metadata.settings}};
    if(change==="selection")f.context.metadata.selection="b".repeat(64);
    if(change==="scope")f.context.metadata.scope="replacement";
    if(change==="model")f.context.metadata.settings.model="changed";
    if(change==="effort")f.context.metadata.settings.reasoningEffort="changed";
    if(change==="mode")f.context.metadata.settings.collaborationMode="default";
    await assert.rejects(f.context.submit(false),/selected conversation, settings or mode changed/);assert.equal(f.calls.length,0);assert.equal(f.textarea.value,"original");
  }
  {
    const f=fixture();let release;f.context.portalDeliveryView.refresh=()=>new Promise(resolve=>{release=resolve});
    const pending=f.context.submit(false);await tick();f.textarea.value="edited during metadata read";f.context.draftGeneration++;
    release(f.metadata);await tick();f.context.portalDeliveryView.refresh=async()=>f.metadata;release(f.metadata);await pending;
    assert.equal(f.calls[0].text,"original");assert.equal(f.textarea.value,"edited during metadata read");
  }
  {
    const f=fixture();f.context.portalDeliverySender.send=async()=>{throw new Error("lost admission reply")};
    await assert.rejects(f.context.submit(false),/lost admission/);assert.equal(f.textarea.value,"original");assert.deepEqual(f.files.values,["saved-file"]);
  }
  for(const state of ["unknown","not_sent"]){const f=fixture();f.context.portalDeliverySender.send=async()=>({state});await f.context.submit(false);assert.equal(f.context.presented.state,state);assert.equal(f.status.hidden,true)}
  for(const change of ["error","newer-error","destroy"]){
    const f=fixture();let release;f.context.portalDeliverySender.send=()=>new Promise(resolve=>{release=resolve});
    const pending=f.context.submit(false);await tick();
    f.status.hidden=false;f.status.textContent=change==="error"?"Other operation failed":"Newer failure";
    if(change==="destroy")f.context.current=false;
    release({state:"queued"});await pending;
    assert.equal(f.status.textContent,change==="error"?"Other operation failed":"Newer failure");assert.equal(f.status.hidden,false);
    assert.equal(Boolean(f.context.presented),change!=="destroy");
  }
completed=true;
})().catch(error=>{console.error(error);process.exitCode=1});
