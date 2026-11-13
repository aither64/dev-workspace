import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createReportingTools} from './reporting.mjs';

const args = {message_id: 'a'.repeat(32), message: 'Result ready.'};

test('report is explicit and only sends stable payload through injected callback', async () => {
  const calls = [];
  const [report, peer] = createReportingTools(async (...call) => {
    calls.push(call);
    return {disposition: 'attempted', outcome: 'unknown'};
  });
  assert.equal(calls.length, 0);
  const result = await report.execute('native-tool-call', args);
  assert.equal(result.details.outcome, 'unknown');
  assert.deepEqual(calls[0].slice(0, 2), ['report_to_lead', args]);
  await peer.execute('call2', {...args, to: 'reviewer0'});
  assert.equal(calls.length, 2);
});

test('identity, unknown keys, invalid IDs and unavailable callbacks have no effect', async () => {
  let calls = 0;
  const [report] = createReportingTools(async () => {calls++; return {disposition: 'held'};});
  for (const field of ['from', 'workspace', 'slug', 'generation', 'native_id', 'caller']) {
    await assert.rejects(report.execute('call', {...args, [field]: 'forged'}), /Invalid/);
  }
  for (const bad of [{...args, message_id: 'new'}, {...args, message: ''}, {...args, message: '\0'}, {...args, message: '\ud800'}]) {
    await assert.rejects(report.execute('call', bad), /Invalid/);
  }
  assert.equal(calls, 0);
  const [unavailable] = createReportingTools();
  await assert.rejects(unavailable.execute('call', args), /unavailable/);
});

test('held/refused outcomes do not trigger retries and missing evidence is unknown', async () => {
  let calls = 0;
  const [held] = createReportingTools(async () => {calls++; return {disposition: 'held'};});
  assert.equal((await held.execute('call', args)).details.disposition, 'held');
  assert.equal(calls, 1);
  const [missing] = createReportingTools(async () => ({accepted: true}));
  await assert.rejects(missing.execute('call', args), /unknown/);
  const [contradictory] = createReportingTools(async () => ({disposition: 'held', outcome: 'accepted'}));
  await assert.rejects(contradictory.execute('call', args), /unknown/);
  const [unproved] = createReportingTools(async () => ({disposition: 'attempted'}));
  await assert.rejects(unproved.execute('call', args), /unknown/);
  const controller = new AbortController(); controller.abort();
  await assert.rejects(held.execute('call', args, controller.signal));
  assert.equal(calls, 1);
});
