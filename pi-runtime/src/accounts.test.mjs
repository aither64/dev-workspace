import assert from 'node:assert/strict';
import test from 'node:test';
import { assertAccountBinding, assertNoAuthOverrides, assertStoredAccount } from './accounts.mjs';

test('canonical account projection and stored source refuse aliases and overrides', () => {
  const settings = { Provider: 'native-provider', AccountRef: 'catalog-account' };
  const bindings = [{ provider: settings.Provider, account_ref: settings.AccountRef }];
  assertAccountBinding(bindings, settings);
  for (const bad of [undefined, [], [...bindings, ...bindings], [{ provider: settings.Provider, account_ref: 'other' }], [{ provider: 'native-provider', account_ref: 'catalog-account', path: '/caller' }]]) {
    assert.throws(() => assertAccountBinding(bad, settings));
  }
  const runtime = (source, configured = true, registered = []) => ({
    getProviderAuthStatus: () => ({ configured, source }), getRegisteredProviderIds: () => registered, getError: () => undefined,
  });
  assertStoredAccount(runtime('stored'), settings.Provider);
  for (const source of ['runtime', 'environment', 'fallback', 'models_json_key', 'models_json_command', undefined]) {
    assert.throws(() => assertStoredAccount(runtime(source), settings.Provider));
  }
  assert.throws(() => assertStoredAccount(runtime('stored', false), settings.Provider));
  assert.throws(() => assertStoredAccount(runtime('stored', true, ['custom']), settings.Provider));
  const clean = { providers: { 'native-provider': { baseUrl: 'https://invalid.example/v1', api: 'openai-responses', models: [{ id: 'custom' }], modelOverrides: { custom: { contextWindow: 8192 } } } } };
  assertNoAuthOverrides(clean);
  for (const field of ['apiKey', 'headers', 'authHeader', 'oauth']) {
    // A stored status can mask a configured override; both gates are required.
    assertStoredAccount(runtime('stored'), settings.Provider);
    for (const make of [() => ({ providers: { p: { [field]: null } } }), () => ({ providers: { p: { models: [{ id: 'm', [field]: null }] } } }), () => ({ providers: { p: { modelOverrides: { m: { [field]: null } } } } })]) {
      assert.throws(() => assertNoAuthOverrides(make()));
    }
  }
});
