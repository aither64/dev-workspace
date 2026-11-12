import { appendFileSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createInterface } from 'node:readline';
import { setTimeout as delay } from 'node:timers/promises';
import { sdk } from './package-api.mjs';
import { fakeProvider, script } from './fake-provider.mjs';

// Finite P0 fixture transport, not a production admission protocol.
const config = JSON.parse(readFileSync(process.argv[2], 'utf8'));
const runtime = await sdk.ModelRuntime.create({
  authPath: join(config.authDir, 'auth.json'),
  modelsPath: join(config.agentDir, 'models.json'),
  allowModelNetwork: false,
});
const fake = fakeProvider();
runtime.registerNativeProvider(fake.provider);
const alternate = fakeProvider('p0-alternate');
runtime.registerNativeProvider(alternate.provider);
await runtime.refresh({ allowNetwork: false });
const settings = sdk.SettingsManager.create(config.cwd, config.agentDir);
const resources = new sdk.DefaultResourceLoader({
  cwd: config.cwd, agentDir: config.agentDir, settingsManager: settings,
  noExtensions: true, noSkills: true, noPromptTemplates: true, noThemes: true,
  systemPromptOverride: () => 'Finite fake-provider probe. No real account access.',
});
await resources.reload();
const manager = config.sessionFile
  ? sdk.SessionManager.open(config.sessionFile)
  : sdk.SessionManager.create(config.cwd, config.historyDir);
let pauseEffectResult = false;
const { session } = await sdk.createAgentSession({
  cwd: config.cwd, agentDir: config.agentDir, modelRuntime: runtime,
  model: fake.getModel('p0-model'), settingsManager: settings,
  sessionManager: manager, resourceLoader: resources, tools: ['probe_effect'],
  customTools: [{
    name: 'probe_effect', label: 'P0 owned effect', description: 'Write only the owned P0 marker',
    parameters: { type: 'object', properties: {} },
    execute: async (_id, _args, signal) => {
      appendFileSync(`${config.effectLog}.intent`, 'intent\n');
      appendFileSync(config.effectLog, 'effect\n');
      emit({ type: 'probe_tool_effect' });
      if (pauseEffectResult) await delay(10000, undefined, { signal });
      return { content: [{ type: 'text', text: 'owned fixture marker written' }], details: { member: config.member, sessionId: session.sessionId } };
    },
  }],
});
const emit = (record) => process.stdout.write(`${JSON.stringify(record)}\n`);
session.subscribe((event) => emit({ type: 'event', event }));
emit({ type: 'ready', pid: process.pid, member: config.member, sessionId: session.sessionId, sessionFile: session.sessionFile });

async function command(request) {
  switch (request.type) {
    case 'prompt':
      pauseEffectResult = request.pauseEffectResult === true;
      script(session.model.provider === 'p0-alternate' ? alternate : fake, request.script);
      await session.prompt(request.message);
      // Fault injection after native work, before the fixture acknowledgement.
      if (request.loseAck) process.exit(73);
      return { disposition: 'completed', sessionId: session.sessionId };
    case 'abort': await session.abort(); return { idle: session.isStreaming === false };
    case 'history': return { messages: session.messages, entries: manager.getEntries() };
    case 'model':
      await session.setModel((request.provider === 'p0-alternate' ? alternate : fake).getModel(request.model), { persist: request.persist === true });
      await settings.flush();
      return { model: session.model.id, defaults: settings.getGlobalSettings() };
    case 'auth': {
      const auth = await runtime.getAuth('p0-fake');
      // Fake token equality only; never put credentials into protocol output.
      return { refreshed: auth?.auth.apiKey === 'fake-refreshed', replacement: auth?.auth.apiKey === 'fake-replaced' };
    }
    case 'refuse': return { outcome: 'NotSubmitted', reason: 'fixture refused before calling Pi' };
    default: throw new Error(`Unsupported finite probe operation: ${request.type}`);
  }
}
const pending = new Set();
const lines = createInterface({ input: process.stdin });
lines.on('line', (line) => {
  const request = JSON.parse(line);
  const task = command(request).then(
    (data) => emit({ type: 'response', id: request.id, data }),
    (error) => emit({ type: 'response', id: request.id, error: error.message }),
  ).finally(() => pending.delete(task));
  pending.add(task);
});
lines.on('close', async () => {
  await session.abort();
  await Promise.allSettled(pending);
  session.dispose();
  await runtime.settleCredentialWrites();
});
