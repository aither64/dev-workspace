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
    importScripts: value => assert.equal(value, '/static/session-attention-store.js?v=2'),
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
  const calls = [], listeners = {};
  const subscription = {endpoint, options: state.keyMismatch ? {applicationServerKey: new Uint8Array([1]).buffer} : {},
    toJSON: () => ({endpoint, keys: {p256dh: 'public', auth: 'auth'}, expirationTime: null}),
    unsubscribe: async () => {
      state.unsubscribeCalls = (state.unsubscribeCalls || 0) + 1;
      if (state.localFailure) return false;
      state.subscribed = false;
      if (state.verificationFails) state.lookupFailure = true;
      return !state.falseAfterRemoval;
    }};
  const registration = {pushManager: {getSubscription: async () => {if (state.subscriptionRead) return state.subscriptionRead(subscription); if (state.lookupFailure) throw Error('lookup unavailable'); return state.subscribed ? subscription : null;},
    subscribe: async () => {state.subscribed = true; return subscription;}}};
  const context = {document: {visibilityState: 'visible', addEventListener: (name, callback) => listeners[name] = callback,
    getElementById: () => ({querySelector: selector => controls[selector.match(/notification-(\w+)/)[1]]})},
    addEventListener: (name, callback) => listeners[name] = callback,
    navigator: {serviceWorker: {addEventListener: (name, callback) => listeners[name] = callback,
      getRegistration: async () => registration, register: async () => {state.onWorkerRegister?.(); return registration;}, ready: Promise.resolve(registration)}},
    Notification: {permission: 'granted', requestPermission: async () => {state.permissionCalls = (state.permissionCalls || 0) + 1; return 'granted';}},
    PushManager: function() {}, isSecureContext: true, AbortController, setTimeout, clearTimeout, atob, Uint8Array, TextEncoder,
    crypto: require('node:crypto').webcrypto,
    localStorage: {getItem: key => storage.get(key) || null, setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key)},
    fetch: async (url, options = {}) => {
      const body = options.body && JSON.parse(options.body); calls.push({url, body, savedId: storage.get(browserKey)});
      if (url.endsWith('/register')) {
        if (state.registerResponse) return state.registerResponse();
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
      if (queriedId !== null && state.readResponse) return state.readResponse(queriedId);
      return {ok: true, json: async () => ({available: true, publicKey: 'Ag', registered: state.serverRecords ? state.serverRecords.has(queriedId) : state.registered && queriedId === browserId})};
    },
  };
  vm.runInNewContext(source('browser-notifications.js'), context);
  for (let i = 0; i < 30; i++) await Promise.resolve();
  const flush = async () => {for (let i = 0; i < 50; i++) await Promise.resolve();};
  return {controls, calls, storage, listeners, context, registration, flush};
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

// Exercise the shipped shared module with asynchronous transactions, retaining
// the same in-memory database across worker/document harnesses and reloads.
function testStoreHarness() {
  const records = new Map(), state = {failed: false}, versions = [];
  const database = {transaction() {
    if (state.failed) throw Error('private storage canary');
    const tx = {}; let pending = 0;
    const request = action => {
      const req = {}; pending++;
      queueMicrotask(() => {
        req.result = action(); req.onsuccess?.();
        if (--pending === 0) queueMicrotask(() => tx.oncomplete?.());
      });
      return req;
    };
    tx.objectStore = () => ({get: key => request(() => structuredClone(records.get(key))),
      put: value => records.set(value.identity, structuredClone(value)), getAll: () => request(() => [...records.values()].map(value => structuredClone(value))),
      delete: key => records.delete(key)});
    return tx;
  }};
  const context = {indexedDB: {open(name, version) {
    assert.equal(name, 'workspace-session-attention'); versions.push(version);
    const req = {}; queueMicrotask(() => {req.result = database; req.onsuccess();}); return req;
  }}, Date};
  vm.runInNewContext(source('session-attention-store.js'), context);
  return {store: context.workspaceAttentionStore, records, state, versions};
}
const testDescriptor = (revision = 100, digit = 'c') => ({identity: `test-${'d'.repeat(64)}`, id: digit.repeat(64), revision, expiresAt: Date.now() + 60000});
const testPayload = descriptor => ({...descriptor, version: 1, kind: 'test', slug: '', route: '/'});
function displayHarness(storage = testStoreHarness()) {
  const listeners = {}, entries = [], shown = [], closed = [], messages = [];
  const state = {record: 'present', failure: null, hold: null};
  const self = {location: {origin: 'https://workspace.example'}, registration: {
    async showNotification(title, options) {
      shown.push({title, ...options});
      if (state.hold) await state.hold;
      if (state.failure) throw state.failure;
      if (state.record === 'present') entries.push({...options, close() {closed.push(this.tag);}});
    },
    async getNotifications(options) {
      if (state.record === 'unavailable') throw Error('private lookup canary');
      return entries.filter(entry => !closed.includes(entry.tag) && (!options?.tag || entry.tag === options.tag));
    },
  }, clients: {matchAll: async () => [], claim: async () => {}}, addEventListener: (name, callback) => listeners[name] = callback};
  const context = {self, URL, Date, AbortController, setTimeout, clearTimeout, importScripts() {}, workspaceAttentionStore: storage.store};
  vm.runInNewContext(source('notification-worker.js'), context);
  const dispatch = (name, value) => {let pending; listeners[name]({...value, waitUntil: promise => pending = promise}); return pending;};
  return {state, entries, shown, closed, messages, storage,
    push: descriptor => dispatch('push', {data: {json: () => testPayload(descriptor)}}),
    ack: identity => dispatch('message', {data: {type: 'session-attention-ack', identity}}),
    capabilities: () => {listeners.message({data: {type: 'notification-test-capabilities'}, ports: [{postMessage: value => messages.push(value)}]}); return messages.at(-1);}};
}

test('test receipts distinguish receipt, display resolution/rejection and browser record without leaking errors', async () => {
  for (const record of ['present', 'absent', 'unavailable']) {
    const h = displayHarness(), descriptor = testDescriptor(); h.state.record = record;
    let release; h.state.hold = new Promise(resolve => {release = resolve;});
    const work = h.push(descriptor);
    for (let i = 0; i < 30; i++) await Promise.resolve();
    assert.equal(h.storage.records.get(descriptor.identity).testOutcome.display, 'pending', 'receipt alone does not certify display');
    release(); await work;
    const outcome = h.storage.records.get(descriptor.identity).testOutcome;
    assert.equal(outcome.received, true); assert.equal(outcome.display, 'resolved'); assert.equal(outcome.browserRecord, record);
    assert.deepEqual(JSON.parse(JSON.stringify(h.capabilities())), {version: 1});
  }
  for (const name of ['NotAllowedError', 'private-error-canary']) {
    const h = displayHarness(), descriptor = testDescriptor(); h.state.failure = {name, message: 'secret message canary'};
    await h.push(descriptor);
    const outcome = h.storage.records.get(descriptor.identity).testOutcome;
    assert.equal(outcome.display, 'rejected'); assert.equal(outcome.error, name === 'NotAllowedError' ? name : 'UnknownError');
    assert.equal(JSON.stringify(outcome).includes('canary'), false);
  }
});

test('test storage failure still displays and fresh tests retire only older same-identity test entries', async () => {
  const h = displayHarness(), first = testDescriptor(), second = testDescriptor(101, 'e');
  await h.storage.store.saveTest(second);
  await h.push(first);
  assert.equal(h.storage.records.get(first.identity).testPending.id, second.id);
  assert.equal(h.storage.records.get(first.identity).testOutcome, undefined, 'old push cannot certify newer test');
  await h.push(second);
  const sessionTag = `session-${identity}`;
  h.entries.push({tag: sessionTag, data: {identity}, close() {h.closed.push(sessionTag);}});
  assert.notEqual(h.shown[0].tag, h.shown[1].tag, 'explicit repeat test has a fresh tag');
  assert.ok(h.closed.includes(h.shown[0].tag)); assert.equal(h.closed.includes(sessionTag), false);
  const before = JSON.stringify(h.storage.records.get(first.identity).testOutcome);
  h.state.failure = {name: 'NotAllowedError'};
  await h.push(second);
  assert.equal(h.shown.at(-1).tag, h.shown[1].tag, 'duplicate uses the same tag');
  assert.equal(JSON.stringify(h.storage.records.get(first.identity).testOutcome), before, 'duplicate cannot downgrade a completed result');
  await h.ack(identity);
  assert.equal(h.closed.includes(h.shown[1].tag), false, 'focused session acknowledgement cannot close Test');
  h.storage.state.failed = true; h.state.failure = null;
  await h.push(testDescriptor(102, 'f'));
  assert.equal(h.shown.at(-1).body, 'Browser notification test');
  assert.deepEqual(h.storage.versions, [1], 'no IndexedDB migration');
});

async function feedbackHarness(storage = testStoreHarness(), options = {}) {
  const controls = Object.fromEntries(['status','enable','disable','test','test-status','provider-status','worker-status'].map(name => [name,
    {disabled: false, textContent: '', addEventListener(type, callback) {this[type] = callback;}}]));
  const listeners = {}, timers = new Map(), calls = [], saved = new Map([[browserKey, browserId]]);
  let now = Date.now(), nextTimer = 0;
  const state = {descriptor: testDescriptor(now), registered: true, workerVersion: options.workerVersion ?? 1};
  const registration = {update: async () => {state.updateChecks = (state.updateChecks || 0) + 1;}, active: {postMessage(_message, ports) {ports[0].reply({version: state.workerVersion});}},
    pushManager: {getSubscription: async () => ({endpoint, unsubscribe: async () => true})}};
  if (options.waitingWorker) registration.waiting = {};
  const context = {document: {visibilityState: 'visible', getElementById: () => ({querySelector: selector => controls[selector.slice(19, -1) || 'status']}),
    addEventListener: (name, callback) => listeners[name] = callback}, navigator: {serviceWorker: {getRegistration: async () => registration,
      addEventListener: (name, callback) => listeners[name] = callback}}, addEventListener: (name, callback) => listeners[name] = callback,
    Date: class extends Date {static now() {return now;}}, Notification: {permission: 'granted'}, PushManager: function() {}, isSecureContext: true,
    workspaceAttentionStore: options.oldStore ? {} : storage.store, AbortController, Uint8Array, TextEncoder, crypto: require('node:crypto').webcrypto,
    MessageChannel: function() {this.port1 = {close() {}}; this.port2 = {reply: value => queueMicrotask(() => this.port1.onmessage({data: value}))};},
    setTimeout(callback, delay) {const id = ++nextTimer; timers.set(id, {callback, at: now + delay}); return id;}, clearTimeout: id => timers.delete(id),
    localStorage: {getItem: key => saved.get(key) || null, setItem: (key,value) => saved.set(key,value), removeItem: key => saved.delete(key)},
    fetch: async (url, config = {}) => {calls.push({url, config});
      if (url.endsWith('/test') && state.testReply) return state.testReply();
      return {ok: true, json: async () => url.endsWith('/test') ? {ok: true, test: options.oldServer ? null : state.descriptor} : {available: true, registered: state.registered, result: 'provider_accepted'}};},
  };
  context.document.getElementById = () => ({querySelector: selector => controls[selector.replace('[data-notification-','').replace(']','')]});
  vm.runInNewContext(source('browser-notifications.js'), context);
  const flush = async () => {for (let i = 0; i < 80; i++) await Promise.resolve();};
  await flush();
  return {controls, storage, state, timers, calls, context, listeners, registration, async advance(ms) {now += ms;
    const ready = [...timers].filter(([,timer]) => timer.at <= now); for (const [id,timer] of ready) {timers.delete(id); timer.callback(); await flush();}}, flush};
}

test('test UI advances from queue only on an exact post-display result and keeps provider correlation explicit', async () => {
  const h = await feedbackHarness();
  assert.equal(h.calls.some(call => call.config.method === 'POST'), false);
  assert.equal(h.state.updateChecks, 1, 'existing worker is checked without registration or permission');
  await h.controls.test.click();
  assert.match(h.controls.status.textContent, /10 seconds/);
  const descriptor = h.state.descriptor;
  await h.storage.store.saveTest({...descriptor, id: 'f'.repeat(64), revision: descriptor.revision - 1}, {received: true, display: 'resolved', browserRecord: 'present'});
  await h.advance(2000);
  assert.match(h.controls['test-status'].textContent, /Waiting for this test/);
  assert.match(h.controls['provider-status'].textContent, /may be from another notification/);
  await h.storage.store.saveTest(descriptor, {received: true, display: 'pending', browserRecord: 'unavailable'});
  h.listeners.message({data: {type: 'notification-test-update', identity: descriptor.identity}}); await h.flush();
  assert.match(h.controls['test-status'].textContent, /Waiting for its display result/);
  await h.storage.store.saveTest(descriptor, {received: true, display: 'resolved', browserRecord: 'absent'});
  h.listeners.message({data: {type: 'notification-test-update', identity: descriptor.identity}}); await h.flush();
  assert.match(h.controls['test-status'].textContent, /No matching notification is recorded/);
  assert.match(h.controls['test-status'].textContent, /unconfirmed/);
  assert.equal(h.timers.size, 0, 'completed test stops polling');
  assert.doesNotMatch(h.controls.status.textContent, /10 seconds/, 'queue text advances');
});

test('test wait survives reopen, stops at pagehide/expiry/disable and treats old components as unknown', async () => {
  const h = await feedbackHarness(); await h.controls.test.click();
  h.listeners.pagehide(); assert.equal(h.timers.size, 0);
  const reopened = await feedbackHarness(h.storage); await reopened.flush();
  assert.match(reopened.controls['test-status'].textContent, /Waiting for this test/);
  await reopened.advance(71000);
  assert.match(reopened.controls['test-status'].textContent, /not confirmed.*check ended/);
  assert.equal(reopened.timers.size, 0);
  await reopened.controls.disable.click();
  assert.equal(h.storage.records.get(h.state.descriptor.identity).testPending, undefined);
  for (const options of [{oldServer: true}, {oldStore: true}]) {
    const old = await feedbackHarness(testStoreHarness(), options); await old.controls.test.click();
    assert.match(old.controls['test-status'].textContent, /matching test result is unavailable/);
    assert.equal(old.timers.size, 0);
  }
});

test('UI preserves rejected/unavailable results, other-tab correlation and worker activation limits', async () => {
  for (const display of ['rejected', 'resolved']) {
    const h = await feedbackHarness(); await h.controls.test.click();
    await h.storage.store.saveTest(h.state.descriptor, {received: true, display, error: 'canary', browserRecord: 'unavailable', privateValue: 'canary'});
    await h.advance(2000);
    assert.match(h.controls['test-status'].textContent, display === 'rejected' ? /rejected.*UnknownError/ : /record could not be checked.*unconfirmed/);
    assert.equal(JSON.stringify(h.storage.records.get(h.state.descriptor.identity)).includes('canary'), false);
    assert.equal(h.timers.size, 0);
  }
  const h = await feedbackHarness(); await h.controls.test.click();
  await h.storage.store.saveTest({...h.state.descriptor, id: 'e'.repeat(64), revision: h.state.descriptor.revision + 1});
  await h.advance(2000);
  assert.match(h.controls['test-status'].textContent, /Another test replaced this check/); assert.equal(h.timers.size, 0);
  for (const options of [{workerVersion: 0}, {waitingWorker: true}]) {
    const old = await feedbackHarness(testStoreHarness(), options);
    assert.match(old.controls['worker-status'].textContent, /Close portal tabs/);
    assert.equal(old.calls.some(call => call.config.method === 'POST'), false, 'updates never mutate subscriptions');
  }
});

test('failed local result reads remain unknown and replacement/Disable cancel the saved wait', async () => {
  const h = await feedbackHarness(); await h.controls.test.click(); h.storage.state.failed = true;
  await h.advance(2000);
  assert.match(h.controls['test-status'].textContent, /could not read.*unconfirmed/);
  h.listeners.pagehide(); assert.equal(h.timers.size, 0);
  h.storage.state.failed = false;
  const reopened = await feedbackHarness(h.storage); await reopened.flush();
  reopened.listeners.storage({key: browserKey, newValue: 'f'.repeat(64)}); await reopened.flush();
  assert.equal(reopened.timers.size, 0);
  assert.equal(h.storage.records.get(h.state.descriptor.identity).testPending, undefined);
  assert.equal(reopened.controls.test.disabled, true);
  const enabling = await feedbackHarness(); await enabling.controls.test.click();
  await enabling.controls.enable.click(); await enabling.flush();
  assert.equal(enabling.timers.size, 0, 'an explicit replacement attempt stops the old test wait');
  assert.equal(enabling.storage.records.get(enabling.state.descriptor.identity).testPending, undefined);
});

test('a stalled diagnostic write cannot prevent the worker display attempt', async () => {
  const h = displayHarness(); h.storage.store.saveTest = () => new Promise(() => {});
  await h.push(testDescriptor());
  assert.equal(h.shown.length, 1); assert.equal(h.shown[0].body, 'Browser notification test');
  assert.equal(h.storage.records.size, 0, 'no fabricated display evidence after storage timeout');
});

test('a failed or lost second Test retires the old check before sending and late old results cannot certify it', async () => {
  for (const failure of ['rejected', 'lost']) {
    const h = await feedbackHarness(); await h.controls.test.click();
    const first = h.state.descriptor;
    let rejectReply;
    h.state.testReply = () => {
      assert.equal(h.storage.records.get(first.identity).testPending, undefined, 'saved A is retired before submitting B');
      return new Promise((resolve, reject) => {rejectReply = () => failure === 'lost' ? reject(Error('lost reply')) :
        resolve({ok: false, json: async () => ({error: 'Browser notifications are disabled.'})});});
    };
    const secondClick = h.controls.test.click(); await h.flush();
    assert.match(h.controls['test-status'].textContent, /matching test result is unavailable/);
    await h.storage.store.saveTest(first, {received: true, display: 'resolved', browserRecord: 'present'});
    h.listeners.message({data: {type: 'notification-test-update', identity: first.identity}}); await h.flush();
    assert.match(h.controls['test-status'].textContent, /matching test result is unavailable/);
    rejectReply(); await secondClick;
    h.listeners.message({data: {type: 'notification-test-update', identity: first.identity}});
    h.listeners.visibilitychange(); h.listeners.pageshow(); await h.flush();
    assert.match(h.controls['test-status'].textContent, /matching test result is unavailable/);
    assert.equal(h.timers.size, 0, 'retired A cannot restart polling after failed B');
    const reopened = await feedbackHarness(h.storage);
    assert.equal(reopened.controls['test-status'].textContent, '', 'retired A does not auto-resume on reload');
    assert.equal(reopened.timers.size, 0);
  }
});

test('an older document cancellation cannot erase a newer shared descriptor, including single-field mismatches', async () => {
  for (const field of ['id', 'revision', 'expiresAt', 'identity']) {
    const storage = testStoreHarness(), firstTab = await feedbackHarness(storage);
    await firstTab.controls.test.click(); const first = firstTab.state.descriptor;
    const secondTab = await feedbackHarness(storage);
    secondTab.state.descriptor = {...first, id: 'e'.repeat(64), revision: first.revision + 1};
    await secondTab.controls.test.click(); const second = secondTab.state.descriptor;
    // Exercise every field of the exact cancellation match independently. This
    // simulates an accepted newer descriptor already saved by the other page.
    const stored = {...first, [field]: second[field]};
    if (field === 'expiresAt') stored.expiresAt = first.expiresAt + 1;
    if (field === 'identity') stored.identity = `test-${'f'.repeat(64)}`;
    storage.records.get(first.identity).testPending = structuredClone(stored);
    firstTab.listeners.storage({key: browserKey, newValue: 'f'.repeat(64)}); await firstTab.flush();
    assert.deepEqual(storage.records.get(first.identity).testPending, stored, `${field} difference prevents stale cancellation`);
    storage.records.get(first.identity).testPending = structuredClone(second);
    const reopened = await feedbackHarness(storage);
    assert.match(reopened.controls['test-status'].textContent, /Waiting for this test/);
    await storage.store.saveTest(second, {received: true, display: 'resolved', browserRecord: 'present'});
    reopened.listeners.message({data: {type: 'notification-test-update', identity: second.identity}}); await reopened.flush();
    assert.match(reopened.controls['test-status'].textContent, /matching notification is recorded/);
  }
});

test('a delayed resume read cannot overwrite a new Test or undo cancellation, pagehide or registration replacement', async () => {
  for (const action of ['new-test', 'disable', 'pagehide', 'replacement']) {
    const storage = testStoreHarness(), descriptor = testDescriptor(Date.now());
    await storage.store.saveTest(descriptor);
    const read = storage.store.read; let release;
    storage.store.read = async identity => {
      const snapshot = structuredClone(await read(identity));
      return new Promise(resolve => {release = () => resolve(snapshot);});
    };
    const h = await feedbackHarness(storage); assert.equal(typeof release, 'function');
    storage.store.read = read;
    if (action === 'new-test') {
      h.state.descriptor = {...descriptor, id: 'e'.repeat(64), revision: descriptor.revision + 1};
      await h.controls.test.click();
    } else if (action === 'disable') await h.controls.disable.click();
    else if (action === 'pagehide') h.listeners.pagehide();
    else h.listeners.storage({key: browserKey, newValue: 'f'.repeat(64)});
    release(); await h.flush();
    if (action === 'new-test') {
      await storage.store.saveTest(h.state.descriptor, {received: true, display: 'resolved', browserRecord: 'present'});
      h.listeners.message({data: {type: 'notification-test-update', identity: descriptor.identity}}); await h.flush();
      assert.match(h.controls['test-status'].textContent, /matching notification is recorded/, 'late A read did not displace B');
    } else {
      assert.equal(h.controls['test-status'].textContent, '');
      assert.equal(h.timers.size, 0, 'invalidated resume never schedules a wait');
    }
  }
});

test('deferred, failed or timed-out retirement cannot auto-resume A or submit B before retirement succeeds', async () => {
  for (const retirement of ['deferred', 'failed', 'timeout']) {
    const h = await feedbackHarness(); await h.controls.test.click(); const first = h.state.descriptor;
    const update = h.storage.store.update; let release;
    h.storage.store.update = (identity, change) => new Promise((resolve, reject) => {
      release = () => retirement === 'failed' ? reject(Error('storage unavailable')) : resolve(update(identity, change));
    });
    h.state.testReply = async () => {throw Error('lost second reply');};
    const second = h.controls.test.click(); await h.flush();
    h.listeners.visibilitychange(); h.listeners.pageshow(); await h.flush();
    assert.equal(h.calls.filter(call => call.url.endsWith('/test')).length, 1, 'no B request while A retirement is pending');
    assert.match(h.controls['test-status'].textContent, /matching test result is unavailable/);
    if (retirement === 'timeout') {await h.advance(1001); await second;}
    else {release(); await second;}
    await h.storage.store.saveTest(first, {received: true, display: 'resolved', browserRecord: 'present'});
    h.listeners.message({data: {type: 'notification-test-update', identity: first.identity}});
    h.listeners.visibilitychange(); h.listeners.pageshow(); await h.flush();
    assert.match(h.controls['test-status'].textContent, /matching test result is unavailable/);
    assert.equal(h.timers.size, 0, 'busy false cannot auto-load retired A after a failed deletion or B reply');
    assert.equal(h.calls.filter(call => call.url.endsWith('/test')).length, retirement === 'deferred' ? 2 : 1,
      'failed or uncertain local retirement refuses B submission');
    if (retirement === 'timeout') {release(); await h.flush();}
    assert.equal(Boolean(h.storage.records.get(first.identity).testPending), retirement === 'failed',
      'failed persistence remains explicitly unconfirmed; no second request was issued');
  }
});

const observationReply = (registered, available = true) => ({ok: true, json: async () => ({registered, available})});
test('old refresh success or rejection cannot replace Enable success for existing or absent local subscriptions', async () => {
  for (const subscribed of [false, true]) for (const reject of [false, true]) {
    const state = {subscribed, registered: false};
    const h = await notificationHarness(state);
    let finish;
    state.readResponse = () => new Promise((resolve, fail) => {finish = () => reject ? fail(Error('old read failed')) : resolve(observationReply(false));});
    h.listeners.controllerchange(); await h.flush();
    assert.equal(typeof finish, 'function', 'refresh GET started before Enable');
    await h.controls.enable.click();
    assert.equal(h.controls.test.disabled, false);
    assert.match(h.controls.status.textContent, /^Enabled on this browser/);
    const enabledStatus = h.controls.status.textContent;
    finish(); await h.flush();
    assert.equal(h.controls.status.textContent, enabledStatus);
    assert.equal(h.controls.test.disabled, false, 'stale GET cannot revoke new registration');
    delete state.readResponse;
    await h.controls.disable.click();
    assert.equal(state.subscribed, false, 'staged old local state did not displace current subscription');
  }
});

test('refresh local reads stage atomically and controllerchange during Enable performs no competing observation', async () => {
  const state = {subscribed: false, registered: false};
  const h = await notificationHarness(state);
  let release;
  state.subscriptionRead = () => new Promise(resolve => {release = () => resolve(null);});
  h.listeners.controllerchange(); await h.flush();
  delete state.subscriptionRead;
  let heldRegister;
  state.registerResponse = () => new Promise(resolve => {heldRegister = () => {
    state.registered = true; resolve({ok: true, json: async () => ({registrationId: browserId})});
  };});
  const enabling = h.controls.enable.click(); await h.flush();
  const readsBefore = h.calls.filter(call => call.url.includes('?registrationId=')).length;
  h.listeners.controllerchange(); h.listeners.visibilitychange(); await h.flush();
  assert.equal(h.calls.filter(call => call.url.includes('?registrationId=')).length, readsBefore, 'triggers ignored during action');
  heldRegister(); await enabling;
  const status = h.controls.status.textContent;
  release(); await h.flush();
  assert.equal(h.controls.status.textContent, status); assert.equal(h.controls.test.disabled, false);
  assert.equal(h.calls.filter(call => call.url.includes('?registrationId=')).length, readsBefore, 'retired local read never issues stale GET');
});

test('newest refresh wins reversed success and failure completion ordering', async () => {
  for (const oldFails of [false, true]) {
    const state = {subscribed: true, registered: true};
    const h = await notificationHarness(state, new Map([[browserKey, browserId]]));
    const pending = [];
    state.readResponse = () => new Promise((resolve, reject) => pending.push({resolve, reject}));
    h.listeners.controllerchange(); await h.flush();
    h.listeners.controllerchange(); await h.flush();
    assert.equal(pending.length, 2);
    pending[1].resolve(observationReply(true)); await h.flush();
    const currentStatus = h.controls.status.textContent;
    assert.equal(h.controls.test.disabled, false);
    if (oldFails) pending[0].reject(Error('older failure'));
    else pending[0].resolve(observationReply(false, false));
    await h.flush();
    assert.equal(h.controls.status.textContent, currentStatus); assert.equal(h.controls.test.disabled, false);
  }
});

test('refresh completion cannot undo Disable, cross-tab revocation, pagehide or an explicit Test', async () => {
  for (const action of ['disable', 'revocation', 'pagehide', 'test']) for (const reject of [false, true]) {
    const state = {subscribed: true, registered: true};
    const h = await notificationHarness(state, new Map([[browserKey, browserId]]));
    let finish;
    state.readResponse = () => new Promise((resolve, fail) => {finish = () => reject ? fail(Error('old read failed')) : resolve(observationReply(true));});
    h.listeners.controllerchange(); await h.flush();
    if (action === 'disable') await h.controls.disable.click();
    else if (action === 'revocation') h.listeners.storage({key: browserKey, newValue: null});
    else if (action === 'pagehide') h.listeners.pagehide();
    else await h.controls.test.click();
    const status = h.controls.status.textContent, disabled = h.controls.test.disabled;
    finish(); await h.flush();
    assert.equal(h.controls.status.textContent, status); assert.equal(h.controls.test.disabled, disabled);
    if (action === 'revocation') {
      const before = h.calls.length; h.listeners.controllerchange(); h.listeners.visibilitychange(); await h.flush();
      assert.equal(h.calls.length, before, 'revoked observation cannot restart under old saved identity');
    }
  }
});

test('real failed registration stays incomplete for both local subscription starting states', async () => {
  for (const subscribed of [false, true]) {
    const state = {subscribed, registered: false, registerResponse: async () => ({ok: false, json: async () => ({error: 'Registration could not be created.'})})};
    const h = await notificationHarness(state);
    state.onWorkerRegister = () => h.listeners.controllerchange();
    await h.controls.enable.click(); await h.flush();
    assert.equal(h.controls.status.textContent, 'Browser setup is incomplete. Disable it or enable notifications again.');
    assert.equal(h.controls.test.disabled, true); assert.equal(h.controls.disable.disabled, false);
  }
});

test('browser fixture failure trace is bounded and excludes raw capability, ID, body and exception canaries', async () => {
  const fixture = fs.readFileSync(path.join(__dirname, 'notifications_browser_test.cjs'), 'utf8');
  const helper = fixture.slice(fixture.indexOf('const safeError ='), fixture.indexOf('async function checkControlSpacing'));
  const events = {}, output = [], context = {URL, setTimeout, clearTimeout, console: {log: value => output.push(value)}};
  vm.runInNewContext(helper, context);
  const trace = context.failureTrace({on: (name, callback) => events[name] = callback});
  const request = {url: () => 'https://workspace.example/api/browser-notifications?registrationId=private-id-canary', method: () => 'GET',
    frame: () => ({evaluate: async () => false})};
  events.request(request);
  events.response({request: () => request, status: () => 200,
    json: async () => ({registered: true, available: true, endpoint: 'endpoint-canary', keys: {auth: 'key-canary'}, error: 'body-canary'})});
  const register = {url: () => 'https://workspace.example/api/browser-notifications/register', method: () => 'POST'};
  events.request(register);
  events.response({request: () => register, status: () => 400,
    json: async () => {throw {name: 'private-error-canary', message: 'exception-canary'};}});
  await trace.dump({evaluate: async () => ({state: 'incomplete', savedIdPresent: false, testDisabled: true, controller: 'activated', active: true, waiting: false, installing: false})});
  assert.equal(output.length, 1); assert.doesNotMatch(output[0], /canary|https:|registrationId/);
  const rows = JSON.parse(output[0]).notificationFailureTrace;
  assert.equal(rows[0].event, 'request'); assert.equal(rows[0].request, 1);
  assert.ok(rows.some(row => row.event === 'response' && row.request === 1 && row.status === 200));
  assert.ok(rows.some(row => row.event === 'response_shape' && row.registered && row.available));
  assert.ok(rows.some(row => row.identity === 'stale'));
  assert.ok(rows.some(row => row.event === 'response_unavailable' && row.category === 'UnknownError'));
  for (let i = 0; i < 200; i++) events.request(request);
  await trace.dump({evaluate: async () => {throw {name: 'private-error-canary', message: 'exception-canary'};}});
  assert.ok(Buffer.byteLength(output[1], 'utf8') <= 8192);
  assert.ok(JSON.parse(output[1]).dropped > 0); assert.doesNotMatch(output[1], /canary|https:|registrationId/);
});
