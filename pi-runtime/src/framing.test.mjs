import assert from 'node:assert/strict';
import test from 'node:test';
import { jsonBytes, parseRecord } from './framing.mjs';

test('private frames reject duplicate/unknown/trailing/surrogate/oversized JSON', () => {
  const keys = ['schema', 'body'];
  const text = '{"schema":1,"body":{"text":"<é>"}}';
  assert.equal(jsonBytes(parseRecord(text, keys)), text);
  for (const bad of ['{"schema":1,"schema":1}', '{"schema":1,"body":{"x":1,"x":2}}', '{"schema":1,"extra":0}', `${text}{}`, '{"body":"\\ud800"}', '[]', 'null', '{"body":[1,]}']) {
    assert.throws(() => parseRecord(bad, keys));
  }
  assert.throws(() => parseRecord(text, keys, 1));
  assert.throws(() => jsonBytes({ text: '\ud800' }));
  assert.throws(() => jsonBytes({ value: NaN }));
  assert.throws(() => jsonBytes({ text: 'x'.repeat(65536) }));
});
