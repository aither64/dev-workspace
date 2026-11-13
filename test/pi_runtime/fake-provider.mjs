import { appendFileSync } from 'node:fs';
import { setTimeout as delay } from 'node:timers/promises';
import { ai } from './package-api.mjs';

export function fakeProvider(provider = 'p0-fake') {
  const fake = ai.fauxProvider({
    provider, api: `${provider}-api`,
    models: [{ id: 'p0-model', reasoning: true }, { id: 'p0-other', reasoning: true }],
    tokensPerSecond: 1000,
    tokenSize: { min: 1, max: 4 },
  });
  // Exercise Pi's real shared credential store and refresh lock, with fake data.
  const auth = {
    apiKey: {
      name: 'P0 fake key',
      login: async (interaction) => ({ type: 'api_key', key: await interaction.prompt({ type: 'secret', message: 'fake' }) }),
      resolve: async ({ credential }) => credential?.key
        ? { auth: { apiKey: credential.key }, source: 'P0 fake stored key' } : undefined,
    },
    oauth: {
      name: 'P0 fake OAuth',
      login: async () => { throw new Error('P0 never performs interactive login'); },
      refresh: async (credential, signal) => {
        appendFileSync(process.env.PI_PROBE_REFRESH_LOG, 'refresh\n');
        await delay(40, undefined, { signal });
        return { ...credential, access: 'fake-refreshed', expires: Date.now() + 3600000 };
      },
      toAuth: (credential) => {
        if (process.env.PI_PROBE_AUTH_LOG) appendFileSync(process.env.PI_PROBE_AUTH_LOG,
          credential.access === 'fake-replaced-again' ? 'replacement-seen\n' : 'other-fake-token\n');
        return { apiKey: credential.access };
      },
    },
  };
  return { ...fake, provider: { ...fake.provider, auth } };
}

export function script(fake, kind = 'text') {
  if (kind === 'slow') {
    fake.setResponses([async (_context, options) => {
      await delay(10000, undefined, { signal: options?.signal });
      return ai.fauxAssistantMessage('late answer');
    }]);
  } else if (kind === 'tool') {
    fake.setResponses([
      ai.fauxAssistantMessage(ai.fauxToolCall('probe_effect', {}), { stopReason: 'toolUse' }),
      ai.fauxAssistantMessage('effect observed'),
    ]);
  } else {
    fake.setResponses([ai.fauxAssistantMessage('finite fake answer')]);
  }
}
