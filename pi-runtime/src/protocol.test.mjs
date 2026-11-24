import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import {WorkerSession} from './session.mjs';
import {modelRuntimeKey, trustedSessionBinding} from './protocol.mjs';
import {mkdtempSync, mkdirSync, writeFileSync, rmSync} from 'node:fs';
import {join} from 'node:path';
import {tmpdir} from 'node:os';

test('production init binds exact retained identity before SDK or injected options', async () => {
  const conversation = {Session: {Workspace: '/fixture/workspace', Slug: '2026-11-19-owned', Incarnation: 'a'.repeat(64)}, Backend: 'pi', Member: 'lead', Incarnation: 'b'.repeat(64), NativeID: 'native-lead'};
  const identity = {Conversation: conversation, Generation: 'c'.repeat(64)};
  const config = {nativeId: conversation.NativeID, generation: identity.Generation, callbackSubject: {Kind: 'conversation', Conversation: identity, Utility: null}};
  assert.match(trustedSessionBinding(identity, config), /Member: lead/);
  for (const member of ['implementer0', 'reviewer0']) {
    const next = structuredClone(identity); next.Conversation.Member = member;
    const binding = trustedSessionBinding(next, {...config, callbackSubject: {Kind: 'conversation', Conversation: next}});
    assert(binding.includes('Member: ' + member));
    assert(!binding.includes('Member: lead'));
  }
  const utility = {OwnerKind: 'utility', Utility: {Session: conversation.Session, OperationID: 'operation', Incarnation: 'd'.repeat(64), NativeID: 'native-utility'}, Generation: identity.Generation};
  const subject = {Kind: 'utility', Conversation: null, Utility: {Session: conversation.Session, MonitorOperationID: 'operation', Incarnation: 'd'.repeat(64), NativeID: 'native-utility', Generation: identity.Generation}};
  assert.equal(trustedSessionBinding(utility, {nativeId: 'native-utility', generation: identity.Generation, callbackSubject: subject}), '');
  for (const changed of [undefined, {Kind: 'conversation'}, {...config.callbackSubject, Utility: {}},
    {Kind: 'conversation', Conversation: {...identity, Generation: 'd'.repeat(64)}},
    {Kind: 'conversation', Conversation: {...identity, Conversation: {...conversation, Member: 'implementer0'}}}]) {
    assert.throws(() => trustedSessionBinding(identity, {...config, callbackSubject: changed}), /scope mismatch/);
  }
  const child = spawn(process.execPath, [fileURLToPath(new URL('./worker.mjs', import.meta.url))], {env: {PI_OFFLINE: '1'}, stdio: ['pipe', 'pipe', 'pipe']});
  let output = '', stderr = '';
  child.stdout.on('data', value => {output += value;}); child.stderr.on('data', value => {stderr += value;});
  const closed = once(child, 'close');
  // Deliberately no SDK path: scope refusal must precede SDK construction.
  child.stdin.end(JSON.stringify({schema: 1, id: 'invalid-subject', identity, op: 'init', body: {...config, callbackSubject: {Kind: 'conversation'}}}) + '\n');
  const [code] = await closed;
  assert.equal(code, 0, stderr);
  assert.equal(JSON.parse(output.trim()).error, 'worker operation unresolved');
});

test('lost role application proof holds before input or settings effects', async () => {
  let prompts = 0, mutations = 0;
  const session = {subscribe() {}, isStreaming: false, prompt() {prompts++;}, setModel() {mutations++;}};
  const worker = new WorkerSession({}, session, {getBranch: () => []}, {verifyInstructions: () => {throw Error('lost actual observation');}});
  worker.settings = {Revision: 1};
  assert.deepEqual(await worker.send('original goal', 'a'.repeat(32)), {outcome: 'Unknown'});
  assert.equal(worker.pending, null); assert.equal(worker.activeInput, undefined);
  assert.equal((await worker.configure({Revision: 2})).Outcome, 'unknown');
  assert.throws(() => worker.snapshot(), /role instructions unavailable/);
  assert.throws(() => worker.identity(), /role instructions unavailable/);
  assert.equal(worker.held, true); assert.equal(prompts, 0); assert.equal(mutations, 0);
});

test('live identity uses required scalars without reconstructing or hashing native history', () => {
  const root = mkdtempSync(join(tmpdir(), 'pi-scalar-identity-'));
  try {
    const file = join(root, 'history.jsonl');
    writeFileSync(file, 'unparsed native history');
    let reads = 0;
    const manager = {getBranch: () => [], getSessionId: () => {reads++; return 'actual-native';}, getSessionFile: () => file};
    const session = {subscribe() {}, isStreaming: false, model: {id: 'model', provider: 'provider'}, thinkingLevel: 'off'};
    const worker = new WorkerSession({}, session, manager, {verifyInstructions: () => 'a'.repeat(64)});
    manager.getBranch = () => {throw Error('transcript reconstruction forbidden');};
    manager.getEntries = () => {throw Error('history enumeration forbidden');};
    const expected = {version: 1, nativeId: 'actual-native', historyFile: file, persisted: true,
      busy: false, held: false, model: 'model', provider: 'provider', thinking: 'off', instructionsDigest: 'a'.repeat(64), inputDelivery: 1};
    assert.deepEqual(worker.identity(), expected);
    session.model = {id: 'changed-live-model', provider: 'provider'};
    session.isStreaming = true;
    const changed = worker.identity();
    assert.equal(changed.model, 'changed-live-model'); assert.equal(changed.busy, true);
    assert.equal(reads, 2);
    assert.throws(() => worker.snapshot(), /transcript reconstruction forbidden/);
  } finally {rmSync(root, {recursive: true});}
});

test('production worker version smoke uses no SDK/session/account configuration', async () => {
  const child = spawn(process.execPath, [fileURLToPath(new URL('./worker.mjs', import.meta.url))], {
    env: { PI_OFFLINE: '1' }, stdio: ['pipe', 'pipe', 'pipe'],
  });
  let stdout = ''; let stderr = '';
  child.stdout.on('data', (chunk) => { stdout += chunk; });
  child.stderr.on('data', (chunk) => { stderr += chunk; });
  const closed = once(child, 'close');
  child.stdin.end(`${JSON.stringify({ schema: 1, id: 'smoke', identity: {}, op: 'version', body: {} })}\n`);
  const [code] = await closed;
  assert.equal(code, 0, stderr);
  const result = JSON.parse(stdout.trim());
  assert.equal(result.schema, 1); assert.equal(result.id, 'smoke');
  assert.deepEqual(result.body, { workerProtocol: 2, hostCallbacks: 1, linearLineage: 1 });
});

test('shared model runtime keys separate credential stores and frozen model/account inputs', () => {
 const root=mkdtempSync(join(tmpdir(),'pi-runtime-key-'));
 try {
  for (const name of ['auth-a','auth-b']) mkdirSync(join(root,name));
  const models=join(root,'models.json');writeFileSync(models,'{"providers":{}}');
  const a={piPackage:'/immutable/pi',authDirectory:join(root,'auth-a'),modelsPath:models,accountBindings:[{provider:'fake',account_ref:'a'}]};
  assert.equal(modelRuntimeKey(a),modelRuntimeKey({...a,agentDir:'/different/member',settings:{Model:'other'}}));
  assert.notEqual(modelRuntimeKey(a),modelRuntimeKey({...a,authDirectory:join(root,'auth-b')}));
  assert.notEqual(modelRuntimeKey(a),modelRuntimeKey({...a,accountBindings:[{provider:'fake',account_ref:'b'}]}));
  const before=modelRuntimeKey(a);writeFileSync(models,'{"providers":{"fake":{}}}');assert.notEqual(before,modelRuntimeKey(a));
 } finally {rmSync(root,{recursive:true});}
});
