"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs"), vm = require("node:vm");
const {createSessionClient} = require("./static/app.js");
const source = fs.readFileSync("./static/app.js", "utf8");
const section = (start,end) => {const a=source.indexOf(start),b=source.indexOf(end,a);assert(a>=0&&b>a);return source.slice(a,b);};
(async () => {
 const calls=[];
 const client=createSessionClient("source",async(path,options)=>{calls.push({path,options});return {url:"/2026-11-18-fork/?member=reviewer0"};},null,"reviewer0");
 const fields={name:{value:"fork"},creationDate:{value:"2026-11-18"},sensitive:{checked:true},browserNotificationsEnabled:{checked:true}};
 let submit,redirect;
 const form={dataset:{inheritsSettings:"true"},elements:fields,addEventListener:(kind,fn)=>{assert.equal(kind,"submit");submit=fn;},querySelectorAll:()=>[]};
 const dialog={querySelector:()=>null,showModal:()=>{}};
 const sandbox={document:{getElementById:id=>id==="fork-form"?form:id==="fork-dialog"?dialog:null},interactive:false,client,Array,Boolean,location:{assign:url=>{redirect=url;}},timedProgress:()=>({stop(){},fail(message){throw Error(message);}}),creationProviderValue:()=>{throw Error("inherited fork read provider override");}};
 vm.runInNewContext(section('  const forkDialog = document.getElementById("fork-dialog");','  document.getElementById("clusters")?.addEventListener'),sandbox);
 assert(submit);await submit({preventDefault(){}});
 assert.equal(calls.length,1);assert.equal(calls[0].path,"/api/sessions/source/fork?member=reviewer0");
 assert.deepEqual(JSON.parse(calls[0].options.body),{name:"fork",creationDate:"2026-11-18",browserNotificationsEnabled:true,sensitive:true});
 assert.equal(redirect,"/2026-11-18-fork/?member=reviewer0");
 // The same ordinary client keeps Codex override semantics.
 calls.length=0;await client.fork("fork","2026-11-18","codex-model","high",false,true,"provider");
 const codex=JSON.parse(calls[0].options.body);assert.equal(codex.model,"codex-model");assert.equal(codex.reasoningEffort,"high");assert.equal(codex.provider,"provider");
 console.log("Fork dialog selected-member/inherited-settings contract PASS");
})().catch(error=>{console.error(error);process.exitCode=1;});
