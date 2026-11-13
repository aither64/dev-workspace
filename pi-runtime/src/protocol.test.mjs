import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import {WorkerSession} from './session.mjs';

test('lost role application proof holds before input or settings effects', async () => {
  let prompts = 0, mutations = 0;
  const session = {subscribe() {}, isStreaming: false, prompt() {prompts++;}, setModel() {mutations++;}};
  const worker = new WorkerSession({}, session, {}, {verifyInstructions: () => {throw Error('lost actual observation');}});
  worker.settings = {Revision: 1};
  assert.deepEqual(await worker.send('original goal', 'a'.repeat(32)), {outcome: 'Unknown'});
  assert.equal(worker.pending, null); assert.equal(worker.activeInput, undefined);
  assert.equal((await worker.configure({Revision: 2})).Outcome, 'unknown');
  assert.throws(() => worker.snapshot(), /role instructions unavailable/);
  assert.equal(worker.held, true); assert.equal(prompts, 0); assert.equal(mutations, 0);
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
  assert.deepEqual(result.body, { workerProtocol: 1, hostCallbacks: 1, linearLineage: 1 });
});
