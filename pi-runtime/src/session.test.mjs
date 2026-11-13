import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';
import { sdk, ai } from '../../test/pi_runtime/package-api.mjs';
import { createSession, WorkerSession } from './session.mjs';

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
