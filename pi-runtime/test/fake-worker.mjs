import { readFileSync, appendFileSync } from 'node:fs';
import * as fs from 'node:fs';
import { syncHistoryPrefix } from '../src/persistence.mjs';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { runWorker } from '../src/protocol.mjs';

// Source fixture only; never installed as the production worker entrypoint.
let fixtureAgentDir;
await runWorker({ onError: (error) => { if (fixtureAgentDir) appendFileSync(join(fixtureAgentDir, "fixture-error.log"), `${error.stack}\n`); }, sessionOptions: async (sdk, config) => {
  fixtureAgentDir = config.agentDir;
  const root = join(config.piPackage, 'lib/pi/node_modules/@earendil-works/pi-ai');
  const manifest = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8'));
  const ai = await import(pathToFileURL(join(root, manifest.exports?.['.']?.import ?? manifest.main)).href);
  const runtime = await sdk.ModelRuntime.create({ authPath: join(config.authDirectory, 'auth.json'), modelsPath: null });
  const fake = ai.fauxProvider({ provider: 'p3-fake', models: [{ id: 'p3-model' }, { id: 'p3-other' }] });
  const alternate = ai.fauxProvider({ provider: 'p3-alternate', models: [{ id: 'p3-model' }] });
  runtime.registerNativeProvider(fake.provider); runtime.registerNativeProvider(alternate.provider); await runtime.refresh({ allowNetwork: false });
  if (config.hostTools?.includes('report_to_lead')) {
    fake.setResponses([
      ai.fauxAssistantMessage([
        ai.fauxToolCall('report_to_lead', { message_id: '77777777-7777-4777-8777-777777777777', message: '<é> native report one' }),
        ai.fauxToolCall('report_to_lead', { message_id: '88888888-8888-4888-8888-888888888888', message: 'native report two' }),
      ], { stopReason: 'toolUse' }),
      ai.fauxAssistantMessage('reports observed'),
    ]);
  } else fake.setResponses(Array.from({ length: 10 }, () => ai.fauxAssistantMessage('fake native answer')));
  alternate.setResponses(Array.from({ length: 10 }, () => ai.fauxAssistantMessage('alternate native answer')));
  return { modelRuntime: runtime, sessionOptions: { tools: [] }, syncHistory: (file, expected) => {
    const trace = join(config.historyDir, '../sync-order.log');
    syncHistoryPrefix(file, expected, { ...fs, fsyncSync: (fd) => { fs.fsyncSync(fd); appendFileSync(trace, fs.fstatSync(fd).isDirectory() ? 'directory-sync\n' : 'file-sync\n'); } });
    appendFileSync(trace, 'native-sync-complete\n');
  } };
} });
