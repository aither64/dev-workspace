import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtempSync, mkdirSync, readFileSync, renameSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import { randomUUID } from 'node:crypto';
import test from 'node:test';
import { nodeBinary, packageRoot } from './package-api.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const credentials = (access = 'fake-expired', expires = 0) => ({
  'p0-fake': { type: 'oauth', access, refresh: 'fake-refresh', expires },
  'p0-alternate': { type: 'api_key', key: 'fake-alternate' },
});

class Child {
  records = [];
  listeners = new Set();
  stderr = '';
  ignoredResponseIds = new Set();
  constructor(argv, env, cwd) {
    this.process = spawn(argv[0], argv.slice(1), { cwd, env, stdio: ['pipe', 'pipe', 'pipe'] });
    this.exited = new Promise((resolveExit) => this.process.once('close', (code, signal) => resolveExit([code, signal])));
    this.process.on('error', (error) => { this.stderr += error.message; });
    this.process.stderr.on('data', (chunk) => { this.stderr += chunk; });
    createInterface({ input: this.process.stdout }).on('line', (line) => {
      let record;
      try { record = JSON.parse(line); } catch { return; }
      if (record.type === 'response' && this.ignoredResponseIds.has(record.id)) return;
      this.records.push(record);
      for (const listener of this.listeners) listener(record);
    });
  }
  until(predicate, timeout = 8000, after = 0) {
    const existing = this.records.slice(after).find(predicate);
    if (existing) return Promise.resolve(existing);
    return new Promise((resolve, reject) => {
      const cleanup = () => { clearTimeout(timer); this.listeners.delete(listener); };
      const listener = (record) => { if (predicate(record)) { cleanup(); resolve(record); } };
      const timer = setTimeout(() => { cleanup(); reject(new Error(`Finite fixture timed out: ${this.stderr.slice(-2000)}`)); }, timeout);
      this.listeners.add(listener);
    });
  }
  send(type, fields = {}) {
    const id = randomUUID();
    this.process.stdin.write(`${JSON.stringify({ id, type, ...fields })}\n`);
    return id;
  }
  async request(type, fields) {
    const after = this.records.length;
    const id = this.send(type, fields);
    const response = await this.until((r) => r.type === 'response' && r.id === id, 8000, after);
    assert.equal(response.error, undefined);
    assert.notEqual(response.success, false, response.error);
    return response.data;
  }
  async close() {
    if (this.process.exitCode !== null || this.process.signalCode !== null) return;
    this.process.stdin.end();
    const timer = setTimeout(() => this.process.kill('SIGKILL'), 5000);
    try { await this.exited; } finally { clearTimeout(timer); }
  }
}

test('finite SDK/RPC fixture: private workers, canonical auth, history and honest Unknown', { timeout: 45000 }, async (t) => {
  const root = mkdtempSync(join(tmpdir(), 'pi-runtime-p0-'));
  const children = [];
  const authDir = join(root, 'canonical-auth');
  mkdirSync(authDir, { mode: 0o700 });
  const authFile = join(authDir, 'auth.json');
  writeFileSync(authFile, JSON.stringify(credentials()), { mode: 0o600 });
  const refreshLog = join(root, 'refresh.log');
  writeFileSync(refreshLog, '');
  const env = {
    PATH: dirname(nodeBinary), HOME: join(root, 'home'), PI_OFFLINE: '1',
    PI_PROBE_PACKAGE: packageRoot, PI_PROBE_REFRESH_LOG: refreshLog,
    TMPDIR: root, LANG: 'C.UTF-8',
  };
  mkdirSync(env.HOME);
  function configuration(session, member) {
    const base = join(root, session, member);
    const config = { cwd: join(base, 'repo'), agentDir: join(base, 'agent'),
      historyDir: join(base, 'history'), effectLog: join(base, 'effects.log'), authDir, member };
    for (const dir of [config.cwd, config.agentDir, config.historyDir]) mkdirSync(dir, { recursive: true, mode: 0o700 });
    writeFileSync(join(config.agentDir, 'settings.json'), JSON.stringify({ cacheWarming: 'off', defaultProvider: 'p0-fake', defaultModel: 'p0-model' }));
    return config;
  }
  async function sdkWorker(config) {
    const file = join(config.agentDir, 'probe-config.json');
    writeFileSync(file, JSON.stringify(config));
    const child = new Child([nodeBinary, join(here, 'sdk-worker.mjs'), file], env, config.cwd);
    children.push(child);
    child.ready = await child.until((r) => r.type === 'ready');
    return child;
  }
  try {
    const configs = ['session-a', 'session-b'].flatMap((session) => ['lead', 'member'].map((member) => configuration(session, member)));
    const workers = await Promise.all(configs.map(sdkWorker));
    assert.equal(new Set(workers.map((w) => w.ready.pid)).size, 4);
    assert.equal(new Set(workers.map((w) => w.ready.sessionId)).size, 4);
    assert.equal(new Set(workers.map((w) => w.ready.sessionFile)).size, 4);
    await Promise.all(workers.map((w) => w.request('auth')));
    assert.equal(readFileSync(refreshLog, 'utf8'), 'refresh\n', 'native shared lock refreshes one fake grant once');
    for (const worker of workers) assert.equal((await worker.request('auth')).refreshed, true);

    const first = workers[0];
    const originalId = first.ready.sessionId;
    await first.request('prompt', { message: 'manual fake send' });
    assert(first.records.some((r) => r.event?.type === 'message_update'));
    assert(first.records.some((r) => r.event?.type === 'agent_settled'));
    const selected = await first.request('model', { provider: 'p0-alternate', model: 'p0-other' });
    assert.equal(selected.defaults.defaultProvider, 'p0-fake');
    await first.request('prompt', { message: 'same conversation on alternate fake provider' });
    assert.equal(first.ready.sessionId, originalId);
    const history = await first.request('history');
    assert(history.messages.some((m) => m.role === 'user' && m.content.some((b) => b.text === 'manual fake send')));
    assert(history.entries.some((e) => e.type === 'model_change' && e.provider === 'p0-alternate'));
    for (const peer of workers.slice(1)) assert.equal((await peer.request('history')).messages.length, 0);
    await first.request('model', { model: 'p0-other', persist: true });
    assert.equal(JSON.parse(readFileSync(join(configs[0].agentDir, 'settings.json'))).defaultModel, 'p0-other');
    for (const config of configs.slice(1)) assert.equal(JSON.parse(readFileSync(join(config.agentDir, 'settings.json'))).defaultModel, 'p0-model');

    const replacement = join(authDir, 'next.json');
    writeFileSync(replacement, JSON.stringify(credentials('fake-replaced', Date.now() + 3600000)), { mode: 0o600 });
    renameSync(replacement, authFile);
    for (const worker of workers) assert.equal((await worker.request('auth')).replacement, true);

    const slowId = first.send('prompt', { message: 'abort controlled fake request', script: 'slow' });
    await first.until((r) => r.event?.type === 'message_start' && JSON.stringify(r.event).includes('abort controlled fake request'));
    await first.request('abort');
    await first.until((r) => r.type === 'response' && r.id === slowId);
    assert.equal((await first.request('refuse')).outcome, 'NotSubmitted');

    const uncertainId = first.send('prompt', { message: 'lost acknowledgement boundary', script: 'tool', loseAck: true });
    assert.equal((await first.exited)[0], 73);
    assert(!first.records.some((r) => r.id === uncertainId));
    const restored = await sdkWorker({ ...configs[0], sessionFile: first.ready.sessionFile });
    const recovered = await restored.request('history');
    assert.equal(restored.ready.sessionId, originalId);
    assert(recovered.messages.some((m) => m.role === 'user' && m.content.some((b) => b.text === 'lost acknowledgement boundary')));
    assert(!JSON.stringify(recovered.entries).includes(uncertainId), 'transport correlation is not native durable request identity');
    assert.equal(readFileSync(configs[0].effectLog, 'utf8'), 'effect\n');
    // Deliberately no resend: history is observation, not a stable admission receipt.
    t.diagnostic(`Unacknowledged operation ${uncertainId}: native history and effect exist, native request UUID absent; keep Unknown, no resend.`);

    restored.send('prompt', { message: 'tool effect before committed result', script: 'tool', pauseEffectResult: true });
    await restored.until((r) => r.type === 'probe_tool_effect');
    restored.process.kill('SIGKILL');
    assert.equal((await restored.exited)[1], 'SIGKILL');
    const interrupted = await sdkWorker({ ...configs[0], sessionFile: first.ready.sessionFile });
    await interrupted.request('history');
    assert.equal(readFileSync(configs[0].effectLog, 'utf8'), 'effect\neffect\n');
    assert.equal(readFileSync(`${configs[0].effectLog}.intent`, 'utf8'), 'intent\nintent\n');
    t.diagnostic('Tool-effect/result crash: worker exited; native history reopened without resend; arbitrary tool outcome remains Unknown.');

    const rpcConfig = configuration('rpc-comparison', 'lead');
    symlinkSync(authFile, join(rpcConfig.agentDir, 'auth.json'));
    const authLog = join(root, 'rpc-auth.log');
    function rpcWorker(sessionFile) {
      const child = new Child([join(packageRoot, 'bin/pi'), '--mode', 'rpc', '--no-skills', '--no-prompt-templates', '--no-themes',
      '--no-extensions', '--extension', join(here, 'rpc-extension.mjs'), '--provider', 'p0-fake', '--model', 'p0-model',
      '--session-dir', rpcConfig.historyDir, '--tools', 'probe_effect', '--no-approve', ...(sessionFile ? ['--session', sessionFile] : [])],
      { ...env, PI_CODING_AGENT_DIR: rpcConfig.agentDir, PI_PROBE_EFFECT_LOG: rpcConfig.effectLog, PI_PROBE_AUTH_LOG: authLog }, rpcConfig.cwd);
      children.push(child);
      return child;
    }
    const rpc = rpcWorker();
    const rpcState = await rpc.request('get_state');
    await rpc.request('prompt', { message: 'stock RPC fake send' });
    await rpc.until((r) => r.type === 'agent_settled');
    assert(rpc.records.some((r) => r.type === 'message_update'));
    assert((await rpc.request('get_messages')).messages.some((m) => m.role === 'user'));
    await rpc.request('set_model', { provider: 'p0-fake', modelId: 'p0-other' });
    assert.equal((await rpc.request('get_state')).sessionId, rpcState.sessionId);
    assert.equal(JSON.parse(readFileSync(join(rpcConfig.agentDir, 'settings.json'))).defaultModel, 'p0-model');
    writeFileSync(replacement, JSON.stringify(credentials('fake-replaced-again', Date.now() + 3600000)), { mode: 0o600 });
    renameSync(replacement, authFile);
    const beforeReplacement = rpc.records.length;
    await rpc.request('prompt', { message: 'observe canonical replacement through RPC alias' });
    await rpc.until((r) => r.type === 'agent_settled', 8000, beforeReplacement);
    assert(readFileSync(authLog, 'utf8').includes('replacement-seen\n'));
    const beforeAbort = rpc.records.length;
    await rpc.request('prompt', { message: 'SLOW fake RPC request' });
    await rpc.request('abort');
    await rpc.until((r) => r.type === 'agent_settled', 8000, beforeAbort);
    assert.equal((await rpc.request('get_state')).isStreaming, false);
    const lostRpcId = randomUUID();
    const beforeLost = rpc.records.length;
    rpc.ignoredResponseIds.add(lostRpcId);
    rpc.process.stdin.write(`${JSON.stringify({ id: lostRpcId, type: 'prompt', message: 'RPC lost native acceptance reply' })}\n`);
    await rpc.until((r) => r.type === 'agent_settled', 8000, beforeLost);
    rpc.process.kill('SIGKILL');
    await rpc.exited;
    assert(!rpc.records.some((r) => r.id === lostRpcId));
    const rpcRestored = rpcWorker(rpcState.sessionFile);
    assert.equal((await rpcRestored.request('get_state')).sessionId, rpcState.sessionId);
    const rpcHistory = await rpcRestored.request('get_entries');
    assert(JSON.stringify(rpcHistory).includes('RPC lost native acceptance reply'));
    assert(!JSON.stringify(rpcHistory).includes(lostRpcId));
    t.diagnostic(JSON.stringify({ package: packageRoot, workers: 4, sharedRefreshes: 1,
      sdk: 'manual API passed', rpc: 'manual API passed', admission: 'Unknown held; no native request UUID proof',
      launch: 'separate containment fixture required' }));
  } finally {
    await Promise.all(children.map((child) => child.close()));
    assert(children.every((child) => child.process.exitCode !== null || child.process.signalCode !== null));
    rmSync(root, { recursive: true });
  }
});
