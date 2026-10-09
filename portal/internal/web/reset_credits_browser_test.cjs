const assert = require('node:assert/strict');
const fs = require('node:fs');
const {test} = require('node:test');
const ready = import('data:text/javascript;base64,' + Buffer.from(fs.readFileSync(__dirname + '/static/reset-credits.js')).toString('base64'));
const key = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const withLock = operation => operation();
const scope = 'b'.repeat(64);

function store() {
  let attempt = null;
  return {available: () => true, load: () => attempt ? [{...attempt}] : [],
    store: value => {attempt = {...value}; return true;}, remove: () => {attempt = null; return true;}};
}

test('opening/reset cancellation sends no request and a saved retry spends only one mocked reset', async () => {
  const {createResetCreditAction} = await ready;
  const memory = store(), calls = [], keys = new Set();
  let confirmed = false, loseResponse = true, remaining = 3, refreshed = 0;
  const options = {store: memory, withLock, confirm: () => confirmed, randomUUID: () => key,
    refresh: async () => {refreshed++;}, request: async (path, request) => {
      assert.equal(path, '/api/codex-limits/reset');
      const body = JSON.parse(request.body); calls.push(body);
      const duplicate = keys.has(body.idempotencyKey);
      if (!duplicate) {keys.add(body.idempotencyKey); remaining--;}
      if (loseResponse) {loseResponse = false; throw Error('Lost response');}
      return {outcome: duplicate ? 'alreadyRedeemed' : 'reset'};
    }};
  let action = createResetCreditAction(options);
  assert.equal(calls.length, 0);
  await action.use({accountScope: scope, creditId: 'reset-1'});
  assert.equal(calls.length, 0); assert.equal(memory.load().length, 0);
  confirmed = true;
  await assert.rejects(action.use({accountScope: scope, creditId: 'reset-1'}), /Lost response/);
  assert.equal(remaining, 2); assert.equal(memory.load()[0].id, key);
  action = createResetCreditAction(options);
  assert.equal(calls.length, 1);
  await assert.rejects(action.use({accountScope: scope, creditId: 'reset-2'}), /Resolve/);
  assert.equal(await action.retry(), 'alreadyRedeemed');
  assert.deepEqual(calls[1], calls[0]); assert.equal(remaining, 2);
  assert.equal(refreshed, 1); assert.equal(memory.load().length, 0);
});

test('duplicate clicks share the in-flight attempt and count-only redemption omits a selected ID', async () => {
  const {createResetCreditAction} = await ready;
  let finish, calls = 0;
  const action = createResetCreditAction({store: store(), withLock, confirm: () => true, randomUUID: () => key,
    refresh: async () => {}, request: async (_path, request) => {
      calls++; assert.equal(JSON.parse(request.body).creditId, '');
      return new Promise(resolve => {finish = resolve;});
    }});
  const pending = action.use({accountScope: scope});
  await action.use({accountScope: scope}); assert.equal(calls, 1);
  finish({outcome:'nothingToReset'}); assert.equal(await pending, 'nothingToReset');
  assert.equal(action.state().pending, null);
});

test('failed storage and unknown responses retain safe behavior', async () => {
  const {createResetCreditAction} = await ready;
  let calls = 0;
  const memory = store(); memory.store = () => false;
  const options = {store: memory, withLock, confirm: () => true, randomUUID: () => key,
    refresh: async () => {}, request: async () => {calls++; return {outcome:'unexpected'};}};
  await assert.rejects(createResetCreditAction(options).use({accountScope:scope}), /not sent/);
  assert.equal(calls, 0);
  const action = createResetCreditAction({...options, store:store()});
  await assert.rejects(action.use({accountScope:scope}), /could not be confirmed/);
  assert.equal(action.state().pending.id, key);
});


test('two stale tabs cannot overwrite or clear an unresolved shared attempt', async () => {
  const {createResetCreditAction} = await ready;
  const memory = store(), calls = [];
  let locked = false, finish;
  const exclusive = async operation => {
    if (locked) throw Error('Another tab owns the lock');
    locked = true;
    try { return await operation(); } finally { locked = false; }
  };
  const options = {store: memory, withLock: exclusive, confirm: () => true, randomUUID: () => key,
    refresh: async () => {}, request: async (_path, request) => {
      calls.push(JSON.parse(request.body));
      return new Promise((resolve, reject) => {finish = {resolve, reject};});
    }};
  const first = createResetCreditAction(options);
  const stale = createResetCreditAction({...options, randomUUID: () => 'cccccccc-cccc-4ccc-8ccc-cccccccccccc'});
  const sending = first.use({accountScope:scope, creditId:'reset-1'});
  await assert.rejects(stale.use({accountScope:scope, creditId:'reset-2'}), /lock/);
  assert.equal(memory.load()[0].id, key);
  finish.reject(Error('Lost acknowledgement'));
  await assert.rejects(sending, /Lost/);
  await assert.rejects(stale.use({accountScope:scope, creditId:'reset-2'}), /Resolve/);
  assert.equal(calls.length, 1);
  assert.equal(stale.state().pending.id, key);
  const retrying = stale.retry();
  await assert.rejects(first.use({accountScope:scope}), /lock/);
  finish.resolve({outcome:'alreadyRedeemed'});
  assert.equal(await retrying, 'alreadyRedeemed');
  assert.deepEqual(calls[1], calls[0]);
  const next = stale.use({accountScope:scope, creditId:'reset-2'});
  first.reload(); assert.equal(first.state().pending.id, 'cccccccc-cccc-4ccc-8ccc-cccccccccccc');
  finish.reject(Error('Lost second acknowledgement'));
  await assert.rejects(next, /Lost second/);
  assert.equal(memory.load()[0].creditId, 'reset-2');
});

test('missing tab coordination fails closed without sending a reset', async () => {
  const {createResetCreditAction} = await ready;
  let calls = 0;
  const action = createResetCreditAction({store:store(), confirm:() => true, randomUUID:() => key,
    refresh:async () => {}, request:async () => {calls++;}});
  assert.equal(action.state().unavailable, true);
  await action.use({accountScope:scope}); await action.retry();
  assert.equal(calls, 0);
});
