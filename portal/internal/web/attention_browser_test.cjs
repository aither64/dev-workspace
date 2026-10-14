const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = name => fs.readFileSync(path.join(__dirname, 'static', name), 'utf8');
const identity = 'a'.repeat(64), id = 'b'.repeat(64);
const event = (revision = 1, kind = 'input') => ({version: 1, identity, id, revision, kind, slug: 'example', member: 'implementer0', route: '/example/?member=implementer0#codex', expiresAt: Date.now() + 60000});

test('title priority and exact persistent reservation count', () => {
  const context = {}; vm.runInNewContext(source('session-attention.js'), context);
  const {title, reservations} = context.workspaceSessionAttention;
  assert.equal(title('api-cleanup · Workspace', event(), [], 0), '? api-cleanup · Workspace');
  assert.equal(title('api-cleanup · Workspace', event(1, 'done'), [], 1), '✓ R api-cleanup · Workspace');
  assert.equal(title('api-cleanup · Workspace', event(), [], 2), '? R2 api-cleanup · Workspace');
  assert.equal(title('api-cleanup · Workspace', event(), [id], 2), 'R2 api-cleanup · Workspace');
  assert.equal(reservations({reservationHolders: [{slug: 'example', observationIdentity: identity, machines: [
    {machine: 'one', state: 'deployed_review'}, {machine: 'one', state: 'recovery_hold'}, {machine: 'two', state: 'recovery_hold'}, {machine: 'three', state: 'requested'}]},
    {slug: 'example', observationIdentity: 'wrong', machines: [{machine: 'foreign', state: 'deployed_review'}]}]}, 'example', identity), 2);
  assert.throws(() => reservations({}, 'example', identity));
});

function workerHarness() {
  const listeners = {}, notifications = [], windows = [], records = new Map(), calls = [];
  let status = {fresh: true, enabled: true, identity, event: event(), revision: 1}, inaccessible = false;
  const context = {URL, AbortController, setTimeout, clearTimeout, Date,
    importScripts: value => assert.equal(value, '/static/session-attention-store.js'),
    workspaceAttentionStore: {update: async (key, change) => {
      const record = change(records.get(key) || {identity: key, seen: [], revision: 0, event: null}); records.set(key, record); return record;
    }},
    fetch: async (url, options) => { calls.push({url, options}); return {ok: !inaccessible, status: inaccessible ? 401 : 200, json: async () => status}; },
    self: {location: {origin: 'https://workspace.example'}, registration: {
      showNotification: async (title, options) => notifications.push({title, ...options}), getNotifications: async () => [],
    }, clients: {matchAll: async () => windows, openWindow: async url => calls.push({openWindow: url}), claim: async () => {}},
    addEventListener: (name, listener) => listeners[name] = listener},
  };
  vm.runInNewContext(source('notification-worker.js'), context);
  return {notifications, calls, records, context,
    status: value => status = value, inaccessible: () => inaccessible = true,
    push: async payload => { let pending; listeners.push({data: {json: () => payload}, waitUntil: promise => pending = promise}); await pending; },
    click: async (route, boundIdentity) => { let pending; listeners.notificationclick({notification: {data: {route, identity: boundIdentity}, close() {}}, waitUntil: promise => pending = promise}); await pending; },
  };
}

test('worker displays with zero tabs, coalesces duplicate and reordered delivery', async () => {
  const h = workerHarness();
  await h.push(event());
  assert.equal(h.notifications[0].body, 'implementer0: Your answer is needed');
  assert.equal(h.notifications[0].renotify, false);
  h.status({fresh: true, enabled: true, identity, event: {...event(2, 'done'), id: 'c'.repeat(64)}, revision: 2});
  await h.push({...event(2, 'done'), id: 'c'.repeat(64)});
  await h.push(event());
  assert.equal(h.notifications.at(-1).body, 'Session update available');
  assert.equal(h.notifications.at(-1).tag, h.notifications[0].tag);
  assert.equal(h.records.get(identity).revision, 2);
  assert.equal(h.notifications.length, 3, 'every delivered valid push is user-visible');
});

test('worker protects auth/mute/freshness races and safe click navigation', async () => {
  const h = workerHarness();
  h.status({fresh: true, enabled: false, identity, event: event(), revision: 1});
  await h.push(event()); assert.equal(h.notifications.at(-1).body, 'Session update available');
  h.inaccessible(); await h.push(event()); assert.equal(h.notifications.at(-1).title, 'Workspace update');
  await h.push({version: 99}); assert.equal(h.notifications.at(-1).title, 'Workspace update');
  await h.click('https://attacker.example/example/'); assert.equal(h.calls.filter(c => c.openWindow).length, 0);
  await h.click('//attacker.example/example/'); assert.equal(h.calls.filter(c => c.openWindow).length, 0);
  await h.click('/example/?member=implementer0#codex'); assert.equal(h.calls.at(-1).openWindow, 'https://workspace.example/example/?member=implementer0#codex');
  assert.ok(h.calls.filter(c => c.url).every(c => c.options.cache === 'no-store' && c.options.credentials === 'same-origin'));
  assert.equal(source('notification-worker.js').includes('caches.'), false, 'no authenticated cache');
});

test('worker handles bounded digit-containing member roles', async () => {
  const h = workerHarness(), member = 'gpt6reviewer0';
  const payload = {...event(), member, route: `/example/?member=${member}#codex`};
  h.status({fresh: true, enabled: true, identity, event: payload, revision: 1});
  await h.push(payload);
  assert.equal(h.notifications.at(-1).body, `${member}: Your answer is needed`);
  assert.equal(h.notifications.at(-1).data.route, payload.route);
  await h.click(payload.route, identity);
  const destination = new URL(h.calls.at(-1).openWindow);
  assert.equal(destination.searchParams.get('member'), member);
  assert.equal(destination.searchParams.get('attentionIdentity'), identity);
  await h.click(`/example/?member=${'a'.repeat(33)}0#codex`);
  assert.equal(h.calls.filter(call => call.openWindow).length, 1, 'oversized role is refused');
});

test('browser permission is only requested after explicit enable click', async () => {
  const controls = Object.fromEntries(['status', 'enable', 'disable', 'test'].map(name => [name, {disabled: false, addEventListener(type, callback) { this[type] = callback; }}]));
  let prompts = 0;
  const storage = new Map();
  const panel = {querySelector: selector => controls[selector.match(/notification-(\w+)/)[1]]};
  const context = {document: {getElementById: () => panel}, navigator: {serviceWorker: {getRegistration: async () => null}},
    Notification: {permission: 'default', requestPermission: async () => { prompts++; return 'denied'; }},
    isSecureContext: true, PushManager: function() {}, AbortController, setTimeout, clearTimeout,
    localStorage: {getItem: key => storage.get(key) || null},
    fetch: async () => ({ok: true, json: async () => ({available: true, registered: false})}),
  };
  vm.runInNewContext(source('browser-notifications.js'), context);
  for (let i = 0; i < 8; i++) await Promise.resolve();
  assert.equal(prompts, 0);
  await controls.enable.click();
  assert.equal(prompts, 1); assert.match(controls.status.textContent, /blocked/);
});

test('new-session muted choice survives exact request retry', async () => {
  const context = {URLSearchParams}; vm.runInNewContext(source('preparation.js'), context);
  const storage = new Map();
  const saved = {getItem: key => storage.get(key) || null, setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key)};
  const bodies = [];
  const fields = {name: 'example', goal: 'work', date: '2026-10-14', model: '', effort: '', team: '', catalogDigest: '', browserNotificationsEnabled: false};
  const draft = context.sessionPreparation.createRequestDraft({storage: saved, fields, randomUUID: () => '11111111-1111-4111-8111-111111111111', fetch: async (url, options = {}) => {
    if (options.body) bodies.push(options.body); throw new Error('network lost');
  }});
  await assert.rejects(draft.submit([], false));
  assert.equal(new URLSearchParams(bodies[0]).get('browserNotificationsEnabled'), 'false');
  assert.equal(draft.draft.browserNotificationsEnabled, false);
  const recovered = context.sessionPreparation.createRequestDraft({storage: saved, fields: {...fields, browserNotificationsEnabled: true}, fetch: async (url, options = {}) => {
    if (!options.body) return {status: 404}; bodies.push(options.body); throw new Error('network lost');
  }});
  await assert.rejects(recovered.submit([], false)); assert.equal(bodies[1], bodies[0]);
});

test('visible focus acknowledges only observed events and keeps stale reservations', async () => {
  const listeners = {}, timers = [], records = new Map(); let focus = false, failReservations = false;
  let current = event();
  const document = {body: {dataset: {session: 'example', selectedMember: 'implementer0', sessionObservation: identity, rootThreadId: 'thread-1'}}, title: 'example · Workspace', visibilityState: 'visible',
    hasFocus: () => focus, getElementById: () => null, addEventListener: (name, callback) => listeners[name] = callback};
  const context = {document, window: {addEventListener: (name, callback) => listeners[name] = callback},
    URL, AbortController, setTimeout: callback => { timers.push(callback); return timers.length; }, clearTimeout() {},
    navigator: {}, workspaceAttentionStore: {update: async (key, change = value => value) => {
      const next = change(records.get(key) || {identity: key, seen: [], revision: 0}); records.set(key, next); return next;
    }}, fetch: async url => {
      if (url.endsWith('/attention')) return {ok: true, json: async () => ({version: 1, identity, rootThread: 'thread-1', event: current, revision: current?.revision || 1, fresh: true, enabled: true})};
      if (failReservations) throw new Error('coordinator unavailable');
      return {ok: true, json: async () => ({reservationHolders: [{slug: 'example', observationIdentity: identity, machines: [{machine: 'one', state: 'recovery_hold'}]}]})};
    },
  };
  const settle = async () => { for (let i = 0; i < 30; i++) await Promise.resolve(); };
  vm.runInNewContext(source('session-attention.js'), context); await settle();
  assert.equal(document.title, '? R example · Workspace'); assert.deepEqual(records.get(identity).seen, []);
  focus = true; listeners.focus(); await settle(); assert.equal(document.title, 'R example · Workspace');
  assert.ok(records.get(identity).seen.includes(id));
  focus = false; current = {...event(2), id: 'c'.repeat(64)}; failReservations = true;
  timers.at(-1)(); await settle();
  assert.equal(document.title, '? R example · Workspace');
  assert.equal(records.get(identity).seen.includes(current.id), false, 'event after blur was not acknowledged');
  document.visibilityState = 'hidden'; focus = true; listeners.focus(); await settle();
  assert.equal(records.get(identity).seen.includes(current.id), false, 'focus alone cannot acknowledge a hidden page');
});

test('retained focused document refuses replaced directory or changed root', async () => {
  for (const next of [{identity: 'c'.repeat(64), rootThread: 'thread-1'}, {identity, rootThread: 'new-root'}]) {
    const listeners = {}, reads = [], records = new Map(), status = {}, control = {disabled: false, addEventListener(name, callback) {this[name] = callback;}};
    let presence = 0;
    const document = {body: {dataset: {session: 'example', sessionObservation: identity, rootThreadId: 'thread-1'}}, title: 'example · Workspace',
      visibilityState: 'visible', hasFocus: () => true, addEventListener: (name, action) => listeners[name] = action,
      getElementById: name => name === 'session-attention-status' ? status : control};
    const context = {document, window: {addEventListener: (name, action) => listeners[name] = action},
      navigator: {}, AbortController, setTimeout: () => 1, clearTimeout() {},
      workspaceAttentionStore: {update: async (key, change = value => value) => { const value = change({identity: key, seen: [], revision: 0}); records.set(key, value); return value; }},
      workspaceBrowserNotifications: {presence: async () => presence++},
      fetch: async (url, options = {}) => {reads.push({url, options}); return {ok: true, json: async () => ({version: 1, ...next, fresh: true, enabled: true, event: {...event(), identity: next.identity}, revision: 1})};},
    };
    vm.runInNewContext(source('session-attention.js'), context);
    for (let i = 0; i < 20; i++) await Promise.resolve();
    assert.equal(status.textContent, 'This session changed. Reload this page to continue.');
    assert.equal(control.disabled, true); assert.equal(records.size, 0); assert.equal(presence, 0);
    await control.change(); listeners.focus(); listeners.pageshow();
    for (let i = 0; i < 20; i++) await Promise.resolve();
    assert.equal(reads.length, 1, 'mismatch stops later reads/settings');
    assert.equal(records.size, 0, 'replacement was never acknowledged');
  }
});

const browserId = 'd'.repeat(64), browserKey = 'workspace-browser-notifications.v1', cleanupKey = `${browserKey}.cleanup`;
const endpoint = 'https://fcm.googleapis.com/private-endpoint?exact=value';
const endpointDigest = require('node:crypto').createHash('sha256').update(endpoint).digest('hex');
async function notificationHarness(state, storage = new Map()) {
  const controls = Object.fromEntries(['status', 'enable', 'disable', 'test'].map(name => [name, {disabled: false, addEventListener(type, callback) {this[type] = callback;}}]));
  const calls = [];
  const subscription = {endpoint, options: state.keyMismatch ? {applicationServerKey: new Uint8Array([1]).buffer} : {},
    toJSON: () => ({endpoint, keys: {p256dh: 'public', auth: 'auth'}, expirationTime: null}),
    unsubscribe: async () => {
      state.unsubscribeCalls = (state.unsubscribeCalls || 0) + 1;
      if (state.localFailure) return false;
      state.subscribed = false;
      if (state.verificationFails) state.lookupFailure = true;
      return !state.falseAfterRemoval;
    }};
  const registration = {pushManager: {getSubscription: async () => {if (state.lookupFailure) throw Error('lookup unavailable'); return state.subscribed ? subscription : null;},
    subscribe: async () => {state.subscribed = true; return subscription;}}};
  const context = {document: {getElementById: () => ({querySelector: selector => controls[selector.match(/notification-(\w+)/)[1]]})},
    navigator: {serviceWorker: {getRegistration: async () => registration, register: async () => registration, ready: Promise.resolve(registration)}},
    Notification: {permission: 'granted', requestPermission: async () => {state.permissionCalls = (state.permissionCalls || 0) + 1; return 'granted';}},
    PushManager: function() {}, isSecureContext: true, AbortController, setTimeout, clearTimeout, atob, Uint8Array, TextEncoder,
    crypto: require('node:crypto').webcrypto,
    localStorage: {getItem: key => storage.get(key) || null, setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key)},
    fetch: async (url, options = {}) => {
      const body = options.body && JSON.parse(options.body); calls.push({url, body, savedId: storage.get(browserKey)});
      if (url.endsWith('/register')) {
        state.registered = true;
        if (state.serverRecords) {
          state.serverRecords.set(state.replacementId, body.subscription.endpoint);
          state.outbox.add(state.replacementId);
        }
        if (state.loseRegisterReply) {state.loseRegisterReply = false; throw Error('lost reply');}
        return {ok: true, json: async () => ({registrationId: state.replacementId || browserId})};
      }
      if (url.endsWith('/disable')) {
        assert.equal(Object.keys(body).length, 1);
        assert.ok(body.registrationId === browserId || (state.replacementId && body.registrationId === state.replacementId) || body.endpointDigest === endpointDigest);
        if (state.offline) throw Error('offline');
        if (state.denied) return {ok: false, json: async () => ({error: 'Notification origin is invalid.'})};
        if (state.failSelector && Object.hasOwn(body, state.failSelector)) throw Error('one deletion unavailable');
        if (state.serverRecords) {
          for (const [id, savedEndpoint] of state.serverRecords) {
            const savedDigest = require('node:crypto').createHash('sha256').update(savedEndpoint).digest('hex');
            if (body.registrationId === id || body.endpointDigest === savedDigest) {
              state.serverRecords.delete(id); state.outbox.delete(id);
            }
          }
        }
        state.registered = false;
        if (state.loseIdDeleteReply && body.registrationId) {state.loseIdDeleteReply = false; throw Error('lost reply');}
        return {ok: true, json: async () => ({ok: true})};
      }
      const queriedId = new URL(url, 'https://workspace.example').searchParams.get('registrationId');
      return {ok: true, json: async () => ({available: true, publicKey: 'Ag', registered: state.serverRecords ? state.serverRecords.has(queriedId) : state.registered && queriedId === browserId})};
    },
  };
  vm.runInNewContext(source('browser-notifications.js'), context);
  for (let i = 0; i < 30; i++) await Promise.resolve();
  return {controls, calls, storage};
}

test('lost registration reply, saved ID or changed key disable without registering again', async () => {
  for (const scenario of ['lost-reply', 'key-mismatch', 'lost-storage']) {
    const state = {subscribed: scenario !== 'lost-reply', registered: scenario === 'lost-storage',
      loseRegisterReply: scenario === 'lost-reply', keyMismatch: scenario === 'key-mismatch'};
    const h = await notificationHarness(state);
    if (scenario !== 'lost-storage') await h.controls.enable.click();
    assert.equal(h.controls.status.textContent, 'Browser setup is incomplete. Disable it or enable notifications again.');
    assert.equal(h.controls.disable.disabled, false);
    const registrationsBefore = h.calls.filter(call => call.url.endsWith('/register')).length;
    await h.controls.disable.click();
    assert.equal(state.unsubscribeCalls, 1); assert.equal(state.registered, false);
    assert.match(h.controls.status.textContent, /^Disabled on this browser/);
    assert.equal(h.calls.filter(call => call.url.endsWith('/register')).length, registrationsBefore);
    assert.deepEqual(h.calls.find(call => call.url.endsWith('/disable')).body, {endpointDigest});
  }
});

test('offline or denied lost-ID cleanup unsubscribes and survives reload for explicit retry', async () => {
  for (const failure of ['offline', 'denied']) {
    const state = {subscribed: true, registered: true, [failure]: true};
    const h = await notificationHarness(state);
    await h.controls.disable.click();
    assert.equal(state.subscribed, false); assert.equal(state.registered, true);
    assert.equal(h.controls.status.textContent, 'Browser subscription removed; server cleanup is still pending. Retry Disable.');
    assert.deepEqual(JSON.parse(h.storage.get(cleanupKey)), {endpointDigest});
    assert.equal(h.controls.enable.disabled, true); assert.equal(h.controls.disable.disabled, false);
    const reloaded = await notificationHarness(state, h.storage);
    assert.equal(reloaded.calls.some(call => call.body), false, 'page load performs no mutation');
    await reloaded.controls.enable.click();
    assert.equal(reloaded.calls.some(call => call.body), false, 'Enable cannot discard pending cleanup');
    assert.equal(state.permissionCalls || 0, 0);
    assert.equal(reloaded.controls.status.textContent, 'Browser subscription removed; server cleanup is still pending. Retry Disable.');
    assert.equal(reloaded.controls.disable.disabled, false);
    state[failure] = false;
    await reloaded.controls.disable.click();
    assert.equal(state.registered, false); assert.equal(state.unsubscribeCalls, 1);
    assert.deepEqual(reloaded.calls.find(call => call.url.endsWith('/disable')).body, {endpointDigest});
    assert.equal(h.storage.has(cleanupKey), false);
    assert.match(reloaded.controls.status.textContent, /^Disabled on this browser/);
  }
});

test('known ID survives unknown deletion outcome and retries idempotently after reload', async () => {
  const state = {subscribed: true, registered: true, loseIdDeleteReply: true};
  const storage = new Map([[browserKey, browserId]]);
  const h = await notificationHarness(state, storage);
  await h.controls.disable.click();
  assert.equal(state.subscribed, false); assert.equal(state.registered, false);
  assert.equal(storage.get(browserKey), browserId, 'lost response is not confirmation');
  const reloaded = await notificationHarness(state, storage);
  await reloaded.controls.disable.click();
  assert.deepEqual(reloaded.calls.find(call => call.body?.registrationId).body, {registrationId: browserId});
  assert.equal(storage.has(browserKey), false); assert.equal(storage.has(cleanupKey), false);
  assert.match(reloaded.controls.status.textContent, /^Disabled on this browser/);
});

test('known-ID disable verifies normal removal, false results and retryable local failure', async () => {
  for (const [localFailure, falseAfterRemoval] of [[true, false], [false, true], [false, false]]) {
    const state = {subscribed: true, registered: true, localFailure, falseAfterRemoval};
    const h = await notificationHarness(state, new Map([[browserKey, browserId]]));
    await h.controls.disable.click();
    assert.equal(state.registered, false);
    assert.deepEqual(h.calls.find(call => call.body?.registrationId).body, {registrationId: browserId});
    assert.equal(h.calls.some(call => call.url.endsWith('/register')), false);
    if (localFailure) {
      assert.equal(h.controls.status.textContent, 'Disabling is incomplete. Retry Disable.');
      assert.equal(h.controls.disable.disabled, false);
      state.localFailure = false;
      await h.controls.disable.click();
    }
    assert.match(h.controls.status.textContent, /^Disabled on this browser/);
    assert.equal(state.subscribed, false);
  }
});

test('uncertain local absence retains its selector and Disable control across reload', async () => {
  const state = {subscribed: true, registered: true, verificationFails: true};
  const h = await notificationHarness(state, new Map([[browserKey, browserId]]));
  await h.controls.disable.click();
  assert.equal(state.registered, false); assert.equal(state.subscribed, false);
  assert.equal(h.controls.status.textContent, 'Disabling is incomplete. Retry Disable.');
  assert.equal(h.controls.disable.disabled, false);
  assert.deepEqual(JSON.parse(h.storage.get(cleanupKey)), {endpointDigest});
  state.lookupFailure = false; state.verificationFails = false;
  const reloaded = await notificationHarness(state, h.storage);
  assert.equal(reloaded.controls.disable.disabled, false);
  await reloaded.controls.disable.click();
  assert.match(reloaded.controls.status.textContent, /^Disabled on this browser/);
  assert.equal(h.storage.has(cleanupKey), false);
});

test('stale saved ID and lost replacement reply retain both deletion obligations and outbox truth', async () => {
  for (const failSelector of [null, 'registrationId', 'endpointDigest']) {
    const replacementId = 'e'.repeat(64);
    const state = {subscribed: false, registered: true, loseRegisterReply: true, replacementId, failSelector,
      serverRecords: new Map([[browserId, 'https://fcm.googleapis.com/old-endpoint']]), outbox: new Set([browserId])};
    const h = await notificationHarness(state, new Map([[browserKey, browserId]]));
    await h.controls.enable.click();
    assert.equal(h.storage.get(browserKey), browserId, 'lost reply leaves a stale saved ID');
    assert.equal(state.subscribed, true); assert.equal(state.serverRecords.size, 2); assert.equal(state.outbox.size, 2);
    await h.controls.disable.click();
    assert.equal(state.subscribed, false, 'one failed deletion cannot block local unsubscribe');
    const deletions = h.calls.filter(call => call.url.endsWith('/disable'));
    assert.equal(deletions.length, 2, 'fixed current digest plus saved ID bound');
    assert.deepEqual(deletions.map(call => Object.keys(call.body)[0]).sort(), ['endpointDigest', 'registrationId']);
    assert.equal(h.calls.filter(call => call.url.endsWith('/register')).length, 1, 'Disable does not register');
    if (failSelector) {
      const remaining = failSelector === 'registrationId' ? browserId : replacementId;
      assert.deepEqual([...state.serverRecords.keys()], [remaining]); assert.deepEqual([...state.outbox], [remaining]);
      assert.equal(h.controls.status.textContent, 'Browser subscription removed; server cleanup is still pending. Retry Disable.');
      assert.deepEqual(JSON.parse(h.storage.get(cleanupKey)), {endpointDigest});
      assert.equal(h.storage.get(browserKey) || '', failSelector === 'registrationId' ? browserId : '');
      const reloaded = await notificationHarness(state, h.storage);
      assert.equal(reloaded.calls.some(call => call.body), false, 'reload does not delete');
      assert.equal(reloaded.controls.disable.disabled, false);
      state.failSelector = null;
      await reloaded.controls.disable.click();
      assert.ok(reloaded.calls.filter(call => call.url.endsWith('/disable')).length <= 2);
      assert.match(reloaded.controls.status.textContent, /^Disabled on this browser/);
    } else {
      assert.match(h.controls.status.textContent, /^Disabled on this browser/);
    }
    assert.equal(state.serverRecords.size, 0); assert.equal(state.outbox.size, 0);
    assert.equal(h.storage.has(browserKey), false); assert.equal(h.storage.has(cleanupKey), false);
  }
});

test('successful replacement retires the old ID before overwrite or retains both obligations for retry', async () => {
  for (const failure of [null, 'failed', 'lost-reply']) {
    const replacementId = 'e'.repeat(64);
    const state = {subscribed: false, registered: true, replacementId,
      failSelector: failure === 'failed' ? 'registrationId' : null, loseIdDeleteReply: failure === 'lost-reply',
      serverRecords: new Map([[browserId, 'https://fcm.googleapis.com/old-endpoint']]), outbox: new Set([browserId])};
    const h = await notificationHarness(state, new Map([[browserKey, browserId]]));
    await h.controls.enable.click();
    const retirement = h.calls.find(call => call.url.endsWith('/disable'));
    assert.deepEqual(retirement.body, {registrationId: browserId});
    assert.equal(retirement.savedId, browserId, 'old ID was not overwritten before its deletion');
    assert.equal(state.subscribed, true, 'Enable does not automatically unsubscribe');
    if (failure) {
      assert.equal(h.storage.get(browserKey), browserId, 'unknown deletion must retain the old ID');
      assert.deepEqual(JSON.parse(h.storage.get(cleanupKey)), {endpointDigest});
      assert.equal(h.controls.status.textContent, 'Disabling is incomplete. Retry Disable.');
      assert.equal(h.controls.disable.disabled, false); assert.equal(h.controls.enable.disabled, true);
      assert.equal(state.serverRecords.has(replacementId), true); assert.equal(state.outbox.has(replacementId), true);
      assert.equal(state.serverRecords.has(browserId), failure === 'failed');
      const reloaded = await notificationHarness(state, h.storage);
      assert.equal(reloaded.calls.some(call => call.body), false, 'reload preserves obligations without deletion');
      state.failSelector = null;
      await reloaded.controls.disable.click();
      assert.equal(reloaded.calls.filter(call => call.url.endsWith('/disable')).length, 2);
      assert.match(reloaded.controls.status.textContent, /^Disabled on this browser/);
    } else {
      assert.equal(h.storage.get(browserKey), replacementId);
      assert.equal(h.storage.has(cleanupKey), false);
      assert.match(h.controls.status.textContent, /^Enabled on this browser/);
      assert.deepEqual([...state.serverRecords.keys()], [replacementId]); assert.deepEqual([...state.outbox], [replacementId]);
      await h.controls.disable.click();
      assert.match(h.controls.status.textContent, /^Disabled on this browser/);
    }
    assert.equal(state.subscribed, false); assert.equal(state.serverRecords.size, 0); assert.equal(state.outbox.size, 0);
    assert.equal(h.storage.has(browserKey), false); assert.equal(h.storage.has(cleanupKey), false);
  }
});
