"use strict";
const assert = require("node:assert/strict");
const {createRequestDraft} = require("./static/preparation.js");
const baseURL = process.argv[2];
const id = number => `00000000-0000-4000-8000-${String(number).padStart(12,"0")}`;
const storage = () => { const values = new Map(); return {
  getItem: key => values.get(key) ?? null, setItem: (key,value) => values.set(key,value), removeItem: key => values.delete(key),
}; };
const send = (path, options) => fetch(baseURL+path, {...options, headers: {...options.headers, Origin:baseURL}});
(async () => {
  let number=0; const managed=process.argv[3]==="true";
  for (const provider of ["openai", "pool", ""]) {
      const requestID=id(++number), saved=storage(); let body;
      const options={storage:saved,randomUUID:()=>requestID,fields:{name:"",goal:"Browser admission request",date:"2026-10-18",
        provider,model:"model-solo",effort:"high",team:managed?"solo":"",catalogDigest:managed?"a".repeat(64):"",sensitive:"true",browserNotificationsEnabled:false},
        fetch:async (path,options)=>{ if(options.method==="POST")body=options.body; return send(path,options); }};
      const draft=createRequestDraft(options);
      const accepted=await draft.submit([],managed);
      assert.equal(accepted.requestId,requestID); assert.equal(accepted.state,"running");
      assert.equal(new URLSearchParams(body).get("provider"),provider || null);
      const replay=await send("/sessions",{method:"POST",headers:{Accept:"application/json","Content-Type":"application/x-www-form-urlencoded"},body});
      assert.equal(replay.status,202,await replay.text());
      const recovered=createRequestDraft({...options,randomUUID:()=>assert.fail("replacement identity")});
      assert.equal((await recovered.submit([],managed)).receiptId,accepted.receiptId);
      const post=async values=>{
        const response=await send("/sessions",{method:"POST",headers:{Accept:"application/json","Content-Type":"application/x-www-form-urlencoded"},body:values.toString()});
        return [response.status,await response.text()];
      };
      const changed=new URLSearchParams(body);changed.set("provider",provider==="openai"?"pool":"openai");
      assert.equal((await post(changed))[0],409,"changed provider must conflict with saved request");
      const duplicate=new URLSearchParams(body);duplicate.set("provider","openai");duplicate.append("provider","pool");
      assert.deepEqual((await post(duplicate)).map((v,i)=>i?v.includes("duplicate session request field"):v),[400,true]);
      const unknown=new URLSearchParams(body);unknown.set("unexpectedBrowserField","true");
      assert.deepEqual((await post(unknown)).map((v,i)=>i?v.includes("unknown session request field"):v),[400,true]);
      const invalid=new URLSearchParams(body);invalid.set("provider","missing-provider");
      assert.equal((await post(invalid))[0],400,"unconfigured providers must stay rejected");
  }
  console.log("Shipped browser serializer -> real HTTP admission/replay: 3 provider cases passed");
})().catch(error=>{console.error(error);process.exitCode=1;});
