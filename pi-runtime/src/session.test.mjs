import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync, readFileSync, readdirSync, existsSync, chmodSync } from 'node:fs';
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
import {trustedSessionBinding} from './protocol.mjs';
import {toolPolicyDigest} from './tool-policy.mjs';
import { WorkerActivity } from './activity.mjs';
import { setImmediate as immediate } from 'node:timers/promises';

test('worker activity observes public settled lifecycle, steer, persistence and reload gaps', {timeout: 10000}, async () => {
  const root = mkdtempSync(join(tmpdir(), 'pi-activity-'));
  for (const dir of ['repo', 'agent', 'auth', 'history']) mkdirSync(join(root, dir));
  const runtime = await sdk.ModelRuntime.create({authPath: join(root, 'auth/auth.json'), modelsPath: null});
  const fake = ai.fauxProvider({provider: 'activity-fake', models: [{id: 'fixture'}]});
  runtime.registerNativeProvider(fake.provider); await runtime.refresh({allowNetwork: false});
  let epoch = 1700000000000, monotonic = 0;
  const activityClock = {now: () => epoch, monotonic: () => monotonic};
  const advance = ms => { epoch += ms; monotonic += ms; };
  const config = {nativeId: randomUUID(), emptyProven: true, cwd: join(root, 'repo'), agentDir: join(root, 'agent'),
    historyDir: join(root, 'history'), authDirectory: join(root, 'auth'),
    accountBindings: [{provider: 'activity-fake', account_ref: 'fixture'}],
    settings: {Provider: 'activity-fake', AccountRef: 'fixture', Model: 'fixture', Thinking: 'off', PolicyRevision: 'fixture', Revision: 1}};
  let worker;
  try {
    writeFileSync(join(root, 'repo/fixture.txt'), 'activity fixture');
    worker = await createSession(sdk, config, {modelRuntime: runtime, sessionOptions: {tools: ['read']}, activityClock});
    advance(5);
    const entered = Promise.withResolvers(), release = Promise.withResolvers();
    fake.setResponses([{...ai.fauxAssistantMessage(''), stopReason: 'toolUse', content: [{type: 'toolCall', id: 'activity-read', name: 'read', arguments: {path: join(root, 'repo/fixture.txt')}}]},
      async () => { entered.resolve(); await release.promise; return ai.fauxAssistantMessage('first'); }, ai.fauxAssistantMessage('steered')]);
    let persistedFinal = false;
    worker.listeners.add(event => {
      if (event.type === 'workflow_changed' && worker.manager.getBranch().some(entry => Array.isArray(entry.message?.content) && entry.message.content.some(block => block.text === 'steered'))) persistedFinal = true;
    });
    assert.equal((await worker.send('initial', randomUUID())).outcome, 'Accepted');
    await entered.promise; advance(10);
    const started = worker.readActivity().startedAtMs;
    assert.equal(worker.readActivity().currentState, 'working');
    assert.equal(worker.readActivity().workingMs, 10);
    const inputOperationID = randomUUID();
    assert.equal(worker.enqueueInput({message: 'steer', inputOperationID, delivery: 'steer'}).outcome, 'Pending');
    advance(20);
    assert.equal(worker.readActivity().startedAtMs, started);
    assert.equal(worker.readActivity().currentState, 'working', 'Pending native queue is not a user wait');
    release.resolve();
    assert.equal((await worker.waitInput({inputOperationID})).outcome, 'Accepted');
    worker.commitInput({inputOperationID});
    await worker.run; await immediate();
    let activity = worker.readActivity();
    assert.equal(activity.currentState, 'idle'); assert.equal(activity.workingMs, 30);
    assert.equal(activity.sentMessages, 2); assert.equal(activity.receivedMessages, 3); assert.equal(activity.totalToolCalls, 1);
    assert.equal(persistedFinal, true, 'post-append invalidation sees final native entry');
    advance(7); activity = worker.readActivity();
    assert.equal(activity.idleMs, 12); assert.equal(activity.coverageComplete, false);
    const historyFile = worker.snapshot().historyFile;
    worker.holdInputs(); advance(11);
    assert.equal(worker.readActivity().currentState, 'unclassified');
    assert.equal(worker.readActivity().unclassifiedMs, 11);
    await worker.abort(); worker.session.dispose(); worker = null;
    advance(1000);
    worker = await createSession(sdk, {...config, historyFile}, {modelRuntime: runtime, sessionOptions: {tools: []}, activityClock});
    activity = worker.readActivity();
    assert.equal(activity.workingMs, 0); assert.equal(activity.sentMessages, 2);
    assert.equal(activity.coverageStartMs, epoch); assert.equal(activity.coverageComplete, false);
    await worker.dispose(); worker = null;
  } finally {
    if (worker) { await worker.abort(); await worker.run; worker.session.dispose(); }
    await runtime.settleCredentialWrites(); rmSync(root, {recursive: true});
  }
});

test('held worker activity waits for actual settled abort and keeps held time unclassified', {timeout: 10000}, async () => {
  const root = mkdtempSync(join(tmpdir(), 'pi-held-activity-'));
  for (const dir of ['repo', 'agent', 'auth', 'history']) mkdirSync(join(root, dir));
  const runtime = await sdk.ModelRuntime.create({authPath: join(root, 'auth/auth.json'), modelsPath: null});
  const fake = ai.fauxProvider({provider: 'held-activity-fake', models: [{id: 'fixture'}]});
  runtime.registerNativeProvider(fake.provider); await runtime.refresh({allowNetwork: false});
  let epoch = 1700000000000, monotonic = 0;
  const advance = ms => { epoch += ms; monotonic += ms; };
  const config = {nativeId: randomUUID(), emptyProven: true, cwd: join(root, 'repo'), agentDir: join(root, 'agent'),
    historyDir: join(root, 'history'), authDirectory: join(root, 'auth'),
    accountBindings: [{provider: 'held-activity-fake', account_ref: 'fixture'}],
    settings: {Provider: 'held-activity-fake', AccountRef: 'fixture', Model: 'fixture', Thinking: 'off', PolicyRevision: 'fixture', Revision: 1}};
  let worker;
  try {
    worker = await createSession(sdk, config, {modelRuntime: runtime, sessionOptions: {tools: []},
      activityClock: {now: () => epoch, monotonic: () => monotonic}});
    const entered = Promise.withResolvers(), settled = Promise.withResolvers();
    fake.setResponses([async (_context, options) => {
      const aborted = new Promise(resolve => options.signal.addEventListener('abort', resolve, {once: true}));
      entered.resolve(); await aborted;
      return ai.fauxAssistantMessage('must not complete');
    }]);
    worker.listeners.add(event => { if (event.type === 'agent_settled') settled.resolve(event); });
    advance(5); assert.equal((await worker.send('one held run', 'held-run')).outcome, 'Accepted');
    await entered.promise; advance(10);
    assert.deepEqual(worker.holdInputs(), {held: true});
    advance(7);
    const running = worker.readActivity();
    assert.equal(running.latestTurnStatus, 'inProgress', 'a hold alone does not establish settlement');
    assert.equal(running.currentState, 'unclassified'); assert.equal(running.unclassifiedMs, 7);
    assert.equal((await settled.promise).aborted, true); await worker.run; await immediate();
    const activity = worker.readActivity();
    const disk = sdk.SessionManager.open(worker.manager.getSessionFile());
    assert(disk.getEntries().some(entry => entry.type === 'message' && entry.message.role === 'assistant' && entry.message.stopReason === 'aborted'));
    assert.equal(activity.latestTurnStatus, 'interrupted'); assert.equal(activity.currentState, 'unclassified');
    assert.equal(activity.stateSinceMs, running.stateSinceMs, 'settlement creates no synthetic idle interval');
    assert.equal(activity.completedAtMs, epoch); assert.equal(activity.workingMs, 10);
    assert.equal(activity.idleMs, 5); assert.equal(activity.unclassifiedMs, 7); assert.equal(activity.coverageComplete, false);
    assert.equal(worker.held, true);
    assert.deepEqual(worker.enqueueInput({message: 'never replay', inputOperationID: 'new', delivery: 'steer'}), {outcome: 'Unknown'});
    assert.equal(disk.getEntries().filter(entry => entry.type === 'message' && entry.message.role === 'user').length, 1);
  } finally {
    if (worker) { await worker.abort(); await worker.run; worker.session.dispose(); }
    await runtime.settleCredentialWrites(); rmSync(root, {recursive: true});
  }
});

test('worker activity keeps retry and malformed historical timing honest', () => {
  let now = 1700000000000, tick = 0;
  const activity = new WorkerActivity({getSessionId: () => 'fixture', getBranch: () => [
    {id: 'u', type: 'message', message: {role: 'user', timestamp: 10}},
    {id: 'a', type: 'message', timestamp: 'bad', message: {role: 'assistant', timestamp: 20, stopReason: 'stop', content: []}},
  ]}, false, {now: () => now, monotonic: () => tick});
  assert.equal(activity.read(false).startedAtMs, undefined);
  assert.equal(activity.read(false).completedAtMs, undefined, 'assistant message time is not persistence time');
  activity.event({type: 'agent_start'}); tick += 10; now += 10;
  activity.event({type: 'agent_end'}); tick += 5; now += 5;
  assert.equal(activity.read(false).currentState, 'working', 'agent_end is not settled');
  activity.event({type: 'auto_retry_start', delayMs: 50}); tick += 20; now += 20;
  assert.equal(activity.read(false).currentState, 'waiting');
  const started = activity.read(false).startedAtMs;
  activity.event({type: 'agent_start'}); tick += 5; now -= 100;
  assert.equal(activity.read(false).startedAtMs, started, 'retry does not reset the active interval');
  activity.event({type: 'agent_settled'});
  const result = activity.read(false);
  assert.equal(result.workingMs, 20); assert.equal(result.waitingMs, 20);
  assert.equal(result.currentState, 'idle'); assert.equal(result.coverageComplete, false);
});

test('image display projection bounds pages and live messages without changing native history or witnesses', () => {
  const root = mkdtempSync(join(tmpdir(), 'pi-image-display-'));
  for (const directory of ['repo', 'history']) mkdirSync(join(root, directory));
  try {
    const manager = sdk.SessionManager.create(join(root, 'repo'), join(root, 'history'), {id: randomUUID()});
    manager.appendMessage(ai.fauxAssistantMessage('earlier text '.padEnd(300 * 1024, 'a')));
    const previousLeaf = manager.getLeafId();
    const message = 'read the fixture image';
    const userId = manager.appendMessage({role: 'user', content: [{type: 'text', text: message}], timestamp: 1});
    manager.appendMessage({...ai.fauxAssistantMessage(''), stopReason: 'toolUse',
      content: [{type: 'toolCall', id: 'read-image', name: 'read', arguments: {path: '/fixture.png'}}]});
    const image = {type: 'image', data: 'A'.repeat(700 * 1024), mimeType: 'image/png'};
    const resultId = manager.appendMessage({role: 'toolResult', toolCallId: 'read-image', toolName: 'read', isError: false,
      content: [{type: 'text', text: 'Read image file [image/png]'}, image], timestamp: 2});
    manager.appendMessage(ai.fauxAssistantMessage('latest text '.padEnd(300 * 1024, 'b')));
    const branch = manager.getBranch();
    const raw = JSON.stringify(branch), bytes = readFileSync(manager.getSessionFile());
    const digest = createHash('sha256').update(raw).digest('hex');
    const result = manager.getEntry(resultId);
    assert(Buffer.byteLength(JSON.stringify(result)) > 512 * 1024);
    // Public SessionManager reads expose native object references. A display
    // projection must also work when those references cannot be changed.
    for (const entry of branch) {
      for (const block of entry.message.content) Object.freeze(block);
      Object.freeze(entry.message.content); Object.freeze(entry.message); Object.freeze(entry);
    }
    let notify;
    const session = {subscribe(listener) {notify = listener;}, isStreaming: true,
      model: {id: 'fixture', provider: 'fixture'}, thinkingLevel: 'off'};
    const worker = new WorkerSession(sdk, session, manager);
    const live = Object.freeze({role: 'assistant', content: Object.freeze([
      Object.freeze({type: 'text', text: 'live text'}), Object.freeze({...image}),
    ])});
    notify({type: 'message_update', message: live});
    const snapshot = worker.snapshot();
    assert.equal(snapshot.historyStart, 1, 'projected image fits beside the latest text; older text stays paged');
    assert.equal(snapshot.historyTotal, branch.length); assert.equal(snapshot.historyDigest, digest);
    const projected = snapshot.entries.find(entry => entry.id === resultId);
    assert.notEqual(projected, result); assert.notEqual(projected.message, result.message);
    assert.equal(projected.message.content[0], result.message.content[0]);
    assert.deepEqual(projected.message.content[1], {type: 'text', text: '[Image omitted from transcript display]'});
    assert.equal(projected.message.toolCallId, 'read-image'); assert.equal(projected.message.isError, false);
    assert.deepEqual(snapshot.liveMessage.content, [live.content[0], projected.message.content[1]]);
    assert.notEqual(snapshot.liveMessage, live); assert.equal(live.content[1].data, image.data);
    assert(Buffer.byteLength(JSON.stringify(snapshot.entries)) < 512 * 1024);
    assert(Buffer.byteLength(JSON.stringify({schema: 1, id: 'snapshot', identity: {}, body: snapshot})) < 1024 * 1024);
    const imagePage = worker.snapshot({before: branch.findIndex(entry => entry.id === resultId) + 1});
    assert.equal(imagePage.historyDigest, digest);
    assert.deepEqual(imagePage.entries.find(entry => entry.id === resultId), projected);
    const older = worker.snapshot({before: snapshot.historyStart});
    assert.equal(older.historyStart, 0); assert.deepEqual(older.entries, branch.slice(0, 1));
    const user = manager.getEntry(userId);
    assert.deepEqual(worker.verifyHistory({Message: message, Witness: {SessionID: manager.getSessionId(),
      EntryID: userId, ParentID: user.parentId, PreviousLeafID: previousLeaf}}), {Verified: true});
    assert.deepEqual(worker.verifyHistory({Message: 'different input', Witness: {SessionID: manager.getSessionId(),
      EntryID: userId, ParentID: user.parentId, PreviousLeafID: previousLeaf}}), {Verified: false});
    assert.equal(JSON.stringify(manager.getBranch()), raw);
    assert.deepEqual(readFileSync(manager.getSessionFile()), bytes);
    assert.equal(sdk.SessionManager.open(manager.getSessionFile()).getEntry(resultId).message.content[1].data, image.data);
    notify({type: 'message_end', message: live}); assert.equal(worker.snapshot().liveMessage, null);
  } finally { rmSync(root, {recursive: true}); }
});

test('snapshot display preserves ordinary content and existing text and entry-count bounds', () => {
  let branch = [{id: 'text', parentId: null, type: 'message', message: {role: 'assistant', content: [
    {type: 'text', text: 'ordinary text'},
    {type: 'toolCall', id: 'call', name: 'fixture', arguments: {type: 'image', data: 'ordinary tool arguments'}},
  ]}}];
  const manager = {getBranch: () => branch, getSessionId: () => 'fixture',
    getSessionFile: () => '/nonexistent/pi-display-fixture.json', getLeafId: () => branch.at(-1).id};
  const worker = new WorkerSession({}, {subscribe() {}, isStreaming: false}, manager);
  worker.liveMessage = branch[0].message;
  let snapshot = worker.snapshot();
  assert.equal(snapshot.entries[0], branch[0]); assert.equal(snapshot.liveMessage, worker.liveMessage);
  assert.equal(snapshot.historyDigest, createHash('sha256').update(JSON.stringify(branch)).digest('hex'));
  branch = Array.from({length: 60}, (_, i) => ({...branch[0], id: String(i)}));
  snapshot = worker.snapshot(); assert.equal(snapshot.entries.length, 50); assert.equal(snapshot.historyStart, 10);
  assert.throws(() => worker.snapshot({before: 61}), /invalid native history position/);
  branch = [{...branch[0], message: {role: 'assistant', content: [{type: 'text', text: 'x'.repeat(512 * 1024)}]}}];
  assert.throws(() => worker.snapshot(), /native entry exceeds display bound/);
});

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
    const identity = {Conversation: {Session: {Workspace: root, Slug: '2026-11-19-owned', Incarnation: 'a'.repeat(64)}, Backend: 'pi', Member: 'lead', Incarnation: 'c'.repeat(64), NativeID: config.nativeId}, Generation: config.generation};
    config.callbackSubject = {Kind: 'conversation', Conversation: identity};
    const binding = trustedSessionBinding(identity, config);
    for (const literal of [pathRole, 'literal é\nrole\r\n', 'é'.repeat(2047)]) {
      worker = await createSession(sdk, {...config, instructions: literal}, {modelRuntime: runtime, sessionOptions: {tools: []}}, binding);
      assert.deepEqual(worker.session.resourceLoader.getAppendSystemPrompt(), [base, binding, literal]);
      assert.equal(worker.snapshot().instructionsDigest, createHash('sha256').update(literal, 'utf8').digest('hex'));
      assert.match(worker.session.systemPrompt, /You are a coding assistant/);
      assert.match(worker.session.systemPrompt, /workspace AGENTS marker/);
      assert.match(worker.session.systemPrompt, /existing append marker/);
      assert(worker.session.systemPrompt.includes(binding));
      assert.equal(worker.session.systemPrompt.includes('path contents must never become the role'), false);
      assert.equal(worker.session.systemPrompt.split(literal).length - 1, 1);
      await worker.dispose(); worker = null;
    }
    await assert.rejects(createSession(sdk, {...config, instructions: pathRole}, {modelRuntime: runtime,
      resourceOptions: {systemPromptOverride: () => 'replacement'}}), /construction unavailable/);
    await assert.rejects(createSession(sdk, {...config, instructions: pathRole}, {modelRuntime: runtime,
      sessionOptions: {resourceLoader: {}}}), /construction unavailable/);
    writeFileSync(join(root, 'repo/.pi/SYSTEM.md'), 'genuine custom prompt marker');
    worker = await createSession(sdk, {...config, instructions: pathRole}, {modelRuntime: runtime, sessionOptions: {tools: []}}, binding);
    assert.equal(worker.session.systemPrompt.startsWith('genuine custom prompt marker'), true);
    assert.match(worker.session.systemPrompt, /workspace AGENTS marker/);
    assert.deepEqual(worker.session.resourceLoader.getAppendSystemPrompt(), [base, binding, pathRole]);
    worker.session.resourceLoader.getAppendSystemPrompt()[1] = 'changed after init';
    assert.throws(() => worker.snapshot(), /role instructions unavailable/);
    assert.deepEqual(await worker.send('must not submit', 'c'.repeat(32)), {outcome: 'Unknown'});
    assert.equal(worker.pending, null); assert.equal(worker.activeInput, undefined);
    worker.session.dispose(); worker = null;
    // Retained members without a saved role still get checked construction,
    // while their historical omitted role digest remains omitted.
    worker = await createSession(sdk, config, {modelRuntime: runtime, sessionOptions: {tools: []}}, binding);
    assert.equal(worker.snapshot().instructionsDigest, undefined);
    assert.deepEqual(worker.session.resourceLoader.getAppendSystemPrompt(), [base, binding]);
    worker.session.resourceLoader.getAppendSystemPrompt()[1] = 'changed ownership';
    assert.throws(() => worker.snapshot(), /role instructions unavailable/);
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
    const witnesses = []; let originalWitness;
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
      originalWitness ??= accepted.witness;
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
    const retained = {...config, historyFile, settings: {...worker.settings}};
    const historyBytes = readFileSync(historyFile), historyDigest = worker.snapshot().historyDigest;
    const leaf = worker.manager.getLeafId();
    await worker.dispose();
    const checkAuth = runtime.checkAuth.bind(runtime); let authChecks = 0;
    runtime.checkAuth = async (...args) => { authChecks++; return checkAuth(...args); };
    worker = await createSession(sdk, retained, { modelRuntime: runtime, sessionOptions: { tools: [] } });
    assert.equal(worker.snapshot().nativeId, config.nativeId);
    assert.equal(worker.session.model.id, retained.settings.Model);
    assert.equal(worker.session.thinkingLevel, retained.settings.Thinking);
    assert(authChecks > 0, 'retained load keeps the public native auth precondition');
    assert.deepEqual(readFileSync(historyFile), historyBytes);
    assert.equal(worker.snapshot().historyDigest, historyDigest); assert.equal(worker.manager.getLeafId(), leaf);
    assert.deepEqual(worker.verifyHistory({Message: 'identical input', Witness: {SessionID: originalWitness.sessionId,
      EntryID: originalWitness.entryId, ParentID: originalWitness.parentId, PreviousLeafID: originalWitness.previousLeafId}}), {Verified: true});
    await worker.dispose(); worker = null;
    runtime.checkAuth = async () => false;
    await assert.rejects(createSession(sdk, retained, {modelRuntime: runtime, sessionOptions: {tools: []}}), /retained native auth unavailable/);
    runtime.checkAuth = checkAuth;
    for (const field of ['model', 'thinkingLevel']) {
      const drift = {...sdk, createAgentSession: async options => {
        const result = await sdk.createAgentSession(options);
        Object.defineProperty(result.session, field, {get: () => field === 'model' ? {provider: 'foreign', id: 'foreign'} : 'xhigh'});
        return result;
      }};
      await assert.rejects(createSession(drift, retained, {modelRuntime: runtime, sessionOptions: {tools: []}}), /retained native settings diverged/);
      assert.deepEqual(readFileSync(historyFile), historyBytes);
    }
    worker = await createSession(sdk, retained, {modelRuntime: runtime, sessionOptions: {tools: []}});
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
  let mutations = 0, cleared = 0;
  const session = { subscribe: () => {}, isStreaming: false,
    setModel: async () => { mutations++; }, setThinkingLevel: () => { throw new Error('finite loss after model mutation'); },
    clearQueue: () => {cleared++;}, abort: async () => {} };
  const worker = new WorkerSession({}, session, {getBranch: () => []});
  worker.settings = { Provider: 'a', AccountRef: 'account-a', Model: 'm', Thinking: 'off', Revision: 1 };
  worker.runtime = { getModel: () => ({ provider: 'b', id: 'm' }) };
  worker.checkAccount = () => {}; worker.storageSettings = { flush: async () => {} };
  const result = await worker.configure({ ...worker.settings, Provider: 'b', AccountRef: 'account-b', Revision: 2 });
  assert.equal(cleared, 1);
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

test('installed builtin Responses uses immutable endpoint and private model cache', {timeout: 30000}, async () => {
  const root=mkdtempSync(join(tmpdir(),'pi-installed-responses-'));
  for(const dir of ['agent','history','repo','auth','model','installed'])mkdirSync(join(root,dir));
  const child=spawn(process.env.PI_COMPONENT_PYTHON || 'python3', [fileURLToPath(new URL('../../test/creation_model.py',import.meta.url))],
    {env:{...process.env,CREATION_MODEL_ROOT:join(root,'model'),CREATION_MODEL_PORT:'0'},stdio:['ignore','pipe','pipe']});
  const joined=once(child,'exit');
  let diagnostic='';child.stderr.on('data',data=>{diagnostic=(diagnostic+data).slice(-4096);});
  const lines=createInterface({input:child.stdout});
  let worker;
  try {
    const [line]=await once(lines,'line');
    const {port,tls}=JSON.parse(line);assert.equal(tls,false);assert(Number.isSafeInteger(port)&&port>0);
    const modelsPath=join(root,'installed/pi-models.json');
    writeFileSync(modelsPath,JSON.stringify({providers:{openai:{baseUrl:`http://127.0.0.1:${port}/primary/v1`,api:'openai-responses',modelOverrides:{'gpt-4o-mini':{samplingParams:{parallel_tool_calls:false}}}}}}),{mode:0o600});
    writeFileSync(join(root,'auth/auth.json'),JSON.stringify({openai:{type:'api_key',key:'synthetic-component-only-key'}}),{mode:0o600});
    const installedBefore=readFileSync(modelsPath);
    const authBefore=readFileSync(join(root,'auth/auth.json'));
    chmodSync(modelsPath,0o444);chmodSync(join(root,'installed'),0o555);
    const policy={Mode:'default',Revision:1,Active:[],RegisteredCeiling:[]};policy.Digest=toolPolicyDigest(policy);
    const literal='PUBLIC_PI_LITERAL_ROLE\n/path-looking-role.md\nKeep the exact café goal separate.\n';
    const goal='PUBLIC_PI_ORIGINAL_GOAL_'+randomUUID();
    const config={nativeId:randomUUID(),emptyProven:true,generation:createHash('sha256').update(randomUUID()).digest('hex'),
      callbackSubject:{Kind:'conversation'},toolPolicy:policy,instructions:literal,cwd:join(root,'repo'),agentDir:join(root,'agent'),
      historyDir:join(root,'history'),authDirectory:join(root,'auth'),accountBindings:[{provider:'openai',account_ref:'fixture-account'}],
      settings:{Provider:'openai',AccountRef:'fixture-account',Model:'gpt-4o-mini',Thinking:'off',PolicyRevision:'component-default',Revision:1}};
    worker=await createSession(sdk,{...config,modelsPath});
    // Must fail here on old production source, before any send/default route.
    assert.equal(worker.session.model.baseUrl,`http://127.0.0.1:${port}/primary/v1`);
    assert.equal(existsSync(join(root,'agent/models.json')),false);
    assert.equal(worker.runtime.modelsPath,modelsPath);
    assert.equal(worker.runtime.getProviderAuthStatus('openai').source,'stored');
    assert.deepEqual(worker.runtime.getRegisteredProviderIds(),[]);
    assert.equal(worker.session.model.api,'openai-responses');assert.equal(worker.session.model.reasoning,false);
    assert.equal(worker.snapshot().instructionsDigest,createHash('sha256').update(literal,'utf8').digest('hex'));
    assert.equal(worker.session.model.samplingParams.parallel_tool_calls,false);
    const originalPayload=worker.session.agent.onPayload;
    let payloads=0;
    worker.session.agent.onPayload=async function(payload,model){
      const transformed=await originalPayload.call(this,payload,model);
      const final=transformed===undefined?payload:transformed;
      assert.equal(final.parallel_tool_calls,false);payloads++;
      return transformed;
    };
    const accepted=await worker.send(goal,randomUUID());assert.equal(accepted.outcome,'Accepted');assert.equal(accepted.witness.effectiveMessage,goal);
    await worker.run;
    const disk=sdk.SessionManager.open(worker.snapshot().historyFile);
    const user=disk.getEntry(accepted.witness.entryId);assert.equal(user.message.role,'user');assert.deepEqual(user.message.content,[{type:'text',text:goal}]);
    const assistant=disk.getEntries().filter(entry=>entry.type==='message'&&entry.message.role==='assistant').at(-1).message;
    assert.equal(assistant.stopReason,'stop');assert(assistant.content.some(block=>block.type==='text'&&block.text==='CREATION_REPLY_primary'));
    const evidence=readFileSync(join(root,'model/requests.jsonl'),'utf8').trim().split('\n');assert.equal(evidence.length,1);
    const {gunzipSync}=await import('node:zlib');const sent=JSON.parse(gunzipSync(Buffer.from(JSON.parse(evidence[0]).body,'base64')));
    // The fixture projects model/input/role evidence; the actual serializer's
    // parallel_tool_calls value is checked by the original chained hook above.
    assert.equal(payloads,1);assert.equal(sent.model,'gpt-4o-mini');const system=[sent.instructions,...(sent.input||[]).filter(item=>['system','developer'].includes(item.role)).flatMap(item=>typeof item.content==='string'?[item.content]:(item.content||[]).map(block=>block.text))].filter(value=>typeof value==='string').join('\n');assert(system.includes(literal));
    assert.equal(JSON.stringify(sent).includes('synthetic-component-only-key'),false);
    assert.equal(disk.getSessionId(),accepted.witness.sessionId);
    assert.equal(user.id,accepted.witness.entryId);assert.equal(user.parentId,accepted.witness.parentId);
    assert.equal(disk.getEntries().filter(entry=>entry.type==='message'&&entry.message.role==='user').length,1);
    assert.deepEqual(readFileSync(modelsPath),installedBefore);
    assert.deepEqual(readFileSync(join(root,'auth/auth.json')),authBefore);
    assert.deepEqual(readdirSync(join(root,'installed')),['pi-models.json']);
    // The SDK retains this public store path even with model network disabled.
    assert.equal(worker.runtime.models.modelsStore.path,join(root,'agent/models-store.json'));
    await worker.dispose();worker=undefined;
    // Both native booleans remain admissible; constructing true sends nothing.
    const truePath=join(root,'true-models.json');
    const trueConfig=JSON.parse(installedBefore);
    trueConfig.providers.openai.modelOverrides['gpt-4o-mini'].samplingParams.parallel_tool_calls=true;
    writeFileSync(truePath,JSON.stringify(trueConfig));
    worker=await createSession(sdk,{...config,nativeId:randomUUID(),modelsPath:truePath});
    assert.equal(worker.session.model.samplingParams.parallel_tool_calls,true);
    await worker.dispose();worker=undefined;
    // Multiple exact builtins resolve without a second provider request.
    const multipleConfig=JSON.parse(installedBefore);
    multipleConfig.providers.openai.modelOverrides['gpt-6.1-sol']={samplingParams:{parallel_tool_calls:false}};
    const multiplePath=join(root,'multiple-models.json');writeFileSync(multiplePath,JSON.stringify(multipleConfig));
    worker=await createSession(sdk,{...config,nativeId:randomUUID(),modelsPath:multiplePath});
    assert.equal(worker.runtime.getModel('openai','gpt-6.1-sol').samplingParams.parallel_tool_calls,false);
    assert.equal(worker.session.model.samplingParams.parallel_tool_calls,false);
    await worker.dispose();worker=undefined;
    assert.equal(readFileSync(join(root,'model/requests.jsonl'),'utf8').trim().split('\n').length,1);
    // A supplied bad path/config refuses before any new history/settings write.
    const protectedFiles=['agent/settings.json','auth/auth.json'];
    const before=protectedFiles.map(file=>existsSync(join(root,file))?readFileSync(join(root,file)):null);
    const historyBefore=readdirSync(join(root,'history'));
    for(const badPath of [join(root,'installed/missing.json'),null,'',join(root,'installed')]) {
      await assert.rejects(createSession(sdk,{...config,nativeId:randomUUID(),modelsPath:badPath}),/installed native model configuration unavailable/);
    }
    const badFile=join(root,'bad-models.json');
    const validOverrides=JSON.parse(installedBefore);
    const badOverrides=[null,{}, {'gpt-*':{samplingParams:{parallel_tool_calls:true}}},
      {'gpt-4o-mini':{}}, {'gpt-4o-mini':{samplingParams:null}}, {'gpt-4o-mini':{samplingParams:{}}},
      {'gpt-4o-mini':{samplingParams:{parallel_tool_calls:1}}},
      {'gpt-4o-mini':{samplingParams:{parallel_tool_calls:'true'}}},
      {'gpt-4o-mini':{samplingParams:{parallel_tool_calls:null}}},
      {'gpt-4o-mini':{samplingParams:{parallel_tool_calls:true,temperature:1}}},
      {'gpt-4o-mini':{samplingParams:{parallel_tool_calls:true},headers:{}}},
      Object.fromEntries(Array.from({length:17},(_,i)=>['model-'+i,{samplingParams:{parallel_tool_calls:false}}]))];
    for(const override of badOverrides){
      const value=structuredClone(validOverrides);value.providers.openai.modelOverrides=override;
      writeFileSync(badFile,JSON.stringify(value));
      await assert.rejects(createSession(sdk,{...config,nativeId:randomUUID(),modelsPath:badFile}),/installed native model configuration unavailable/);
    }
    for(const value of ['null','{}','{"providers":{"openai":{"api":"openai-responses","baseUrl":"https://user:pass@gateway/v1"}}}',
      '{"providers":{"openai":{"baseUrl":"http://127.0.0.1/v1","api":"openai-responses","apiKey":"forbidden"}}}',
      '{"providers":{"openai":{"baseUrl":"http://127.0.0.1/v1","api":"openai-responses","api":"openai-responses"}}}']) {
      writeFileSync(badFile,value);
      await assert.rejects(createSession(sdk,{...config,nativeId:randomUUID(),modelsPath:badFile}),/installed native model configuration unavailable/);
    }
    assert.deepEqual(readdirSync(join(root,'history')),historyBefore);
    assert.deepEqual(protectedFiles.map(file=>existsSync(join(root,file))?readFileSync(join(root,file)):null),before);
  } finally {
    lines.close();
    if(worker){await worker.abort();await worker.run;if(!worker.held)await worker.dispose();}
    child.kill('SIGTERM');await joined;
    chmodSync(join(root,'installed'),0o755);
    rmSync(root,{recursive:true});
  }
  assert.equal(diagnostic,'');
});
