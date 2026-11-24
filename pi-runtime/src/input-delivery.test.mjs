import assert from 'node:assert/strict';
import test from 'node:test';
import {mkdtempSync,mkdirSync,rmSync,readFileSync,writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {randomUUID} from 'node:crypto';
import {sdk,ai} from '../../test/pi_runtime/package-api.mjs';
import {createSession,WorkerSession} from './session.mjs';
import {syncHistoryPrefix} from './persistence.mjs';
import {HostBridge} from './host-bridge.mjs';

async function fixture() {
 const root=mkdtempSync(join(tmpdir(),'pi-native-input-'));
 for(const dir of ['repo','agent','history','auth']) mkdirSync(join(root,dir));
 const runtime=await sdk.ModelRuntime.create({authPath:join(root,'auth/auth.json'),modelsPath:null});
 const fake=ai.fauxProvider({provider:'input-fake',models:[{id:'model'}],tokensPerSecond:100000});
 runtime.registerNativeProvider(fake.provider);await runtime.refresh({allowNetwork:false});
 let release,started;
 const toolStarted=new Promise(resolve=>{started=resolve;});
 const gate=new Promise(resolve=>{release=resolve;});
 const tool={name:'fixture_gate',label:'gate',description:'offline gate',exposure:'direct',parameters:{type:'object',properties:{}},execute:async()=>{started();await gate;return {content:[{type:'text',text:'gate complete'}]};}};
 const config={nativeId:randomUUID(),emptyProven:true,cwd:join(root,'repo'),agentDir:join(root,'agent'),historyDir:join(root,'history'),authDirectory:join(root,'auth'),accountBindings:[{provider:'input-fake',account_ref:'fake'}],settings:{Provider:'input-fake',AccountRef:'fake',Model:'model',Thinking:'off',PolicyRevision:'fixture',Revision:1}};
 const worker=await createSession(sdk,config,{modelRuntime:runtime,sessionOptions:{tools:['fixture_gate'],customTools:[tool]}});
 return {worker,fake,toolStarted,release,async close(){release();await worker.abort();await worker.run;worker.session.dispose();await runtime.settleCredentialWrites();rmSync(root,{recursive:true});}};
}

test('idle explicit steer follows a completed legacy prompt with an exact durable witness',async()=>{
 const f=await fixture();const {worker,fake}=f;
 try {
  fake.setResponses([ai.fauxAssistantMessage('initial completed'),ai.fauxAssistantMessage('idle steer completed')]);
  assert.equal((await worker.send('initial input','initial')).outcome,'Accepted');
  await worker.run;
  assert.equal(worker.session.isStreaming,false);assert.equal(worker.pending,null);
  const literal='idle literal ${unchanged}';
  assert.deepEqual(worker.enqueueInput({message:literal,inputOperationID:'idle-steer',delivery:'steer'}),{outcome:'Pending'});
  const accepted=await worker.waitInput({inputOperationID:'idle-steer'});
  assert.equal(accepted.outcome,'Accepted');
  assert.deepEqual(worker.commitInput({inputOperationID:'idle-steer'}),{committed:true});
  await worker.run;
  const disk=sdk.SessionManager.open(worker.manager.getSessionFile());
  const users=disk.getEntries().filter(e=>e.type==='message'&&e.message.role==='user');
  assert.equal(users.length,2);assert.equal(users[1].id,accepted.witness.entryId);
  assert.deepEqual(users[1].message.content,[{type:'text',text:literal}]);
  assert.equal(worker.held,false);assert.equal(worker.inputs.size,0);
 }finally{await f.close();}
});

test('native Pending consumes exact steer before followUp with duplicate literal attachment references',async()=>{
 const f=await fixture();const {worker,fake}=f;
 try {
  fake.setResponses([ai.fauxAssistantMessage([ai.fauxToolCall('fixture_gate',{})],{stopReason:'toolUse'}),ai.fauxAssistantMessage('steer completed'),ai.fauxAssistantMessage('follow-up completed')]);
  assert.equal((await worker.send('initial input','initial')).outcome,'Accepted');
  await f.toolStarted;
  const literal='/not-a-template ${unchanged}\n[attachment: 11111111-1111-4111-8111-111111111111]';
  for(const [id,delivery] of [['follow','followUp'],['steer','steer']]) {
   assert.deepEqual(worker.enqueueInput({message:literal,inputOperationID:id,delivery}),{outcome:'Pending'});
   assert.equal(worker.inputs.get(id).finished,false);
  }
  assert.equal(worker.activeInput.operationID,'initial','enqueue cannot invalidate running callbacks');
  const follow=worker.waitInput({inputOperationID:'follow'}),steer=worker.waitInput({inputOperationID:'steer'});
  f.release();
  const acceptedSteer=await steer;assert.equal(acceptedSteer.outcome,'Accepted');
  assert.equal(worker.inputs.get('steer').committed,false);
  assert.deepEqual(worker.commitInput({inputOperationID:'steer'}),{committed:true});
  const acceptedFollow=await follow;assert.equal(acceptedFollow.outcome,'Accepted');
  assert.deepEqual(worker.commitInput({inputOperationID:'follow'}),{committed:true});
  assert.notEqual(acceptedSteer.witness.entryId,acceptedFollow.witness.entryId);
  const disk=sdk.SessionManager.open(worker.manager.getSessionFile());
  const users=disk.getEntries().filter(e=>e.type==='message'&&e.message.role==='user');
  assert.deepEqual(users.slice(1).map(e=>e.id),[acceptedSteer.witness.entryId,acceptedFollow.witness.entryId]);
  assert.deepEqual(users.slice(1).map(e=>e.message.content),Array(2).fill([{type:'text',text:literal}]));
  for(const value of [acceptedSteer,acceptedFollow]) assert.deepEqual(worker.verifyHistory({Witness:{SessionID:value.witness.sessionId,EntryID:value.witness.entryId,ParentID:value.witness.parentId,PreviousLeafID:value.witness.previousLeafId},Message:literal}),{Verified:true});
  await worker.run;assert.equal(worker.inputs.size,0);assert.equal(worker.held,false);
 }finally{await f.close();}
});

test('queued steer crosses append-only SDK compaction with one exact durable witness',{timeout:10000},async()=>{
 const f=await fixture();const {worker,fake}=f;
 try {
  fake.setResponses([ai.fauxAssistantMessage([ai.fauxToolCall('fixture_gate',{})],{stopReason:'toolUse'}),ai.fauxAssistantMessage('steer completed')]);
  assert.equal((await worker.send('initial input','initial')).outcome,'Accepted');await f.toolStarted;
  const message='owned steer after compaction',inputOperationID='queued-compaction';
  assert.deepEqual(worker.enqueueInput({message,inputOperationID,delivery:'steer'}),{outcome:'Pending'});
  const pending=worker.inputs.get(inputOperationID),file=worker.manager.getSessionFile();
  const bytes=readFileSync(file),prefix=pending.prefixJSON;
  const compaction=worker.manager.appendCompaction('retained fixture summary',worker.manager.getEntries().find(e=>e.type==='message').id,4096);
  assert.equal(worker.manager.getEntry(compaction).parentId,pending.leaf);
  assert.deepEqual(readFileSync(file).subarray(0,bytes.length),bytes);
  const waiting=worker.waitInput({inputOperationID});f.release();
  const accepted=await waiting;assert.equal(accepted.outcome,'Accepted');
  assert.deepEqual(await worker.waitInput({inputOperationID}),accepted,'observation never submits another input');
  const disk=sdk.SessionManager.open(file),entry=disk.getEntry(accepted.witness.entryId);
  assert.equal(pending.operationID,inputOperationID);assert.equal(pending.observed.length,1);
  assert.equal(worker.manager.getEntry(entry.id).message,pending.object);
  assert.equal(JSON.stringify(disk.getEntries().slice(0,pending.prefixLength)),prefix);
  assert.equal(disk.getEntry(compaction).type,'compaction');
  assert.deepEqual(worker.verifyHistory({Witness:{SessionID:accepted.witness.sessionId,EntryID:entry.id,ParentID:entry.parentId,PreviousLeafID:pending.leaf},Message:message}),{Verified:true});
  assert.deepEqual(worker.commitInput({inputOperationID}),{committed:true});await worker.run;
  const users=sdk.SessionManager.open(file).getEntries().filter(e=>e.type==='message'&&e.message.role==='user');
  assert.equal(users.length,2);assert.equal(users.filter(e=>e.id===entry.id).length,1);
  assert.equal(worker.held,false);assert.equal(worker.inputs.size,0);
 }finally{await f.close();}
});

test('queued steer still refuses an unwitnessed foreign user ancestor',{timeout:10000},async()=>{
 const f=await fixture();const {worker,fake}=f;
 try {
  fake.setResponses([ai.fauxAssistantMessage([ai.fauxToolCall('fixture_gate',{})],{stopReason:'toolUse'}),ai.fauxAssistantMessage('done')]);
  assert.equal((await worker.send('initial input','initial')).outcome,'Accepted');await f.toolStarted;
  const message='owned steer',inputOperationID='foreign-ancestor';
  assert.equal(worker.enqueueInput({message,inputOperationID,delivery:'steer'}).outcome,'Pending');
  const previousLeaf=worker.inputs.get(inputOperationID).leaf;
  worker.manager.appendMessage({role:'user',content:[{type:'text',text:'foreign input'}],timestamp:Date.now()});
  const waiting=worker.waitInput({inputOperationID});f.release();
  assert.deepEqual(await waiting,{outcome:'Unknown'});assert.equal(worker.held,true);await worker.run;
  const disk=sdk.SessionManager.open(worker.manager.getSessionFile());
  const entry=disk.getEntries().find(e=>e.type==='message'&&e.message.role==='user'&&e.message.content[0]?.text===message);
  assert(entry);
  assert.deepEqual(worker.verifyHistory({Witness:{SessionID:disk.getSessionId(),EntryID:entry.id,ParentID:entry.parentId,PreviousLeafID:previousLeaf},Message:message}),{Verified:false});
 }finally{await f.close();}
});

test('abort settles unconsumed native inputs Unknown and clears queues without replay',async()=>{
 const f=await fixture();const {worker,fake}=f;
 try {
  fake.setResponses([ai.fauxAssistantMessage([ai.fauxToolCall('fixture_gate',{})],{stopReason:'toolUse'}),ai.fauxAssistantMessage('finished')]);
  assert.equal((await worker.send('original','original')).outcome,'Accepted');await f.toolStarted;
  assert.deepEqual(worker.enqueueInput({message:'never consumed',inputOperationID:'queued',delivery:'followUp'}),{outcome:'Pending'});
  const input=worker.inputs.get('queued'),prefix=input.prefixJSON;
  const waiting=worker.waitInput({inputOperationID:'queued'});
  const aborted=worker.abort();f.release();await aborted;
  assert.deepEqual(await waiting,{outcome:'Unknown'});assert.equal(worker.session.agent.hasQueuedMessages(),false);
  assert.equal(worker.held,true);
  assert.throws(()=>worker.commitInput({inputOperationID:'queued'}),/unwitnessed native input/);
  assert.equal(input.prefixJSON,prefix);
  assert.equal(worker.manager.getEntries().filter(e=>e.type==='message'&&e.message.role==='user').length,1);
 }finally{await f.close();}
});

test('lost commit hold settles pending slots without replay',async()=>{
 const f=await fixture();const {worker,fake}=f;
 try {
  fake.setResponses([ai.fauxAssistantMessage([ai.fauxToolCall('fixture_gate',{})],{stopReason:'toolUse'}),ai.fauxAssistantMessage('done')]);
  assert.equal((await worker.send('original','original')).outcome,'Accepted');await f.toolStarted;
  assert.deepEqual(worker.enqueueInput({message:'follow',inputOperationID:'follow',delivery:'followUp'}),{outcome:'Pending'});
  const input=worker.inputs.get('follow'),prefix=input.prefixJSON;
  const waiting=worker.waitInput({inputOperationID:'follow'});
  assert.deepEqual(worker.holdInputs(),{held:true});
  assert.deepEqual(await waiting,{outcome:'Unknown'});
  assert.throws(()=>worker.commitInput({inputOperationID:'follow'}),/unwitnessed native input/);
  assert.equal(input.prefixJSON,prefix);
  assert.deepEqual(worker.enqueueInput({message:'never replay',inputOperationID:'new',delivery:'steer'}),{outcome:'Unknown'});
  assert.equal(worker.session.agent.hasQueuedMessages(),false);f.release();await worker.run;
 }finally{await f.close();}
});

test('native registry refuses twenty-first pending input and abort settles every waiter',async()=>{
 const f=await fixture();const {worker,fake}=f;
 try {
  fake.setResponses([ai.fauxAssistantMessage([ai.fauxToolCall('fixture_gate',{})],{stopReason:'toolUse'}),ai.fauxAssistantMessage('done')]);
  assert.equal((await worker.send('original','original')).outcome,'Accepted');await f.toolStarted;
  const waits=[];
  for(let i=0;i<20;i++) {
   assert.deepEqual(worker.enqueueInput({message:'same',inputOperationID:String(i),delivery:'followUp'}),{outcome:'Pending'});
   waits.push(worker.waitInput({inputOperationID:String(i)}));
  }
  assert.deepEqual(worker.enqueueInput({message:'excess',inputOperationID:'excess',delivery:'steer'}),{outcome:'NotSubmitted',refusal:'native input capacity reached'});
  const aborting=worker.abort();f.release();await aborting;
  assert.deepEqual(await Promise.all(waits),Array(20).fill({outcome:'Unknown'}));
  assert.equal(worker.session.agent.hasQueuedMessages(),false);
 }finally{await f.close();}
});

test('changed admitted native prefix cannot produce an Accepted queued witness',async()=>{
 const f=await fixture();const {worker,fake}=f;
 try {
  fake.setResponses([ai.fauxAssistantMessage([ai.fauxToolCall('fixture_gate',{})],{stopReason:'toolUse'}),ai.fauxAssistantMessage('done')]);
  assert.equal((await worker.send('original','original')).outcome,'Accepted');await f.toolStarted;
  assert.deepEqual(worker.enqueueInput({message:'follow',inputOperationID:'follow',delivery:'followUp'}),{outcome:'Pending'});
  const waiting=worker.waitInput({inputOperationID:'follow'});
  worker.manager.getEntries()[0].timestamp='changed admitted prefix';
  f.release();assert.deepEqual(await waiting,{outcome:'Unknown'});assert.equal(worker.held,true);
 }finally{await f.close();}
});

test('committed queued input releases its prefix without losing retained callback provenance',{timeout:10000},async()=>{
 const f=await fixture();const {worker,fake}=f;
 try{
  const call=ai.fauxToolCall('fixture_gate',{});
  fake.setResponses([ai.fauxAssistantMessage([ai.fauxToolCall('fixture_gate',{})],{stopReason:'toolUse'}),
   ai.fauxAssistantMessage([call],{stopReason:'toolUse'}),ai.fauxAssistantMessage('finished')]);
  assert.equal((await worker.send('retained history '.repeat(2048),'legacy')).outcome,'Accepted');
  await f.toolStarted;
  const legacy=worker.activeInput,legacyPrefix=legacy.prefixJSON;
  assert.equal(worker.enqueueInput({message:'exact retained input',inputOperationID:'retained',delivery:'steer'}).outcome,'Pending');
  const input=worker.inputs.get('retained'),prefix=input.prefixJSON;
  assert(prefix.length>32768);assert.equal(input.finished,false);
  assert.throws(()=>worker.commitInput({inputOperationID:'retained'}),/unwitnessed native input/);
  assert.equal(input.prefixJSON,prefix);
  f.release();
  const accepted=await worker.waitInput({inputOperationID:'retained'});
  assert.equal(accepted.outcome,'Accepted');assert.equal(input.finished,true);
  assert.equal(input.prefixJSON,prefix,'Accepted alone must retain the admitted prefix');
  assert.equal(worker.activeInput,input);
  const {prefixJSON,committed,...provenance}=input;
  let notifications=0;worker.listeners.add(()=>{notifications++;});
  assert.deepEqual(worker.commitInput({inputOperationID:'retained'}),{committed:true});
  assert.equal(notifications,1);
  assert.equal(worker.acceptedInputs.get('retained'),input);assert.equal(worker.activeInput,input);
  assert.equal(input.prefixJSON,undefined);assert.equal(input.committed,true);
  assert.deepEqual((({prefixJSON,committed,...rest})=>rest)(input),provenance);
  assert.equal(legacy.prefixJSON,legacyPrefix,'legacy send lifetime is unchanged');
  await worker.observePersisted(input); // A delayed observation must stop at its owning/finished gate.
  await worker.run;
  assert.deepEqual(input.witness,accepted.witness);
  const Witness={SessionID:accepted.witness.sessionId,EntryID:accepted.witness.entryId,
   ParentID:accepted.witness.parentId,PreviousLeafID:accepted.witness.previousLeafId};
  const file=worker.manager.getSessionFile(),bytes=readFileSync(file);
  assert.deepEqual(worker.verifyHistory({Witness,Message:input.message}),{Verified:true});
  const subject={Kind:'conversation',Conversation:{Conversation:{Session:{Workspace:'/fixture',Slug:'2026-11-23-fixture',Incarnation:'a'.repeat(64)},
   Backend:'pi',Member:'lead',Incarnation:'b'.repeat(64),NativeID:accepted.witness.sessionId},Generation:'c'.repeat(64)}};
  let frame;
  const bridge=new HostBridge({sdk,manager:worker.manager,subject,held:()=>worker.held,
   getInput:id=>id?worker.inputs.get(id)??worker.acceptedInputs.get(id)??(worker.activeInput?.operationID===id?worker.activeInput:null):worker.activeInput,
   emit(value){frame=value;bridge.result(value.id,{Outcome:'result',Body:{retained:true}});}});
  assert.deepEqual(await bridge.call('fixture_gate',{}, {nativeToolCallID:call.id}),{retained:true});
  assert.equal(frame.event,'host_call');assert.equal(frame.body.inputOperationID,'retained');
  const verified=bridge.verify(frame.body);
  assert.equal(verified.Verified,true);assert.equal(verified.AnchorEntryID,frame.body.anchorEntryID);
  assert.equal(verified.SelectionToken,frame.body.selectionToken);
  assert.throws(()=>bridge.verify({...frame.body,arguments:{changed:true}}),/native tool origin mismatch/);
  assert.deepEqual(readFileSync(file),bytes,'retained verification must not rewrite or replay history');
  assert.equal(sdk.SessionManager.open(file).getEntries().filter(e=>e.type==='message'&&e.message.role==='user').length,2);
  assert.equal(worker.inputs.size,0);assert.equal(worker.held,false);
 }finally{await f.close();}
});

test('committed registry retains no prefix strings while independent pending input and capacity survive',async()=>{
 const root=mkdtempSync(join(tmpdir(),'pi-input-prefix-')),file=join(root,'history.json');
 const entries=[{id:'root',parentId:null,type:'model_change',metadata:'substantial prefix '.repeat(2048)}];
 const view=values=>({getEntries:()=>values,getBranch:()=>values,getLeafId:()=>values.at(-1)?.id,
  getEntry:id=>values.find(entry=>entry.id===id),getSessionFile:()=>file,getSessionId:()=> 'owned',getCwd:()=>root});
 const worker=new WorkerSession({SessionManager:{open:()=>view(JSON.parse(readFileSync(file,'utf8')))}},
  {subscribe(){},isStreaming:false,clearQueue(){},async abort(){},agent:{hasQueuedMessages:()=>false}},view(entries));
 try{
  const waiting=worker.makeInput('still pending','pending','followUp'),prefix=waiting.prefixJSON;
  assert(prefix.length>32768);
  for(let i=0;i<22;i++){
   const input=worker.makeInput('literal '+i,'committed-'+i,'steer');
   const frozen=input.prefixJSON,object={role:'user',content:[{type:'text',text:input.message}]};
   input.object=object;input.observed.push(object);
   entries.push({id:'entry-'+i,parentId:input.leaf,type:'message',message:object});
   writeFileSync(file,JSON.stringify(entries),{mode:0o600});
   await worker.observePersisted(input);assert.equal((await input.result).outcome,'Accepted');
   assert.equal(input.prefixJSON,frozen);
   worker.commitInput({inputOperationID:input.operationID});
   assert.equal(worker.acceptedInputs.get(input.operationID),input);
   assert([...worker.acceptedInputs.values()].every(value=>value.prefixJSON===undefined));
   assert.equal(worker.inputs.get('pending'),waiting);assert.equal(waiting.prefixJSON,prefix);
  }
  assert.equal(worker.acceptedInputs.size,20);
  assert.deepEqual([...worker.acceptedInputs.keys()],Array.from({length:20},(_,i)=>'committed-'+(i+2)));
  assert.equal(worker.inputs.size,1);assert.equal(waiting.finished,false);assert.equal(waiting.witness,undefined);
  assert.throws(()=>worker.commitInput({inputOperationID:'pending'}),/unwitnessed native input/);
  assert.equal(waiting.prefixJSON,prefix);
  await worker.abort();assert.deepEqual(await waiting.result,{outcome:'Unknown'});
  assert.equal(waiting.prefixJSON,prefix);
 }finally{rmSync(root,{recursive:true});}
});


// Exercise makeInput/observePersisted themselves against their prior comparison
// expressions. These local JSON histories test representation boundaries; the
// pinned SDK cases above own actual native consumption and persistence proof.
test('input history comparison matches the prior oracle without weakening full disk proof',async(t)=>{
 const texts=['plain', 'é雪😀𝄞', 'quotes" backslash\\ controls\n\t\u0000', 'embedded ],[{"id":"fake"}], delimiters ,]'];
 const changes=['extended','equal','shortened','prefix-changed','prefix-reordered','key-order','undefined-omitted','disk-tail-changed'];
 for(const text of texts) for(const change of changes) await t.test(`${change}: ${JSON.stringify(text)}`,async()=>{
  const root=mkdtempSync(join(tmpdir(),'pi-input-compare-'));
  const file=join(root,'history.json');
  const prefix=[{id:'root',parentId:null,type:'model_change',metadata:{text,optional:undefined,number:-0}},
    {id:'previous',parentId:'root',type:'message',message:{role:'assistant',content:[{type:'text',text}]}}];
  let entries=prefix;
  const view=(values)=>({getEntries:()=>values,getBranch:()=>values,getLeafId:()=>values.at(-1)?.id,
    getEntry:(id)=>values.find(entry=>entry.id===id),getSessionFile:()=>file,getSessionId:()=> 'owned',getCwd:()=>root});
  const manager={getEntries:()=>entries,getBranch:()=>entries,getLeafId:()=>entries.at(-1)?.id,
    getEntry:(id)=>entries.find(entry=>entry.id===id),getSessionFile:()=>file,getSessionId:()=> 'owned',getCwd:()=>root};
  let opens=0,syncs=0,holds=0;
  const session={subscribe(){},isStreaming:false,clearQueue(){},async abort(){holds++;},agent:{hasQueuedMessages:()=>false}};
  const worker=new WorkerSession({SessionManager:{open(path){assert.equal(path,file);opens++;return view(JSON.parse(readFileSync(path,'utf8')));}}},session,manager,
    {historyDirectory:root,syncHistory(path,identity){syncHistoryPrefix(path,identity);syncs++;}});
  try {
   const pending=worker.makeInput('exact consumed literal','original','steer');
   const frozen=pending.prefixJSON;
   assert.equal(frozen,JSON.stringify(prefix));assert.equal(pending.prefixLength,2);assert.equal(pending.leaf,'previous');
   const object={role:'user',content:[{type:'text',text:pending.message}]};
   const consumed={id:'consumed',parentId:'previous',type:'message',message:object};
   pending.object=object;pending.observed.push(object);
   entries=[...prefix,consumed];
   switch(change){
    case 'equal':entries=prefix;break;
    case 'shortened':entries=prefix.slice(0,1);break;
    case 'prefix-changed':prefix[0].metadata.text+=' changed after capture';break;
    case 'prefix-reordered':entries=[{...prefix[1],parentId:null},{...prefix[0],parentId:'previous'},{...consumed,parentId:'root'}];break;
    case 'key-order':entries[0]={type:prefix[0].type,parentId:null,id:'root',metadata:prefix[0].metadata};break;
    case 'undefined-omitted':delete prefix[0].metadata.optional;break;
   }
   const disk=JSON.parse(JSON.stringify(entries));
   if(change==='disk-tail-changed') disk.at(-1).message.content[0].text+=' changed only on disk';
   writeFileSync(file,JSON.stringify(disk),{mode:0o600});
   const bytes=readFileSync(file);
   const oldPrefix=JSON.stringify(entries.slice(0,pending.prefixLength))===frozen;
   const oldDisk=JSON.stringify(disk)===JSON.stringify(entries);
   assert.equal(oldPrefix,!['shortened','prefix-changed','prefix-reordered','key-order'].includes(change));
   assert.equal(oldDisk,change!=='disk-tail-changed');
   await worker.observePersisted(pending);
   const result=await pending.result;
   const accepted=oldPrefix&&oldDisk&&entries.some(entry=>entry.message===object);
   assert.equal(result.outcome,accepted?'Accepted':'Unknown');
   assert.equal(syncs,accepted?1:0);assert.equal(worker.held,!accepted);assert.equal(holds,accepted?0:1);
   assert.equal(pending.prefixJSON,frozen,'live mutation must not refresh admitted bytes');
   assert.deepEqual(readFileSync(file),bytes,'verification cannot rewrite disk history');
   if(accepted){
    assert.deepEqual(result.witness,{sessionId:'owned',entryId:'consumed',parentId:'previous',previousLeafId:'previous',effectiveMessage:pending.message});
    assert.equal(opens,1);assert.deepEqual(worker.commitInput({inputOperationID:'original'}),{committed:true});
   }else{
    assert.equal(result.witness,undefined);assert.equal(pending.witness,undefined);
    assert.throws(()=>worker.commitInput({inputOperationID:'original'}),/unwitnessed native input/);
    assert.equal(opens,change==='disk-tail-changed'?1:0);
   }
  }finally{rmSync(root,{recursive:true});}
 });
});

test('each input observation uses fresh live and disk history and holds serialization failures',async(t)=>{
 for(const fault of ['disk-tail','live-serialization']) await t.test(fault,async()=>{
 const root=mkdtempSync(join(tmpdir(),'pi-input-fresh-')),file=join(root,'history.json');
 let entries=[{id:'root',parentId:null,type:'model_change'}];
 const view=(values)=>({getEntries:()=>values,getBranch:()=>values,getLeafId:()=>values.at(-1)?.id,
  getEntry:(id)=>values.find(entry=>entry.id===id),getSessionFile:()=>file,getSessionId:()=> 'owned',getCwd:()=>root});
 const manager={...view(entries),getEntries:()=>entries,getBranch:()=>entries,getLeafId:()=>entries.at(-1)?.id,getEntry:(id)=>entries.find(entry=>entry.id===id)};
 let opens=0,syncs=0;
 const session={subscribe(){},isStreaming:false,clearQueue(){},async abort(){},agent:{hasQueuedMessages:()=>false}};
 const worker=new WorkerSession({SessionManager:{open(){opens++;return view(JSON.parse(readFileSync(file,'utf8')));}}},session,manager,
  {historyDirectory:root,syncHistory(path,identity){syncHistoryPrefix(path,identity);syncs++;}});
 try{
  const observe=async(id,mutate)=>{
   const pending=worker.makeInput('literal '+id,id,'followUp');
   const object={role:'user',content:[{type:'text',text:pending.message}]};
   entries.push({id,parentId:pending.leaf,type:'message',message:object});pending.object=object;pending.observed.push(object);
   writeFileSync(file,JSON.stringify(entries),{mode:0o600});mutate?.();
   await worker.observePersisted(pending);return {pending,result:await pending.result};
  };
  const first=await observe('first');assert.equal(first.result.outcome,'Accepted');worker.commitInput({inputOperationID:'first'});
  const second=await observe('second',()=>{
   if(fault==='live-serialization') entries[0].invalid=1n;
   else {const disk=JSON.parse(readFileSync(file,'utf8'));disk.at(-1).message.content[0].text='new disk mismatch';writeFileSync(file,JSON.stringify(disk));}
  });
  assert.deepEqual(second.result,{outcome:'Unknown'});assert.equal(worker.held,true);assert.equal(opens,fault==='disk-tail'?2:1);assert.equal(syncs,1);
  assert.throws(()=>worker.commitInput({inputOperationID:'second'}),/unwitnessed native input/);
  assert.deepEqual(worker.enqueueInput({message:'do not replay',inputOperationID:'third',delivery:'steer'}),{outcome:'Unknown'});
  entries=[];assert.throws(()=>worker.makeInput('empty','empty','steer'),/empty native selection/);
 }finally{rmSync(root,{recursive:true});}
 });
});
