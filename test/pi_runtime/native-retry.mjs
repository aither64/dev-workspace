import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, appendFileSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';
import { sdk, ai, packageRoot, nodeBinary } from './package-api.mjs';
import { createNativeRetryRouting } from '../../pi-runtime/src/native-retry.mjs';
import { syncHistoryPrefix } from '../../pi-runtime/src/persistence.mjs';

const nativeId = '22222222-2222-4222-8222-222222222222';
const generation = 'c'.repeat(64); const policyDigest = 'd'.repeat(64);
const entryType = sdk.VIRTUAL_MODEL_STATE_ENTRY;
const self = fileURLToPath(import.meta.url);
function fixturePolicy(root) {
  const Conversation = { Session: { Workspace: root, Slug: '2026-11-12-sdk-native-fixture', Incarnation: 'a'.repeat(64) }, Backend: 'pi', Member: 'lead', Incarnation: 'b'.repeat(64), NativeID: nativeId };
  const Targets = [{ Provider: 'fixture-a', AccountRef: 'account-a', Model: 'a', Thinking: 'medium' }, { Provider: 'fixture-b', AccountRef: 'account-b', Model: 'b', Thinking: 'medium' }, { Provider: 'fixture-b', AccountRef: 'account-b', Model: 'c', Thinking: 'medium' }];
  return { Conversation, Settings: { ...Targets[0], PolicyRevision: policyDigest, Revision: 1 }, CatalogDigest: 'e'.repeat(64), PolicyDigest: policyDigest,
    Targets, MaxExtraAttempts: 1, MaxHops: 1, FallbackOn: ['overload', 'rate_limit'] };
}
function errorMessage(responseId, content = '') { return ai.fauxAssistantMessage(content, { stopReason: 'error', errorMessage: 'overloaded', responseId }); }
function providers() {
  return ['fixture-a', 'fixture-b'].map((provider) => {
    const fake = ai.fauxProvider({ provider, api: `${provider}-api`, models: (provider === 'fixture-a' ? ['a'] : ['b', 'c']).map((id) => ({ id, reasoning: true, contextWindow: 1000000 })), tokensPerSecond: 100000 });
    return { ...fake, provider: { ...fake.provider, auth: { apiKey: { name: 'Owned fake auth', resolve: async ({ credential }) => credential?.key ? { auth: { apiKey: credential.key }, source: 'owned fixture' } : undefined } } } };
  });
}
async function createOwned(root, existingFile) {
  for (const name of ['auth', 'agent', 'history']) mkdirSync(join(root, name), { recursive: true });
  writeFileSync(join(root, 'auth/auth.json'), JSON.stringify({ 'fixture-a': { type: 'api_key', key: 'fake-only' }, 'fixture-b': { type: 'api_key', key: 'fake-only' } }));
  const runtime = await sdk.ModelRuntime.create({ authPath: join(root, 'auth/auth.json'), modelsPath: join(root, 'agent/models.json'), allowModelNetwork: false });
  const [a, b] = providers(); runtime.registerNativeProvider(a.provider); runtime.registerNativeProvider(b.provider); await runtime.refresh({ allowNetwork: false });
  const manager = existingFile ? sdk.SessionManager.open(existingFile) : sdk.SessionManager.create(root, join(root, 'history'), { id: nativeId });
  const settings = sdk.SettingsManager.inMemory({ retry: { enabled: true, maxRetries: 1, baseDelayMs: 2, maxAgentDelayMs: 2, provider: { maxRetries: 0 } }, compaction: { enabled: false }, cacheWarming: 'off' });
  const resources = new sdk.DefaultResourceLoader({ cwd: root, agentDir: join(root, 'agent'), settingsManager: settings, noExtensions: true, noSkills: true, noPromptTemplates: true, noThemes: true,
    systemPromptOverride: () => 'Finite SDK-only fake admission fixture. No real credentials or model service.' });
  await resources.reload();
  return { runtime, manager, settings, resources, a, b };
}
async function runCase(kind = 'happy', root = mkdtempSync(join(tmpdir(), 'pi-native-retry-'))) {
  const owned = await createOwned(root); const { runtime, manager, settings, resources, a, b } = owned;
  const policy = fixturePolicy(root); const Handle = { Conversation: policy.Conversation, Generation: generation };
  const Runtime = { Session: policy.Conversation.Session, InstanceID: '44444444-4444-4444-8444-444444444444', RequestID: '11111111-1111-4111-8111-111111111111', Digest: 'f'.repeat(64), Generation: generation };
  const Submission = { Conversation: policy.Conversation, OperationID: '33333333-3333-4333-8333-333333333333', Action: 'send', Digest: '1'.repeat(64) };
  // Explicit fake host receipt: this fixture proves SDK mechanics, NOT durable
  // P3 admission/current-generation/effect authority or production registration.
  const receipt = Object.freeze({ scope: 'SDK-only fake admission', Submission, Accepted: true }); const originalReceipt = JSON.stringify(receipt);
  const holds = []; const gateCalls = []; const calls = []; const contexts = []; const effects = []; const retryEvents = [];
  let syncs = 0; let router; let session; let leaf;
  const persistCalls = () => writeFileSync(join(root, 'calls.json'), JSON.stringify(calls));
  const observed = (tag, response) => async (context, options, _state, model) => {
    assert.equal(options.maxRetries, 0, 'hidden provider retries enabled');
    calls.push(model.id); contexts.push({ tag, messages: structuredClone(context.messages) }); persistCalls();
    if (kind === 'bad-state' && tag === 'a-error') manager.appendCustomEntry(entryType, { provider: 'dev-workspace-router', modelId: `fallback-${policyDigest}`, state: { malformed: true } });
    return response;
  };
  a.setResponses([
    observed('prefix', ai.fauxAssistantMessage(ai.fauxToolCall('owned_effect', { label: 'prefix' }), { stopReason: 'toolUse' })),
    observed('a-error', kind === 'partial' ? errorMessage('a-error', 'partial output') : kind === 'thinking' ? errorMessage('a-error', ai.fauxThinking('partial thinking'))
      : kind === 'failed-tool' ? errorMessage('a-error', ai.fauxToolCall('owned_effect', { label: 'unsafe' }))
        : kind === 'text-refusal' ? ai.fauxAssistantMessage('I cannot assist with that request. overloaded 503') : errorMessage('a-error')),
  ]);
  b.setResponses(kind === 'budget' ? [
    observed('later-tool', ai.fauxAssistantMessage(ai.fauxToolCall('owned_effect', { label: 'later' }), { stopReason: 'toolUse' })),
    observed('b-error', errorMessage('b-error')),
  ] : [observed('reply', ai.fauxAssistantMessage('B reply'))]);
  ({ session } = await sdk.createAgentSession({ cwd: root, agentDir: join(root, 'agent'), modelRuntime: runtime, model: a.getModel('a'), settingsManager: settings, sessionManager: manager, resourceLoader: resources,
    tools: ['owned_effect'], customTools: [{ name: 'owned_effect', label: 'Owned marker', description: 'Record one fake effect', parameters: { type: 'object', properties: { label: { type: 'string' } }, required: ['label'] },
      execute: async (_id, args) => { effects.push(args.label); appendFileSync(join(root, 'effects.log'), `${args.label}\n`); return { content: [{ type: 'text', text: `completed:${args.label}` }], details: { label: args.label } }; } }] }));
  router = createNativeRetryRouting({ sdk, modelRuntime: runtime, sessionManager: manager, policy, runtimeRef: Runtime, handle: Handle,
    certifyEffects: kind === 'missing-effects' ? undefined : async () => ({ PriorEffectsComplete: true, NoPendingEffects: !(kind === 'pending' && manager.getBranch().some((e) => e.message?.responseId === 'a-error')) }),
    certifyFailure: async ({ message }) => {
      assert.ok(['a-error', 'b-error'].includes(message.responseId), 'failure lacks owned adapter attribution');
      if (kind === 'paired') manager.appendMessage({ role: 'toolResult', toolCallId: 'unproved-pair', toolName: 'owned_effect', content: [{ type: 'text', text: 'unproved' }], isError: false, timestamp: Date.now() });
      return { FailureCategory: kind === 'quota' ? 'quota_billing' : kind === 'unknown-code' ? 'overload' : 'overload', FailureCode: kind === 'unknown-code' ? 'unsupported' : kind === 'quota' ? 'insufficient_quota' : 'overloaded', HTTPStatus: 503, FailureEvidenceRef: `owned-adapter:${message.responseId}` };
    },
    prepareNativeRetry: async (request) => {
      assert.deepEqual(request.Runtime, Runtime); assert.deepEqual(request.Handle, Handle); assert.deepEqual(request.Evidence.Submission, Submission);
      assert.equal(request.Evidence.Generation, generation); gateCalls.push(structuredClone(request));
      if (kind === 'crash-before-charge') process.exit(73);
      if (kind === 'gate-loss') throw new Error('owned missing host reply');
      const current = request.Evidence.Counters;
      return current.ExtraAttempts >= 1 ? { Outcome: 'stop', Reason: 'finite original budget', Target: null, Counters: current }
        : { Outcome: 'allow', Reason: 'explicit fake host decision', Target: policy.Targets[1], Counters: { ChainIndex: 1, ExtraAttempts: 1, Hops: 1 } };
    },
    syncHistory: (file, stat) => {
      syncs++;
      if (kind === 'charge-loss' && syncs === 2) throw new Error('owned charge barrier failure');
      syncHistoryPrefix(file, stat);
      if (kind === 'crash-after-charge' && syncs === 2) process.exit(74);
    },
    onHold: (result) => holds.push(result),
  });
  await router.attach(session); leaf = manager.getLeafId();
  writeFileSync(join(root, 'metadata.json'), JSON.stringify({ policy, Runtime, Handle, receipt, file: manager.getSessionFile() }));
  session.subscribe((event) => { if (event.type === 'auto_retry_start') { retryEvents.push(event); if (kind === 'cancel') void router.cancelInput(); } });
  const originalInput = { Submission, Generation: generation, deadline: Date.now() + 15000, admit: async () => {
    const user = manager.getBranch().findLast((e) => e.type === 'message' && e.message.role === 'user');
    return { Submission: kind === 'wrong-receipt' ? { ...Submission, Digest: '2'.repeat(64) } : Submission, Generation: kind === 'wrong-generation' ? '9'.repeat(64) : generation, Accepted: true,
      Witness: { sessionId: nativeId, entryId: user.id, parentId: user.parentId, previousLeafId: leaf, effectiveMessage: 'one original input' } };
  } };
  if (kind === 'missing-deadline') {
    const { deadline: _deadline, ...missing } = originalInput;
    assert.throws(() => router.beginInput(missing), /trusted original input unavailable/);
    assert.deepEqual(calls, []); assert.deepEqual(effects, []); assert.equal(manager.getBranch().filter((e) => e.message?.role === 'user').length, 0);
    router.dispose(); session.dispose(); await runtime.settleCredentialWrites(); return;
  }
  router.beginInput(originalInput);
  await session.prompt('one original input', { expandPromptTemplates: false });
  await session.waitForIdle();
  const disk = sdk.SessionManager.open(manager.getSessionFile()); const branch = disk.getBranch();
  const states = branch.filter((e) => e.type === 'custom' && e.customType === entryType);
  const users = branch.filter((e) => e.type === 'message' && e.message.role === 'user');
  assert.equal(users.length, 1, 'input reappended'); assert.equal(JSON.stringify(receipt), originalReceipt, 'original fake receipt changed');
  assert.equal(manager.getSessionId(), nativeId); assert.equal(disk.getSessionId(), nativeId);
  assert.equal(session.model.provider, 'dev-workspace-router'); assert.equal(session.model.id, router.selection.Model);
  const report = { kind, calls, effects, gateCalls: gateCalls.length, charges: router.inspect().charges.map((e) => e.state), stateEntries: states.length,
    held: router.inspect().held, stopped: router.inspect().stopped, holds, userEntries: users.length, retryEvents: retryEvents.length };
  const globals = settings.getGlobalSettings(); assert.equal(globals.defaultProvider, undefined); assert.equal(globals.defaultModel, undefined);
  router.dispose(); session.dispose(); await runtime.settleCredentialWrites();
  return { report, states, contexts, branch, root };
}

if (process.argv[2] === '--fault-child') {
  await runCase(process.argv[3], process.argv[4]);
  throw new Error('fault child did not reach its exact crash point');
} else {
  test('public SDK retry charges once before B, keeps one input/tool prefix and separate selection', async () => {
    const result = await runCase(); const { report, states, contexts } = result;
    assert.deepEqual(report.calls, ['a', 'a', 'b'], JSON.stringify(report)); assert.deepEqual(report.effects, ['prefix']); assert.equal(report.gateCalls, 1);
    assert.equal(states.length, 2, 'SDK appended returned state twice'); assert.deepEqual(states.map((e) => e.data.state.extraAttempts), [0, 1]);
    assert.equal(report.held, false); assert.equal(report.stopped, false);
    const before = contexts.find((e) => e.tag === 'a-error').messages.find((m) => m.role === 'toolResult');
    const after = contexts.find((e) => e.tag === 'reply').messages.find((m) => m.role === 'toolResult');
    assert.deepEqual(after, before, 'completed tool prefix changed across provider');
    const physical = result.branch.filter((e) => e.message?.role === 'assistant').map((e) => e.message.provider);
    assert.ok(physical.includes('fixture-a') && physical.includes('fixture-b')); console.log(JSON.stringify(report));
  });
  test('successful B tool continuation cannot reset original budget before C', async () => {
    const { report, states } = await runCase('budget');
    assert.deepEqual(report.calls, ['a', 'a', 'b', 'b'], JSON.stringify(report)); assert.deepEqual(report.effects, ['prefix', 'later']);
    assert.equal(report.gateCalls, 2); assert.equal(states.length, 2); assert.equal(report.stopped, true); assert.ok(!report.calls.includes('c'));
    console.log(JSON.stringify(report));
  });
  test('finite fault cases never dispatch B or replay effects', async (t) => {
    for (const kind of ['partial', 'thinking', 'failed-tool', 'pending', 'paired', 'bad-state', 'quota', 'unknown-code', 'cancel', 'wrong-receipt', 'wrong-generation', 'charge-loss', 'missing-effects', 'gate-loss']) await t.test(kind, async () => {
      const { report } = await runCase(kind);
      const beforeInput = ['wrong-receipt', 'wrong-generation', 'missing-effects'].includes(kind);
      assert.deepEqual(report.calls, beforeInput ? [] : ['a', 'a'], JSON.stringify(report));
      assert.deepEqual(report.effects, beforeInput ? [] : ['prefix'], JSON.stringify(report));
      assert.equal(report.charges.length, beforeInput ? 0 : 1, JSON.stringify(report));
      assert.equal(report.stateEntries, ['charge-loss', 'bad-state'].includes(kind) ? 2 : beforeInput ? 0 : 1, JSON.stringify(report));
      assert.equal(report.gateCalls, ['charge-loss', 'paired', 'gate-loss'].includes(kind) ? 1 : 0, JSON.stringify(report));
      assert.ok(!report.calls.includes('b') && !report.calls.includes('c'), kind);
      assert.ok(!report.effects.includes('unsafe'), kind); assert.ok(report.held || report.stopped || kind === 'cancel', kind);
      console.log(JSON.stringify(report));
    });
  });
  test('missing original deadline refuses before input and model text refusal is not a retry', async () => {
    await runCase('missing-deadline');
    const { report } = await runCase('text-refusal');
    assert.deepEqual(report.calls, ['a', 'a']); assert.deepEqual(report.effects, ['prefix']);
    assert.equal(report.gateCalls, 0); assert.equal(report.retryEvents, 0); assert.equal(report.stateEntries, 1);
    assert.equal(report.held, false); assert.equal(report.stopped, false); console.log(JSON.stringify(report));
  });
  test('owned process death before/after charge holds and never replays the original input', async () => {
    for (const [kind, status, charged] of [['crash-before-charge', 73, 1], ['crash-after-charge', 74, 2]]) {
      const root = mkdtempSync(join(tmpdir(), 'pi-native-retry-crash-'));
      const child = spawnSync(nodeBinary, [self, '--fault-child', kind, root], { env: { PI_PROBE_PACKAGE: packageRoot, PI_OFFLINE: '1' }, encoding: 'utf8', timeout: 30000 });
      assert.equal(child.status, status, child.stderr); assert.equal(child.signal, null);
      const metadata = JSON.parse(readFileSync(join(root, 'metadata.json'), 'utf8'));
      const calls = JSON.parse(readFileSync(join(root, 'calls.json'), 'utf8')); assert.deepEqual(calls, ['a', 'a']);
      const manager = sdk.SessionManager.open(metadata.file); const states = manager.getBranch().filter((e) => e.type === 'custom' && e.customType === entryType);
      assert.equal(states.length, charged); assert.equal(manager.getBranch().filter((e) => e.message?.role === 'user').length, 1);
      const { runtime } = await createOwned(root, metadata.file);
      const router = createNativeRetryRouting({ sdk, modelRuntime: runtime, sessionManager: manager, policy: metadata.policy, runtimeRef: metadata.Runtime, handle: metadata.Handle });
      assert.throws(() => router.beginInput({ Submission: metadata.receipt.Submission, Generation: generation, deadline: Date.now() + 10000, admit: async () => { throw new Error('must not admit'); } }), /cannot resume/);
      await runtime.settleCredentialWrites(); console.log(JSON.stringify({ kind, status, states: charged, calls, replayed: false }));
    }
  });
}
