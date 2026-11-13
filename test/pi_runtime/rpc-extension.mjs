import { appendFileSync } from 'node:fs';
import { fakeProvider, script } from './fake-provider.mjs';

// Reviewed first-party extension candidate; stock Pi RPC remains unmodified.
export default function probeExtension(pi) {
  const fake = fakeProvider();
  pi.registerProvider(fake.provider);
  pi.on('before_agent_start', (event) => {
    script(fake, event.prompt.includes('SLOW') ? 'slow' : event.prompt.includes('TOOL') ? 'tool' : 'text');
  });
  pi.registerTool({
    name: 'probe_effect', label: 'P0 marker', description: 'Owned P0 marker only',
    parameters: { type: 'object', properties: {} },
    execute: async () => {
      appendFileSync(process.env.PI_PROBE_EFFECT_LOG, 'effect\n');
      return { content: [{ type: 'text', text: 'owned fixture effect' }], details: {} };
    },
  });
}
