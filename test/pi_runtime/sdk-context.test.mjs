import assert from 'node:assert/strict';
import test from 'node:test';
import {mkdtempSync, mkdirSync, rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {sdk, ai} from './package-api.mjs';

async function fixture({extensions = true} = {}) {
  const root = mkdtempSync(join(tmpdir(), 'pi-sdk-context-'));
  for (const dir of ['repo', 'agent']) mkdirSync(join(root, dir));
  const runtime = await sdk.ModelRuntime.create({authPath: join(root, 'auth.json'), modelsPath: null});
  const fake = ai.fauxProvider({provider: 'context-fake', models: [{id: 'model'}]});
  runtime.registerNativeProvider(fake.provider);
  await runtime.refresh({allowNetwork: false});
  const settings = sdk.SettingsManager.inMemory({retry: {enabled: false}, cacheWarming: 'off'});
  let api, session;
  const loader = new sdk.DefaultResourceLoader({cwd: join(root, 'repo'), agentDir: join(root, 'agent'),
    settingsManager: settings, noExtensions: true, noSkills: true, noThemes: true,
    noPromptTemplates: true, noContextFiles: true,
    extensionFactories: extensions ? [value => {api = value; api.on('session_start', () => {});}] : []});
  try {
    await loader.reload();
    ({session} = await sdk.createAgentSession({cwd: join(root, 'repo'), agentDir: join(root, 'agent'),
      modelRuntime: runtime, model: runtime.getModel('context-fake', 'model'), thinkingLevel: 'off',
      sessionManager: sdk.SessionManager.inMemory(), settingsManager: settings, resourceLoader: loader, tools: []}));
    const errors = [];
    await session.bindExtensions({onError: error => errors.push(error)});
    const runner = session.extensionRunner, emit = runner.emitContext.bind(runner);
    let calls = 0;
    // Observe the real SDK dispatch boundary without replacing its implementation.
    runner.emitContext = (...args) => {calls++; return emit(...args);};
    return {api, session, runner, fake, errors, calls: () => calls,
      async close() {await session.abort(); session.dispose(); await runtime.settleCredentialWrites(); rmSync(root, {recursive: true});}};
  } catch (error) {
    session?.dispose(); await runtime.settleCredentialWrites(); rmSync(root, {recursive: true}); throw error;
  }
}

const messages = () => [
  {role: 'system', content: 'original prompt', timestamp: 1,
    toolsAdded: [{name: 'owned_tool', description: 'owned', parameters: {type: 'object', properties: {}}}]},
  {role: 'user', content: [{type: 'text', text: 'nested original'}], timestamp: 2},
];

test('selected SDK avoids context dispatch with no extensions or only unrelated handlers', async t => {
  // The public session always constructs an ExtensionRunner, even with no extensions.
  for (const extensions of [false, true]) await t.test(`extensions=${extensions}`, async () => {
    const f = await fixture({extensions});
    try {
      const input = messages(), before = structuredClone(input);
      const history = structuredClone(f.session.sessionManager.getEntries());
      assert.equal(f.runner.hasHandlers('context'), false);
      assert.equal(f.runner.hasHandlers('context_with_system'), false);
      assert.equal(await f.session.agent.transformContext(input), input);
      assert.equal(f.calls(), 0);
      const direct = await f.runner.emitContext(input);
      assert.notEqual(direct, input);
      assert.notEqual(direct[1].content[0], input[1].content[0]);
      direct[1].content[0].text = 'direct caller mutation';
      assert.deepEqual(input, before, 'direct emitContext still deep-copies without listeners');
      assert.deepEqual(f.session.sessionManager.getEntries(), history);
    } finally {await f.close();}
  });
});

test('either context phase retains deep-copy isolation through the real agent transform', async t => {
  for (const phase of ['context', 'context_with_system']) await t.test(phase, async () => {
    const f = await fixture();
    try {
      f.api.on(phase, event => {
        const user = event.messages.find(message => message.role === 'user');
        user.content[0].text = phase;
      });
      const input = messages(), before = structuredClone(input);
      const output = await f.session.agent.transformContext(input);
      assert.equal(f.calls(), 1);
      assert.notEqual(output, input);
      assert.equal(output.find(message => message.role === 'user').content[0].text, phase);
      assert.deepEqual(input, before);
    } finally {await f.close();}
  });
});

test('both SDK context phases preserve order, system/tools restoration and error reporting', async () => {
  const f = await fixture();
  try {
    const order = [];
    f.api.on('context', event => {
      order.push('context');
      assert(event.messages.every(message => message.role !== 'system'));
      event.messages[0].content[0].text = 'conversation phase';
      return {messages: event.messages.map(message => ({...message}))};
    });
    f.api.on('context', () => {order.push('context error'); throw new Error('context fixture error');});
    f.api.on('context_with_system', event => {
      order.push('context_with_system');
      assert.equal(event.messages[0].content, 'original prompt');
      assert.deepEqual(event.messages[0].toolsAdded, messages()[0].toolsAdded);
      assert.equal(event.messages[1].content[0].text, 'conversation phase');
      event.messages[0].content = 'full transcript phase';
    });
    f.api.on('context_with_system', () => {order.push('system error'); throw new Error('system fixture error');});
    const input = messages(), before = structuredClone(input);
    const output = await f.session.agent.transformContext(input);
    assert.deepEqual(order, ['context', 'context error', 'context_with_system', 'system error']);
    assert.deepEqual(f.errors.map(error => [error.event, error.error]),
      [['context', 'context fixture error'], ['context_with_system', 'system fixture error']]);
    assert.equal(output[0].content, 'full transcript phase');
    assert.deepEqual(output[0].toolsAdded, before[0].toolsAdded);
    assert.deepEqual(input, before);
  } finally {await f.close();}
});

test('SDK context registration and removal are observed on every request', async () => {
  const f = await fixture();
  try {
    const input = messages(), before = structuredClone(input);
    assert.equal(await f.session.agent.transformContext(input), input);
    const remove = f.api.on('context_with_system', event => {event.messages[1].content[0].text = 'registered later';});
    assert.equal((await f.session.agent.transformContext(input))[1].content[0].text, 'registered later');
    assert.equal(f.calls(), 1);
    remove();
    assert.equal(await f.session.agent.transformContext(input), input);
    assert.equal(f.calls(), 1, 'no cached handler decision survives removal');
    assert.deepEqual(input, before);
  } finally {await f.close();}
});

test('SDK no-listener path retains actual forced prompt and tool projection', async () => {
  const f = await fixture();
  try {
    f.api.registerTool({name: 'context_owned_tool', label: 'owned', description: 'fixture', exposure: 'direct',
      parameters: {type: 'object', properties: {}}, execute: async () => ({content: []})});
    f.api.on('before_agent_start', () => ({systemPrompt: 'FORCED_CONTEXT_FIXTURE'}));
    let requests = 0;
    f.fake.setResponses([context => {
      requests++;
      assert.equal(context.messages[0].role, 'system');
      assert.equal(context.messages[0].content, 'FORCED_CONTEXT_FIXTURE');
      assert(context.messages[0].toolsAdded.some(tool => tool.name === 'context_owned_tool'));
      return ai.fauxAssistantMessage('context projection complete');
    }]);
    await f.session.prompt('one literal input', {expandPromptTemplates: false});
    assert.equal(requests, 1);
    assert.equal(f.calls(), 0);
    assert.equal(f.session.sessionManager.getEntries().filter(entry => entry.type === 'message' && entry.message.role === 'user').length, 1);
    assert(f.session.sessionManager.getEntries().every(entry => entry.message?.content !== 'FORCED_CONTEXT_FIXTURE'));
  } finally {await f.close();}
});
