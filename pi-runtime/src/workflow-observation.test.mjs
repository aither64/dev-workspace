import assert from 'node:assert/strict';
import test from 'node:test';
import {WorkerSession} from './session.mjs';
import {createToolPolicy, toolPolicyDigest} from './tool-policy.mjs';

test('workflow observation synchronously includes exact policy and all native activity', () => {
  const target = {Mode: 'plan', Revision: 1, Active: ['read'], RegisteredCeiling: ['read']};
  target.Digest = toolPolicyDigest(target);
  let worker, active = ['read'];
  const policy = createToolPolicy(target, {changed: () => worker?.notifyWorkflow()});
  const handlers = new Map();
  policy.extensionFactory({on: (name, fn) => handlers.set(name, fn)});
  const session = {subscribe() {}, isStreaming: false, model: {id: 'model', provider: 'provider'}, thinkingLevel: 'off',
    getActiveToolNames: () => active, getCallableToolNames: () => active,
    getAllTools: () => [{name: 'read'}], setActiveToolsByName: names => {active = names;}};
  worker = new WorkerSession({}, session, {getSessionId: () => 'native'});
  worker.toolPolicy = policy; worker.generation = 'g'; worker.hostTools = [];
  assert.equal(policy.apply(session, target, 'g').Outcome, 'applied');
  const changes = [];
  worker.listeners.add(event => changes.push(event.type));
  const initial = worker.workflowObservation();
  assert.deepEqual(initial, {nativeId: 'native', model: 'model', provider: 'provider', thinking: 'off',
    proof: policy.inspect(session, 'g'), running: false, activeCalls: 0,
    pending: false, streaming: false, hostCalls: 0, held: false, hostTools: []});
  handlers.get('before_agent_start')({systemPrompt: 'trusted'}, {abort() {throw Error('unexpected abort');}});
  session.isStreaming = true; worker.pending = {}; worker.hostBridge = {pending: new Map([['callback', {}]])};
  handlers.get('tool_execution_start')({toolCallId: 'exact-tool'});
  const busy = worker.workflowObservation();
  assert.equal(busy.running, true); assert.equal(busy.activeCalls, 1);
  assert.equal(busy.pending, true); assert.equal(busy.streaming, true); assert.equal(busy.hostCalls, 1);
  assert.equal(busy.proof.Applied, true, 'enforcement proof is independent of idle');
  handlers.get('tool_execution_end')({toolCallId: 'exact-tool'});
  handlers.get('agent_settled')(); worker.pending = null; session.isStreaming = false; worker.hostBridge.pending.clear();
  const settled = worker.workflowObservation();
  assert.equal(settled.running, false); assert.equal(settled.activeCalls, 0); assert.equal(settled.hostCalls, 0);
  assert.deepEqual(changes, ['workflow_changed', 'workflow_changed', 'workflow_changed', 'workflow_changed']);
  settled.proof.Policy.Active.push('write'); settled.hostTools.push('publish_plan');
  assert.deepEqual(worker.workflowObservation().proof.Policy.Active, ['read']);
  assert.deepEqual(worker.workflowObservation().hostTools, []);
});

test('workflow observation refuses missing enforcement and includes held state', () => {
  const session = {subscribe() {}, isStreaming: false};
  const worker = new WorkerSession({}, session, {getSessionId: () => 'native'});
  assert.throws(() => worker.workflowObservation(), /unavailable/);
  worker.toolPolicy = {workflow: () => ({proof: {}, running: false, activeCalls: 0, held: true})};
  worker.hostTools = [];
  assert.equal(worker.workflowObservation().held, true);
});
