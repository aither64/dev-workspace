import assert from 'node:assert/strict';
import test from 'node:test';
import * as fs from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { lp32, selectionToken, linearEntries, inspectOrigin, inspectHistoricalOrigin } from './lineage.mjs';

export const fixtureSubject = { Kind: 'conversation', Conversation: {
  Conversation: { Session: { Workspace: '/espace/é', Slug: '2026-11-11-fixture', Incarnation: 'a'.repeat(64) }, Backend: 'pi',
    Member: 'lead', Incarnation: 'b'.repeat(64), NativeID: '11111111-1111-4111-8111-111111111111' }, Generation: 'c'.repeat(64) } };

test('linear selection uses exact UTF-8 tuple and stable domain-separated identity', () => {
  assert.equal(lp32(['a', 'é', '']).toString('hex'), '00000003000000016100000002c3a900000000');
  assert.throws(() => lp32(['\ud800']));
  const token = selectionToken(fixtureSubject, 'root');
  assert.equal(token, 'linear-v1:285c02f4e5a7ed8e2396883d73ae191eaa9beef6e2b7218f8b6d85e374863d2c');
  assert.equal(selectionToken({ ...fixtureSubject, Conversation: { ...fixtureSubject.Conversation, Generation: 'd'.repeat(64) } }, 'root'), token);
  assert.notEqual(selectionToken({ Kind: 'utility', Utility: { Session: fixtureSubject.Conversation.Conversation.Session,
    MonitorOperationID: '22222222-2222-4222-8222-222222222222', Incarnation: 'b'.repeat(64), NativeID: fixtureSubject.Conversation.Conversation.NativeID,
    Generation: 'c'.repeat(64) } }, 'root'), token);
});

test('historical origin copies bounded native arguments without active input or writes', () => {
  const root = fs.mkdtempSync(join(tmpdir(), 'pi-origin-'));
  const file = join(root, 'owned-fake-history');
  fs.writeFileSync(file, 'fixture marker');
  const entries = [
    { id: 'root', parentId: null, type: 'model_change' },
    { id: 'assistant', parentId: 'root', type: 'message', message: { role: 'assistant', content: [
      { type: 'toolCall', id: 'tool-one', name: 'publish_plan', arguments: { text: 'é' } },
    ] } },
  ];
  const view = (values) => ({ getSessionId: () => fixtureSubject.Conversation.Conversation.NativeID,
    getCwd: () => root, getSessionFile: () => file, getEntries: () => values,
    getBranch: () => values, getLeafId: () => values.at(-1).id });
  entries[1].message.details = undefined;
  const live = view(entries); let persisted = JSON.parse(JSON.stringify(entries)); let opens = 0;
  const sdk = { SessionManager: { open: (path) => { assert.equal(path, file); opens++; return view(persisted); } } };
  const ref = { AnchorEntryID: 'assistant', SelectionToken: selectionToken(fixtureSubject, 'root'), NativeToolCallID: 'tool-one', Tool: 'publish_plan' };
  try {
    const observation = inspectHistoricalOrigin(sdk, live, fixtureSubject, ref);
    assert.deepEqual(observation, { RootEntryID: 'root', Arguments: { text: 'é' } });
    observation.Arguments.text = 'caller changed copy';
    assert.equal(entries[1].message.content[0].arguments.text, 'é');
    assert.equal(fs.readFileSync(file, 'utf8'), 'fixture marker');
    for (const changed of [{ ...ref, AnchorEntryID: 'other' }, { ...ref, NativeToolCallID: 'missing' },
      { ...ref, Tool: 'other' }, { ...ref, SelectionToken: `linear-v1:${'d'.repeat(64)}` }]) {
      assert.throws(() => inspectHistoricalOrigin(sdk, live, fixtureSubject, changed));
    }
    persisted[1].message.content[0].arguments.text = 'replacement';
    assert.throws(() => inspectHistoricalOrigin(sdk, live, fixtureSubject, ref));
    persisted = structuredClone(entries);
    entries[1].message.content.push(structuredClone(entries[1].message.content[0]));
    persisted = structuredClone(entries);
    assert.throws(() => inspectHistoricalOrigin(sdk, live, fixtureSubject, ref));
    entries[1].message.content.pop();
    for (const args of [null, [], { text: 'x'.repeat(65536) }, { text: '\ud800' }]) {
      entries[1].message.content[0].arguments = args; persisted = structuredClone(entries);
      assert.throws(() => inspectHistoricalOrigin(sdk, live, fixtureSubject, ref));
    }
    assert.ok(opens > 0);
    fs.unlinkSync(file);
    const before = opens;
    assert.throws(() => inspectHistoricalOrigin(sdk, live, fixtureSubject, ref));
    assert.equal(opens, before, 'missing file must not be opened/created');
    assert.equal(fs.existsSync(file), false);
  } finally { fs.rmSync(root, { recursive: true }); }
});

test('origin refuses divergent, replaced, missing and duplicate tool history', () => {
  const entries = [
    { id: 'root', parentId: null, type: 'model_change' },
    { id: 'input', parentId: 'root', type: 'message', message: { role: 'user', content: [{ type: 'text', text: 'original' }] } },
    { id: 'assistant', parentId: 'input', type: 'message', message: { role: 'assistant', content: [
      { type: 'toolCall', id: 'tool-one', name: 'publish_plan', arguments: { text: 'é' } },
      { type: 'toolCall', id: 'tool-two', name: 'publish_plan', arguments: { text: 'second' } },
    ] } },
  ];
  const manager = (values, branch = values) => ({ getEntries: () => values, getBranch: () => branch,
    getLeafId: () => branch.at(-1)?.id, getSessionId: () => fixtureSubject.Conversation.Conversation.NativeID });
  assert.deepEqual(linearEntries(manager(entries)), entries);
  for (const values of [[], [...entries, { ...entries[2] }], [...entries, { id: 'bad', parentId: 'root', type: 'message' }],
    [...entries, { id: 'edit', parentId: 'assistant', type: 'context_edit' }], [...entries, { id: 'summary', parentId: 'assistant', type: 'branch_summary' }]]) {
    assert.throws(() => linearEntries(manager(values)));
  }
  assert.throws(() => linearEntries(manager(entries, entries.slice(0, 2))));
  const input = { witness: { sessionId: fixtureSubject.Conversation.Conversation.NativeID, entryId: 'input', parentId: 'root', effectiveMessage: 'original' } };
  // These fail before any native-file access or host effect.
  for (const [id, name, args] of [['missing', 'publish_plan', { text: 'é' }], ['tool-one', 'wrong', { text: 'é' }], ['tool-one', 'publish_plan', { text: 'changed' }]]) {
    assert.throws(() => inspectOrigin({}, manager(entries), fixtureSubject, input, id, name, args));
  }
});
