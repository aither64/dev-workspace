import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync, readFileSync } from 'node:fs';
import { createHash, randomUUID } from 'node:crypto';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';
import { sdk, ai } from '../../test/pi_runtime/package-api.mjs';
import { createSession, WorkerSession } from './session.mjs';
import {toolPolicyDigest} from './tool-policy.mjs';

test('role instructions observe the actual public loader and effective SDK construction', async () => {
  const root = mkdtempSync(join(tmpdir(), 'pi-p3-role-interface-'));
  for (const dir of ['agent', 'history', 'repo', 'auth', 'repo/.pi']) mkdirSync(join(root, dir));
  const base = 'existing append marker\n';
  writeFileSync(join(root, 'repo/.pi/APPEND_SYSTEM.md'), base);
  writeFileSync(join(root, 'repo/AGENTS.md'), 'workspace AGENTS marker\n');
  const pathRole = join(root, 'role-source.md');
  writeFileSync(pathRole, 'path contents must never become the role');
  const runtime = await sdk.ModelRuntime.create({authPath: join(root, 'auth/auth.json'), modelsPath: null});
  const fake = ai.fauxProvider({provider: 'p3-fake', models: [{id: 'p3-model'}]});
  runtime.registerNativeProvider(fake.provider); await runtime.refresh({allowNetwork: false});
  const policy = {Mode: 'default', Revision: 1, Active: [], RegisteredCeiling: []}; policy.Digest = toolPolicyDigest(policy);
  const config = {nativeId: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', emptyProven: true, generation: 'b'.repeat(64),
    callbackSubject: {Kind: 'conversation'}, toolPolicy: policy, cwd: join(root, 'repo'), agentDir: join(root, 'agent'),
    historyDir: join(root, 'history'), authDirectory: join(root, 'auth'),
    accountBindings: [{provider: 'p3-fake', account_ref: 'fake-account'}],
    settings: {Provider: 'p3-fake', AccountRef: 'fake-account', Model: 'p3-model', Thinking: 'off', PolicyRevision: 'role-fixture', Revision: 1}};
  let worker;
  try {
    for (const literal of [pathRole, 'literal é\nrole\r\n']) {
      worker = await createSession(sdk, {...config, instructions: literal}, {modelRuntime: runtime, sessionOptions: {tools: []}});
      assert.deepEqual(worker.session.resourceLoader.getAppendSystemPrompt(), [base, literal]);
      assert.equal(worker.snapshot().instructionsDigest, createHash('sha256').update(literal, 'utf8').digest('hex'));
      assert.match(worker.session.systemPrompt, /You are a coding assistant/);
      assert.match(worker.session.systemPrompt, /workspace AGENTS marker/);
      assert.match(worker.session.systemPrompt, /existing append marker/);
      assert.equal(worker.session.systemPrompt.includes('path contents must never become the role'), false);
      assert.equal(worker.session.systemPrompt.split(literal).length - 1, 1);
      await worker.dispose(); worker = null;
    }
    await assert.rejects(createSession(sdk, {...config, instructions: pathRole}, {modelRuntime: runtime,
      resourceOptions: {systemPromptOverride: () => 'replacement'}}), /construction unavailable/);
    await assert.rejects(createSession(sdk, {...config, instructions: pathRole}, {modelRuntime: runtime,
      sessionOptions: {resourceLoader: {}}}), /construction unavailable/);
    writeFileSync(join(root, 'repo/.pi/SYSTEM.md'), 'genuine custom prompt marker');
    worker = await createSession(sdk, {...config, instructions: pathRole}, {modelRuntime: runtime, sessionOptions: {tools: []}});
    assert.equal(worker.session.systemPrompt.startsWith('genuine custom prompt marker'), true);
    assert.match(worker.session.systemPrompt, /workspace AGENTS marker/);
    assert.deepEqual(worker.session.resourceLoader.getAppendSystemPrompt(), [base, pathRole]);
    worker.session.resourceLoader.getAppendSystemPrompt()[1] = 'changed after init';
    assert.throws(() => worker.snapshot(), /role instructions unavailable/);
    assert.deepEqual(await worker.send('must not submit', 'c'.repeat(32)), {outcome: 'Unknown'});
    assert.equal(worker.pending, null); assert.equal(worker.activeInput, undefined);
    worker.session.dispose(); worker = null;
  } finally {
    if (worker) { await worker.abort(); await worker.run; worker.session.dispose(); }
    await runtime.settleCredentialWrites(); rmSync(root, {recursive: true});
  }
});

test('ordinary fresh and retained input has exact independent persisted witness before acknowledgement', async () => {
  const root = mkdtempSync(join(tmpdir(), 'pi-p3-witness-'));
  const dirs = ['agent', 'history', 'repo', 'auth'];
  for (const dir of dirs) mkdirSync(join(root, dir));
  const runtime = await sdk.ModelRuntime.create({ authPath: join(root, 'auth/auth.json'), modelsPath: null });
  const fake = ai.fauxProvider({ provider: 'p3-fake', models: [{ id: 'p3-model' }, { id: 'p3-other' }] });
  runtime.registerNativeProvider(fake.provider); await runtime.refresh({ allowNetwork: false });
  const config = { nativeId: '11111111-1111-4111-8111-111111111111', emptyProven: true,
    cwd: join(root, 'repo'), agentDir: join(root, 'agent'), historyDir: join(root, 'history'), authDirectory: join(root, 'auth'),
    accountBindings: [{ provider: 'p3-fake', account_ref: 'fake-account' }],
    settings: { Provider: 'p3-fake', AccountRef: 'fake-account', Model: 'p3-model', Thinking: 'off', PolicyRevision: 'fake-policy', Revision: 1 } };
  let worker;
  try {
    worker = await createSession(sdk, config, { modelRuntime: runtime, sessionOptions: { tools: [] } });
    const witnesses = [];
    let liveTextObserved = false;
    worker.listeners.add((event) => {
      if (event.type === 'message_update') {
        const live = worker.snapshot().liveMessage;
        if (live?.role === 'assistant' && live.content?.some((block) => block.type === 'text' && block.text)) liveTextObserved = true;
      }
    });
    for (let i = 0; i < 2; i++) {
      fake.setResponses([ai.fauxAssistantMessage('fake answer')]);
      const accepted = await worker.send('identical input');
      assert.equal(accepted.outcome, 'Accepted');
      const disk = sdk.SessionManager.open(worker.snapshot().historyFile);
      assert.equal(disk.getEntry(accepted.witness.entryId).message.role, 'user');
      witnesses.push(accepted.witness.entryId);
      await worker.run;
    }
    assert.notEqual(witnesses[0], witnesses[1]);
    assert.equal(liveTextObserved, true, 'public streaming events expose the current assistant snapshot');
    const next = { ...config.settings, Model: 'p3-other', Revision: 2 };
    assert.equal((await worker.configure(next)).Outcome, 'applied');
    assert.equal((await worker.configure(next)).Outcome, 'applied');
    assert.equal((await worker.configure({ ...next, Model: 'p3-model' })).Outcome, 'rejected');
    assert.equal((await worker.configure({ ...next, Revision: 4 })).Outcome, 'rejected');
    assert.equal((await worker.configure({ ...next, Revision: 3, AccountRef: 'unproved-account' })).Outcome, 'rejected');
    fake.setResponses([async (_context, options) => {
      await delay(10000, undefined, { signal: options.signal });
      return ai.fauxAssistantMessage('should be aborted');
    }]);
    const abortAccepted = await worker.send('finite abort input');
    assert.equal(abortAccepted.outcome, 'Accepted');
    assert.equal(worker.snapshot().busy, true);
    const abortResult = await worker.abort();
    await worker.run;
    assert.equal(abortResult.idle, true);
    assert.equal(worker.snapshot().busy, false);
    const historyFile = worker.snapshot().historyFile;
    await worker.dispose();
    worker = await createSession(sdk, { ...config, historyFile }, { modelRuntime: runtime, sessionOptions: { tools: [] } });
    assert.equal(worker.snapshot().nativeId, config.nativeId);
    fake.setResponses([ai.fauxAssistantMessage('after reload')]);
    assert.equal((await worker.send('identical input')).outcome, 'Accepted');
    await worker.run; await worker.dispose(); worker = null;
    await assert.rejects(createSession(sdk, { ...config, historyFile: join(root, 'missing') }, { modelRuntime: runtime }), /missing/);
  } finally {
    if (worker) { await worker.abort(); await worker.run; if (!worker.held) await worker.dispose(); }
    await runtime.settleCredentialWrites(); rmSync(root, { recursive: true });
  }
});

test('two configured providers switch on the same native history without global defaults or new input', async () => {
  const root = mkdtempSync(join(tmpdir(), 'pi-p3-switch-'));
  for (const dir of ['agent', 'history', 'repo', 'auth']) mkdirSync(join(root, dir));
  const runtime = await sdk.ModelRuntime.create({ authPath: join(root, 'auth/auth.json'), modelsPath: null });
  const a = ai.fauxProvider({ provider: 'provider-a', models: [{ id: 'same-model' }] });
  const b = ai.fauxProvider({ provider: 'provider-b', models: [{ id: 'same-model' }] });
  runtime.registerNativeProvider(a.provider); runtime.registerNativeProvider(b.provider);
  await runtime.refresh({ allowNetwork: false });
  const config = { nativeId: '22222222-2222-4222-8222-222222222222', emptyProven: true,
    cwd: join(root, 'repo'), agentDir: join(root, 'agent'), historyDir: join(root, 'history'), authDirectory: join(root, 'auth'),
    accountBindings: [{ provider: 'provider-a', account_ref: 'account-a' }, { provider: 'provider-b', account_ref: 'account-b' }],
    settings: { Provider: 'provider-a', AccountRef: 'account-a', Model: 'same-model', Thinking: 'off', PolicyRevision: 'switch-policy', Revision: 1 } };
  let worker;
  try {
    worker = await createSession(sdk, config, { modelRuntime: runtime, sessionOptions: { tools: [] } });
    a.setResponses([ai.fauxAssistantMessage('from a')]);
    assert.equal((await worker.send('original first input')).outcome, 'Accepted'); await worker.run;
    const file = worker.snapshot().historyFile;
    const users = () => worker.manager.getEntries().filter((entry) => entry.type === 'message' && entry.message.role === 'user').length;
    const originalUsers = users();
    const defaults = structuredClone(worker.storageSettings.getGlobalSettings());
    const target = { ...config.settings, Provider: 'provider-b', AccountRef: 'account-b', Revision: 2 };
    for (const bad of [{ ...target, AccountRef: 'account-a' }, { ...target, Provider: 'provider-a' }, { ...target, Revision: 4 }]) {
      assert.equal((await worker.configure(bad)).Outcome, 'rejected');
      assert.equal(worker.snapshot().provider, 'provider-a');
    }
    const bindings = config.accountBindings;
    for (const missing of [[bindings[0]], [bindings[1]]]) {
      config.accountBindings = missing;
      assert.equal((await worker.configure(target)).Outcome, 'rejected');
      assert.equal(worker.snapshot().provider, 'provider-a');
    }
    config.accountBindings = bindings;
    const applied = await worker.configure(target);
    assert.equal(applied.Outcome, 'applied'); assert.deepEqual(applied.Effective, target);
    assert.equal((await worker.configure(target)).Outcome, 'applied');
    assert.equal(worker.snapshot().provider, 'provider-b');
    assert.equal(worker.snapshot().nativeId, config.nativeId); assert.equal(worker.snapshot().historyFile, file);
    assert.equal(users(), originalUsers); assert.deepEqual(worker.storageSettings.getGlobalSettings(), defaults);
    b.setResponses([ai.fauxAssistantMessage('from b')]);
    assert.equal((await worker.send('next explicit input')).outcome, 'Accepted'); await worker.run;
    assert.equal(users(), originalUsers + 1);
    await worker.dispose(); worker = null;
    worker = await createSession(sdk, { ...config, historyFile: file, settings: target }, { modelRuntime: runtime, sessionOptions: { tools: [] } });
    assert.equal(worker.snapshot().provider, 'provider-b'); assert.equal(worker.snapshot().nativeId, config.nativeId);
    assert.deepEqual(worker.storageSettings.getGlobalSettings(), defaults);
  } finally {
    if (worker) { await worker.abort(); await worker.run; if (!worker.held) await worker.dispose(); }
    await runtime.settleCredentialWrites(); rmSync(root, { recursive: true });
  }
});

test('partial public settings mutation stays Unknown and holds the worker', async () => {
  let mutations = 0;
  const session = { subscribe: () => {}, isStreaming: false,
    setModel: async () => { mutations++; }, setThinkingLevel: () => { throw new Error('finite loss after model mutation'); },
    abort: async () => {} };
  const worker = new WorkerSession({}, session, {});
  worker.settings = { Provider: 'a', AccountRef: 'account-a', Model: 'm', Thinking: 'off', Revision: 1 };
  worker.runtime = { getModel: () => ({ provider: 'b', id: 'm' }) };
  worker.checkAccount = () => {}; worker.storageSettings = { flush: async () => {} };
  const result = await worker.configure({ ...worker.settings, Provider: 'b', AccountRef: 'account-b', Revision: 2 });
  assert.equal(result.Outcome, 'unknown'); assert.equal(worker.held, true); assert.equal(mutations, 1);
  assert.equal((await worker.configure({ ...worker.settings, Revision: 2 })).Outcome, 'rejected');
  assert.equal(mutations, 1);
});

test('native sync failure after real input holds Unknown without a second native input', async () => {
  const root = mkdtempSync(join(tmpdir(), 'pi-p3-sync-failure-'));
  for (const dir of ['agent', 'history', 'repo', 'auth']) mkdirSync(join(root, dir));
  const runtime = await sdk.ModelRuntime.create({ authPath: join(root, 'auth/auth.json'), modelsPath: null });
  const fake = ai.fauxProvider({ provider: 'p3-fake', models: [{ id: 'p3-model' }] });
  runtime.registerNativeProvider(fake.provider); await runtime.refresh({ allowNetwork: false });
  fake.setResponses([async (_context, options) => { await delay(10000, undefined, { signal: options.signal }); return ai.fauxAssistantMessage('must abort'); }]);
  const config = { nativeId: '33333333-3333-4333-8333-333333333333', emptyProven: true,
    cwd: join(root, 'repo'), agentDir: join(root, 'agent'), historyDir: join(root, 'history'), authDirectory: join(root, 'auth'),
    accountBindings: [{ provider: 'p3-fake', account_ref: 'fake-account' }],
    settings: { Provider: 'p3-fake', AccountRef: 'fake-account', Model: 'p3-model', Thinking: 'off', PolicyRevision: 'sync-failure', Revision: 1 } };
  let worker; let attempts = 0;
  try {
    worker = await createSession(sdk, config, { modelRuntime: runtime, sessionOptions: { tools: [] }, syncHistory: () => { attempts++; throw new Error('owned directory sync failure'); } });
    assert.equal((await worker.send('one actual input')).outcome, 'Unknown');
    await worker.run;
    assert.equal(worker.held, true); assert.equal(worker.snapshot().busy, false);
    assert.equal((await worker.send('must never replay')).outcome, 'Unknown');
    assert.equal(attempts, 1);
    const disk = sdk.SessionManager.open(worker.snapshot().historyFile);
    assert.equal(disk.getEntries().filter((entry) => entry.type === 'message' && entry.message.role === 'user').length, 1);
  } finally {
    if (worker) { await worker.abort(); await worker.run; worker.session.dispose(); }
    await runtime.settleCredentialWrites(); rmSync(root, { recursive: true });
  }
});

// One finite real transport check. models.json is prepared before construction;
// no SDK/runtime/provider/session injection or production endpoint override.
test('stored builtin Responses transport preserves literal role and rejects unfinished completion', {timeout: 30000}, async () => {
  const root=mkdtempSync(join(tmpdir(),'pi-public-responses-'));
  for(const dir of ['agent','history','repo','auth','model'])mkdirSync(join(root,dir));
  const child=spawn(process.env.PI_COMPONENT_PYTHON || 'python3', [fileURLToPath(new URL('../../test/creation_model.py',import.meta.url))],
    {env:{...process.env,CREATION_MODEL_ROOT:join(root,'model'),CREATION_MODEL_PORT:'0'},stdio:['ignore','pipe','pipe']});
  const joined=once(child,'exit');
  let diagnostic='';child.stderr.on('data',data=>{diagnostic=(diagnostic+data).slice(-4096);});
  const lines=createInterface({input:child.stdout});
  let worker;
  try {
    const [line]=await once(lines,'line');
    const {port,tls}=JSON.parse(line);assert.equal(tls,false);assert(Number.isSafeInteger(port)&&port>0);
    writeFileSync(join(root,'agent/models.json'),JSON.stringify({providers:{openai:{baseUrl:`http://127.0.0.1:${port}/primary/v1`,api:'openai-responses'}}}),{mode:0o600});
    writeFileSync(join(root,'auth/auth.json'),JSON.stringify({openai:{type:'api_key',key:'synthetic-component-only-key'}}),{mode:0o600});
    const policy={Mode:'default',Revision:1,Active:[],RegisteredCeiling:[]};policy.Digest=toolPolicyDigest(policy);
    const literal='PUBLIC_PI_LITERAL_ROLE\n/path-looking-role.md\nKeep the exact café goal separate.\n';
    const goal='PUBLIC_PI_ORIGINAL_GOAL_'+randomUUID();
    const config={nativeId:randomUUID(),emptyProven:true,generation:createHash('sha256').update(randomUUID()).digest('hex'),
      callbackSubject:{Kind:'conversation'},toolPolicy:policy,instructions:literal,cwd:join(root,'repo'),agentDir:join(root,'agent'),
      historyDir:join(root,'history'),authDirectory:join(root,'auth'),accountBindings:[{provider:'openai',account_ref:'fixture-account'}],
      settings:{Provider:'openai',AccountRef:'fixture-account',Model:'gpt-4o-mini',Thinking:'off',PolicyRevision:'component-default',Revision:1}};
    worker=await createSession(sdk,config);
    assert.equal(worker.runtime.getProviderAuthStatus('openai').source,'stored');
    assert.deepEqual(worker.runtime.getRegisteredProviderIds(),[]);
    assert.equal(worker.session.model.api,'openai-responses');assert.equal(worker.session.model.reasoning,false);
    assert.equal(worker.snapshot().instructionsDigest,createHash('sha256').update(literal,'utf8').digest('hex'));
    const accepted=await worker.send(goal,randomUUID());assert.equal(accepted.outcome,'Accepted');assert.equal(accepted.witness.effectiveMessage,goal);
    await worker.run;
    const disk=sdk.SessionManager.open(worker.snapshot().historyFile);
    const user=disk.getEntry(accepted.witness.entryId);assert.equal(user.message.role,'user');assert.deepEqual(user.message.content,[{type:'text',text:goal}]);
    const assistant=disk.getEntries().filter(entry=>entry.type==='message'&&entry.message.role==='assistant').at(-1).message;
    assert.equal(assistant.stopReason,'stop');assert(assistant.content.some(block=>block.type==='text'&&block.text==='CREATION_REPLY_primary'));
    const evidence=readFileSync(join(root,'model/requests.jsonl'),'utf8').trim().split('\n');assert.equal(evidence.length,1);
    const {gunzipSync}=await import('node:zlib');const sent=JSON.parse(gunzipSync(Buffer.from(JSON.parse(evidence[0]).body,'base64')));
    assert.equal(sent.model,'gpt-4o-mini');const system=[sent.instructions,...(sent.input||[]).filter(item=>['system','developer'].includes(item.role)).flatMap(item=>typeof item.content==='string'?[item.content]:(item.content||[]).map(block=>block.text))].filter(value=>typeof value==='string').join('\n');assert(system.includes(literal));
    assert.equal(JSON.stringify(sent).includes('synthetic-component-only-key'),false);
    writeFileSync(join(root,'model/unfinished-response'),'');
    // Input acceptance proves persisted user input, independently of model
    // completion. An unfinished SSE response must remain an assistant error.
    const unfinished=await worker.send('PUBLIC_PI_UNFINISHED_'+randomUUID(),randomUUID());assert.equal(unfinished.outcome,'Accepted');
    await worker.run;
    const failed=worker.snapshot().entries.filter(entry=>entry.type==='message'&&entry.message.role==='assistant').at(-1).message;
    assert.equal(failed.stopReason,'error');assert(failed.errorMessage);
    assert.equal(worker.snapshot().entries.filter(entry=>entry.type==='message'&&entry.message.role==='user').length,2);
  } finally {
    lines.close();
    if(worker){await worker.abort();await worker.run;if(!worker.held)await worker.dispose();}
    child.kill('SIGTERM');await joined;
    rmSync(root,{recursive:true});
  }
  assert.equal(diagnostic,'');
});
