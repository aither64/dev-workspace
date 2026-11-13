import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { runWorker } from '../src/protocol.mjs';

// Source fixture only; never installed as the production worker entrypoint.
await runWorker({ sessionOptions: async (sdk, config) => {
  const root = join(config.piPackage, 'lib/pi/node_modules/@earendil-works/pi-ai');
  const manifest = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8'));
  const ai = await import(pathToFileURL(join(root, manifest.exports?.['.']?.import ?? manifest.main)).href);
  const runtime = await sdk.ModelRuntime.create({ authPath: join(config.authDirectory, 'auth.json'), modelsPath: null });
  const fake = ai.fauxProvider({ provider: 'p3-fake', models: [{ id: 'p3-model' }, { id: 'p3-other' }] });
  const alternate = ai.fauxProvider({ provider: 'p3-alternate', models: [{ id: 'p3-model' }] });
  runtime.registerNativeProvider(fake.provider); runtime.registerNativeProvider(alternate.provider); await runtime.refresh({ allowNetwork: false });
  fake.setResponses(Array.from({ length: 10 }, () => ai.fauxAssistantMessage('fake native answer')));
  alternate.setResponses(Array.from({ length: 10 }, () => ai.fauxAssistantMessage('alternate native answer')));
  return { modelRuntime: runtime, sessionOptions: { tools: [] } };
} });
