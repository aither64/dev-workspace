import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createQuestionTools} from './questions.mjs';
import {createPlanTools} from './plan.mjs';
test('question factory returns durable pending ACK without waiting for an answer', async () => {
  let calls = 0;
  const [tool] = createQuestionTools({boundCaller: {}, host: {publishQuestion: async (id, args) => {
    calls++; assert.equal(id, 'trusted-tool-call'); assert.equal(args.questions[0].title, 'Which?');
    return {id: '00000000-0000-4000-8000-000000000001', revision: 1, status: 'pending'};
  }}});
  const out = await tool.execute('trusted-tool-call', {questions: [{title: 'Which?'}]});
  assert.equal(out.details.status, 'pending'); assert.equal(calls, 1);
});
test('missing callbacks and failed publication never become successful stubs', async () => {
  const [tool] = createQuestionTools({boundCaller: {}, host: {}});
  await assert.rejects(tool.execute('call', {}), /unavailable/);
  const [plan] = createPlanTools({boundCaller: {}, host: {publishPlan: async () => null}});
  await assert.rejects(plan.execute('call', {}), /not acknowledged/);
});
test('tool factories forward SDK cancellation and never publish an already aborted call', async () => {
  const controller = new AbortController(); let calls = 0;
  const [tool] = createQuestionTools({boundCaller: {}, host: {publishQuestion: async (_id, _params, signal) => {
    calls++; assert.equal(signal, controller.signal);
    return {id: '00000000-0000-4000-8000-000000000001', revision: 1, status: 'pending'};
  }}});
  await tool.execute('call', {}, controller.signal); controller.abort();
  await assert.rejects(tool.execute('other-call', {}, controller.signal)); assert.equal(calls, 1);
});
