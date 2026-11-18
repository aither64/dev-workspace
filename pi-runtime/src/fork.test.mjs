import test from 'node:test';
import assert from 'node:assert/strict';
import * as fs from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { WorkerSession } from './session.mjs';
import { captureFork, initializeFork, verifyFork } from './fork.mjs';

test('fork copy is synced, exact-ID recovery is source independent, prefixes survive later input', () => {
  const root = fs.mkdtempSync(join(tmpdir(), 'pi-fork-'));
  const source = join(root, 'source.jsonl'), historyDir = join(root, 'destination');fs.mkdirSync(historyDir);
  const id = '11111111-1111-4111-8111-111111111111', target = '22222222-2222-4222-8222-222222222222';
  const header = {type:'session',version:3,id,timestamp:'2026-11-18T00:00:00.000Z',cwd:root};
  const entries = [{type:'message',id:'a',parentId:null,message:{role:'user',content:[{type:'text',text:'original'}]}},{type:'message',id:'b',parentId:'a',message:{role:'assistant',content:[{type:'text',text:'context'}]}}];
  const write = (file, values) => fs.writeFileSync(file, values.map(x=>JSON.stringify(x)).join('\n')+'\n');write(source,[header,...entries]);
  let copies=0,syncs=0;
  const sdk={SessionManager:{open(file){
    const values=fs.readFileSync(file,'utf8').trim().split('\n').map(x=>JSON.parse(x));const [h,...es]=values;
    return {getHeader:()=>h,getSessionId:()=>h.id,getCwd:()=>h.cwd,getSessionFile:()=>file,getEntries:()=>es,getBranch:()=>es,getLeafId:()=>es.at(-1)?.id};
  },forkFrom(file,cwd,dir,{id}){copies++;const es=fs.readFileSync(file,'utf8').trim().split('\n').map(x=>JSON.parse(x)).slice(1);const to=join(dir,`timestamp_${id}.jsonl`);write(to,[{...header,id,cwd,parentSession:file},...es]);return this.open(to);}}};
  const sync = (file,stat) => {assert.equal(stat.ino,fs.statSync(file).ino);syncs++;};
  try {
    const before=fs.readFileSync(source);const origin=captureFork(sdk,sdk.SessionManager.open(source));
    const config={fork:origin,nativeId:target,cwd:historyDir,historyDir};const manager=initializeFork(sdk,config,sync);
    assert.deepEqual(fs.readFileSync(source),before);assert.equal(copies,1);assert.ok(syncs);
    fs.renameSync(source,source+'.unavailable');
    assert.equal(initializeFork(sdk,config,sync).getSessionId(),target);assert.equal(copies,1);
    fs.appendFileSync(manager.getSessionFile(),JSON.stringify({type:'message',id:'c',parentId:'b',message:{role:'user',content:[]}})+'\n');
    assert.equal(verifyFork(sdk,sdk.SessionManager.open(manager.getSessionFile()),origin,sync).synced,true);
    assert.throws(()=>initializeFork(sdk,config,sync),'initialization must not adopt later appends');
    const saved=fs.readFileSync(manager.getSessionFile());fs.writeFileSync(manager.getSessionFile(),'');assert.throws(()=>initializeFork(sdk,config,sync));assert.equal(fs.statSync(manager.getSessionFile()).size,0);fs.writeFileSync(manager.getSessionFile(),saved);fs.appendFileSync(manager.getSessionFile(),'{partial');
    assert.throws(()=>initializeFork(sdk,config,sync));assert.equal(copies,1);fs.writeFileSync(manager.getSessionFile(),saved);
    fs.writeFileSync(join(historyDir,`other_${target}.jsonl`),saved);assert.throws(()=>initializeFork(sdk,config,sync));assert.equal(copies,1);
    fs.unlinkSync(join(historyDir,`other_${target}.jsonl`));fs.renameSync(source+'.unavailable',source);
    const driftDir=join(root,'drift');fs.mkdirSync(driftDir);write(source,[header,...entries.slice(0,1)]);
    assert.throws(()=>initializeFork(sdk,{...config,historyDir:driftDir},sync));assert.deepEqual(fs.readdirSync(driftDir),[]);
  } finally {fs.rmSync(root,{recursive:true});}
});

test('fork quiescence refuses queued or asynchronous work without aborting it', () => {
 const state={checkInstructions(){},session:{isStreaming:false,agent:{hasQueuedMessages:()=>false}},inputs:new Map(),held:false};
 const check=()=>WorkerSession.prototype.assertForkQuiescent.call(state);
 check();state.session.agent.hasQueuedMessages=()=>true;assert.throws(check);
 state.session.agent.hasQueuedMessages=()=>false;state.session.isStreaming=true;assert.throws(check);
 state.session.isStreaming=false;state.inputs.set('pending',{});assert.throws(check);
 state.inputs.clear();state.toolPolicy={workflow:()=>({held:false,running:false,activeCalls:1,proof:{Applied:true}})};assert.throws(check);
});
