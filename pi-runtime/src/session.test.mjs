import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { sdk, ai } from '../../test/pi_runtime/package-api.mjs';
import { createSession } from './session.mjs';

test('ordinary fresh and retained input has exact independent persisted witness before acknowledgement', async () => {
  const root = mkdtempSync(join(tmpdir(), 'pi-p3-witness-'));
  const dirs = ['agent', 'history', 'repo', 'auth'];
  for (const dir of dirs) mkdirSync(join(root, dir));
  const runtime = await sdk.ModelRuntime.create({ authPath: join(root, 'auth/auth.json'), modelsPath: null });
  const fake = ai.fauxProvider({ provider: 'p3-fake', models: [{ id: 'p3-model' }, { id: 'p3-other' }] });
  runtime.registerNativeProvider(fake.provider); await runtime.refresh({ allowNetwork: false });
  const config = { nativeId: '11111111-1111-4111-8111-111111111111', emptyProven: true,
    cwd: join(root, 'repo'), agentDir: join(root, 'agent'), historyDir: join(root, 'history'), authDirectory: join(root, 'auth'),
    settings: { Provider: 'p3-fake', AccountRef: 'fake-account', Model: 'p3-model', Thinking: 'off', PolicyRevision: 'fake-policy', Revision: 1 } };
  let worker;
  try {
    worker = await createSession(sdk, config, { modelRuntime: runtime, sessionOptions: { tools: [] } });
    const witnesses = [];
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
    const next = { ...config.settings, Model: 'p3-other', Revision: 2 };
    assert.equal((await worker.configure(next)).Outcome, 'applied');
    assert.equal((await worker.configure(next)).Outcome, 'applied');
    assert.equal((await worker.configure({ ...next, Model: 'p3-model' })).Outcome, 'rejected');
    assert.equal((await worker.configure({ ...next, Revision: 4 })).Outcome, 'rejected');
    assert.equal((await worker.configure({ ...next, Revision: 3, AccountRef: 'unproved-account' })).Outcome, 'rejected');
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
