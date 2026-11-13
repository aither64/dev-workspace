import assert from 'node:assert/strict';
import test from 'node:test';
import {mkdtempSync, mkdirSync, rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {sdk, ai} from '../../test/pi_runtime/package-api.mjs';
import {createToolPolicy, toolPolicyDigest, validateToolPolicy} from './tool-policy.mjs';

const ceiling = ['bash', 'edit', 'find', 'grep', 'ls', 'publish_plan', 'read', 'request_user_input_async', 'write'];
const plan = ['find', 'grep', 'ls', 'publish_plan', 'read', 'request_user_input_async'];
function target(Mode = 'plan', Revision = 1, Active = plan) {
  const t = {Mode, Revision, Active: [...Active], RegisteredCeiling: [...ceiling]}; t.Digest = toolPolicyDigest(t); return t;
}
test('Go-compatible policy bytes exclude only the independent revision', () => {
  const t = target(); assert.equal(t.Digest, '4cd0c8d971732e90864c75f9f313f184c39d331efb6167c94e4bab85e81278d8');
  assert.equal(toolPolicyDigest({...t, Revision: 99}), t.Digest);
  for (const bad of [{...t, Active: null}, {...t, Active: ['read', 'read']}, target('plan', 1, ['bash']), {...t, Digest: '0'.repeat(64)}]) assert.throws(() => validateToolPolicy(bad));
});
test('pinned SDK applies exact sets, blocks nested and late tool calls with the real hook', async () => {
  const root = mkdtempSync(join(tmpdir(), 'pi-b2-policy-')); for (const d of ['repo', 'agent']) mkdirSync(join(root, d));
  const runtime = await sdk.ModelRuntime.create({authPath: join(root, 'auth.json'), modelsPath: null});
  const fake = ai.fauxProvider({provider: 'b2-fake', models: [{id: 'model'}]}); runtime.registerNativeProvider(fake.provider); await runtime.refresh({allowNetwork: false});
  const policy = createToolPolicy(target()); let api, mutations = 0, nested;
  const tools = ceiling.map(name => ({name, label: name, description: name, exposure: 'direct',
    parameters: {type: 'object', properties: {}, additionalProperties: false},
    execute: async (_id, _args, _signal, _update, ctx) => {
      if (name === 'publish_plan') nested = await ctx.executeTool('bash', {});
      if (['bash', 'edit', 'write'].includes(name)) mutations++;
      return {content: [{type: 'text', text: 'fixture result'}]};
    }}));
  const settings = sdk.SettingsManager.inMemory({retry: {enabled: false}});
  const loader = new sdk.DefaultResourceLoader({cwd: join(root, 'repo'), agentDir: join(root, 'agent'), settingsManager: settings,
    noExtensions: true, noSkills: true, noThemes: true, noPromptTemplates: true,
    extensionFactories: [policy.extensionFactory, a => {api = a;}]});
  let session;
  try {
    await loader.reload();
    ({session} = await sdk.createAgentSession({cwd: join(root, 'repo'), agentDir: join(root, 'agent'),
      modelRuntime: runtime, model: runtime.getModel('b2-fake', 'model'), thinkingLevel: 'off',
      sessionManager: sdk.SessionManager.inMemory(), settingsManager: settings, resourceLoader: loader, tools: ceiling, customTools: tools}));
    await session.bindExtensions({});
    assert.equal(policy.inspect(session, 'generation').Applied, false);
    fake.setResponses([ai.fauxAssistantMessage('must not dispatch')]);
    await session.prompt('unapplied policy fixture', {expandPromptTemplates: false});
    assert.equal(fake.state.callCount, 0, 'unapplied guard aborts before the provider');
    const applied = policy.apply(session, target(), 'generation');
    assert.equal(applied.Outcome, 'applied', JSON.stringify(applied.Proof));
    assert.deepEqual(policy.inspect(session, 'generation').Callable, plan);
    fake.setResponses([ai.fauxAssistantMessage([ai.fauxToolCall('publish_plan', {})], {stopReason: 'toolUse'}), ai.fauxAssistantMessage('done')]);
    await session.prompt('nested refusal fixture', {expandPromptTemplates: false});
    assert.equal(nested.isError, true); assert.equal(mutations, 0);
    const defaultNames = ceiling.filter(name => name !== 'publish_plan');
    const full = target('default', 2, defaultNames);
    assert.equal(policy.apply(session, target('default', 4, defaultNames), 'generation').Outcome, 'rejected');
    assert.equal(policy.apply(session, full, 'generation').Outcome, 'applied');
    assert.equal(policy.apply(session, full, 'generation').Outcome, 'applied');
    assert.deepEqual(policy.inspect(session, 'generation').Callable, defaultNames);
    fake.setResponses([context => {
      assert(context.messages.some(m => m.role === 'system' && JSON.stringify(m).includes('Default mode:')));
      return ai.fauxAssistantMessage([ai.fauxToolCall('bash', {})], {stopReason: 'toolUse'});
    }, ai.fauxAssistantMessage('done')]);
    await session.prompt('default fixture', {expandPromptTemplates: false});
    assert.equal(mutations, 1, JSON.stringify(session.messages.slice(-3)));
    assert.equal(policy.apply(session, target('plan', 3), 'generation').Outcome, 'applied');
    assert.deepEqual(policy.inspect(session, 'generation').Callable, plan);
    // Alter the real SDK registry after preflight. Even an otherwise allowed call
    // must be refused by the installed tool_call hook, not merely DTO checks.
    fake.setResponses([() => {
      api.registerTool({name: 'late_mcp', label: 'late', description: 'late', exposure: 'deferred', parameters: {type: 'object', properties: {}},
        execute: async () => {mutations++; return {content: []};}});
      return ai.fauxAssistantMessage([ai.fauxToolCall('read', {})], {stopReason: 'toolUse'});
    }, ai.fauxAssistantMessage('done')]);
    await session.prompt('late registration fixture', {expandPromptTemplates: false});
    assert.equal(mutations, 1);
    const results = session.messages.filter(m => m.role === 'toolResult');
    assert(results.some(m => m.isError && JSON.stringify(m.content).includes('outside the applied host policy')));
    assert.equal(policy.inspect(session, 'generation').Applied, false);
  } finally { if (session) {await session.abort(); await session.dispose();} await runtime.settleCredentialWrites(); rmSync(root, {recursive: true}); }
});

test('native apply preserves fixed ceilings and unknown partial mutation cannot be replayed', () => {
  const t = target(); const controller = createToolPolicy(t), handlers = new Map();
  controller.extensionFactory({on: (name, fn) => handlers.set(name, fn)});
  let active = [...plan], changes = 0;
  const session = {isStreaming: false, getActiveToolNames: () => active,
    getCallableToolNames: () => active, getAllTools: () => ceiling.map(name => ({name, exposure: 'direct'})),
    setActiveToolsByName: names => {changes++; active = names; if (changes === 2) throw Error('lost after mutation');}};
  assert.equal(controller.apply(session, t, 'g').Outcome, 'applied');
  const narrow = {...target('default', 2, ['read']), RegisteredCeiling: ['read']}; narrow.Digest = toolPolicyDigest(narrow);
  assert.equal(controller.apply(session, narrow, 'g').Outcome, 'rejected'); assert.equal(changes, 1);
  const broad = {...target('default', 2, ['read']), RegisteredCeiling: [...ceiling, 'zz_extra']}; broad.Digest = toolPolicyDigest(broad);
  assert.equal(controller.apply(session, broad, 'g').Outcome, 'rejected'); assert.equal(changes, 1);
  const next = target('default', 2, ceiling.filter(name => name !== 'publish_plan'));
  assert.equal(controller.apply(session, next, 'g').Outcome, 'unknown');
  assert.equal(controller.inspect(session, 'g').Applied, false);
  assert.equal(controller.apply(session, next, 'g').Outcome, 'rejected'); assert.equal(changes, 2);
  const max = target('plan', Number.MAX_SAFE_INTEGER), last = createToolPolicy(max);
  last.extensionFactory({on() {}}); changes = 0;
  assert.equal(last.apply(session, max, 'g').Outcome, 'applied');
  assert.equal(last.apply(session, max, 'g').Outcome, 'applied'); assert.equal(changes, 1);
  assert.throws(() => validateToolPolicy(target('plan', Number.MAX_SAFE_INTEGER + 1)));
  assert.throws(() => validateToolPolicy(target('plan', 1.5)));
});
