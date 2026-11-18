import assert from 'node:assert/strict';
import test from 'node:test';
import {mkdtempSync,mkdirSync,rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {randomUUID} from 'node:crypto';
import {sdk,ai} from '../../test/pi_runtime/package-api.mjs';
import {createSession} from './session.mjs';

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

test('abort settles unconsumed native inputs Unknown and clears queues without replay',async()=>{
 const f=await fixture();const {worker,fake}=f;
 try {
  fake.setResponses([ai.fauxAssistantMessage([ai.fauxToolCall('fixture_gate',{})],{stopReason:'toolUse'}),ai.fauxAssistantMessage('finished')]);
  assert.equal((await worker.send('original','original')).outcome,'Accepted');await f.toolStarted;
  assert.deepEqual(worker.enqueueInput({message:'never consumed',inputOperationID:'queued',delivery:'followUp'}),{outcome:'Pending'});
  const waiting=worker.waitInput({inputOperationID:'queued'});
  const aborted=worker.abort();f.release();await aborted;
  assert.deepEqual(await waiting,{outcome:'Unknown'});assert.equal(worker.session.agent.hasQueuedMessages(),false);
  assert.equal(worker.held,true);
  assert.equal(worker.manager.getEntries().filter(e=>e.type==='message'&&e.message.role==='user').length,1);
 }finally{await f.close();}
});

test('lost commit hold settles pending slots without replay',async()=>{
 const f=await fixture();const {worker,fake}=f;
 try {
  fake.setResponses([ai.fauxAssistantMessage([ai.fauxToolCall('fixture_gate',{})],{stopReason:'toolUse'}),ai.fauxAssistantMessage('done')]);
  assert.equal((await worker.send('original','original')).outcome,'Accepted');await f.toolStarted;
  assert.deepEqual(worker.enqueueInput({message:'follow',inputOperationID:'follow',delivery:'followUp'}),{outcome:'Pending'});
  const waiting=worker.waitInput({inputOperationID:'follow'});
  assert.deepEqual(worker.holdInputs(),{held:true});
  assert.deepEqual(await waiting,{outcome:'Unknown'});
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
