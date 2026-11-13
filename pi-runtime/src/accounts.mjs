import { existsSync, readFileSync, statSync } from 'node:fs';

const identifier = /^[a-z0-9][a-z0-9_-]{0,62}$/;
export function assertAccountBinding(bindings, settings) {
  if (!Array.isArray(bindings) || !bindings.length || bindings.length > 64) throw new Error('native account mapping unavailable');
  let previous = '';
  for (const binding of bindings) {
    if (!binding || Object.keys(binding).sort().join(',') !== 'account_ref,provider'
      || typeof binding.provider !== 'string' || typeof binding.account_ref !== 'string'
      || !identifier.test(binding.provider) || !identifier.test(binding.account_ref)
      || (previous && previous >= binding.provider)) throw new Error('native account mapping unavailable');
    previous = binding.provider;
  }
  if (!bindings.some((binding) => binding.provider === settings.Provider && binding.account_ref === settings.AccountRef)) {
    throw new Error('native account mapping unavailable');
  }
}

// Check the actual provider/model/override locations accepted by pinned Pi's
// public model-config schema. Never inspect or report credential field values.
export function assertNoAuthOverrides(config) {
  const check = (object) => {
    if (!object || typeof object !== 'object' || Array.isArray(object)) throw new Error('invalid native model configuration');
    for (const field of ['apiKey', 'headers', 'authHeader', 'oauth']) {
      if (Object.hasOwn(object, field)) throw new Error('native credential override unavailable');
    }
  };
  if (!config || typeof config !== 'object' || Array.isArray(config)) throw new Error('invalid native model configuration');
  for (const provider of Object.values(config.providers ?? {})) {
    check(provider);
    for (const model of provider.models ?? []) check(model);
    for (const model of Object.values(provider.modelOverrides ?? {})) check(model);
  }
}

export function checkModelsFile(path) {
  if (!existsSync(path)) return;
  const info = statSync(path);
  if (!info.isFile() || info.size > 1024 * 1024) throw new Error('native model configuration unavailable');
  assertNoAuthOverrides(JSON.parse(readFileSync(path, 'utf8')));
}

export function assertStoredAccount(runtime, provider) {
  if (runtime.getRegisteredProviderIds().length) throw new Error('custom registered auth configuration unavailable');
  const status = runtime.getProviderAuthStatus(provider);
  if (!status.configured || status.source !== 'stored') throw new Error('canonical stored account unavailable');
  if (runtime.getError()) throw new Error('native model configuration unavailable');
}
