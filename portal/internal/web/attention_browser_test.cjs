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
  let soundReads = 0;
  const sound = {value: true, failed: false};
  let status = {fresh: true, enabled: true, identity, event: event(), revision: 1}, inaccessible = false;
  const context = {URL, AbortController, setTimeout, clearTimeout, Date,
    importScripts: value => assert.equal(value, '/static/session-attention-store.js?v=4'),
    workspaceAttentionStore: {readDeliveryGate: async () => null, readSound: async () => {soundReads++; if (sound.failed) throw Error("sound read canary"); return sound.value;}, update: async (key, change) => {
      const record = change(records.get(key) || {identity: key, seen: [], revision: 0, event: null}); records.set(key, record); return record;
    }},
    fetch: async (url, options) => { calls.push({url, options}); return {ok: !inaccessible, status: inaccessible ? 401 : 200, json: async () => status}; },
    self: {location: {origin: 'https://workspace.example'}, registration: {
      showNotification: async (title, options) => notifications.push({title, ...options}), getNotifications: async () => [],
    }, skipWaiting: async () => calls.push({skipWaiting: true}),
    clients: {matchAll: async () => windows, openWindow: async url => calls.push({openWindow: url}), claim: async () => calls.push({claim: true})},
    addEventListener: (name, listener) => listeners[name] = listener},
  };
  vm.runInNewContext(source('notification-worker.js'), context);
  return {notifications, windows, calls, records, context, sound, soundReads: () => soundReads,
    lifecycle: async name => {let pending; listeners[name]({waitUntil: promise => pending = promise}); await pending;},
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
  const panel = {querySelector: selector => controls[selector.slice(19, -1)]};
  const context = {document: {querySelectorAll: selector => [panel.querySelector(selector)].filter(Boolean), getElementById: () => null}, navigator: {locks:{request:async(_name,work)=>work()},serviceWorker: {getRegistration: async () => null}},
    Notification: {permission: 'default', requestPermission: async () => { prompts++; return 'denied'; }},
    isSecureContext: true, PushManager: function() {}, AbortController, setTimeout, clearTimeout,
    localStorage: {getItem: key => storage.get(key) || null}, workspaceAttentionStore:testStoreHarness().store, crypto:require('node:crypto').webcrypto,
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

test('50 attention documents stop hidden HTTP and storage work and retire old results on resume', async () => {
  const documents=[];
  for(let index=0;index<50;index++) {
    const listeners={},reads=[],timers=new Map();let timerID=0,stores=0;
    const document={body:{dataset:{session:'example',sessionObservation:identity,rootThreadId:'thread-1'}},title:'example',visibilityState:index?'hidden':'visible',hidden:index!==0,
      hasFocus:()=>false,getElementById:()=>null,addEventListener:(name,fn)=>{listeners[name]=fn;}};
    const context={document,window:{addEventListener:(name,fn)=>{listeners[name]=fn;}},navigator:{},AbortController,
      setTimeout:(fn,delay)=>{timers.set(++timerID,{fn,delay});return timerID;},clearTimeout:id=>timers.delete(id),
      workspaceAttentionStore:{update:async(key,change=value=>value)=>{stores++;return change({seen:[],revision:0});}},
      fetch:(url,options)=>new Promise((resolve,reject)=>reads.push({url,options,resolve,reject}))};
    vm.runInNewContext(source('session-attention.js'),context);
    documents.push({document,listeners,reads,timers,stores:()=>stores});
  }
  const flush=async()=>{for(let i=0;i<30;i++)await Promise.resolve();};
  const active=documents[0];assert.equal(active.reads.length,1);
  for(const h of documents.slice(1)) {h.listeners.focus();h.listeners.pageshow();assert.equal(h.reads.length,0);assert.equal(h.stores(),0);}
  active.document.visibilityState='hidden';active.document.hidden=true;active.listeners.visibilitychange();
  assert(active.reads[0].options.signal.aborted);
  active.document.visibilityState='visible';active.document.hidden=false;active.listeners.visibilitychange();active.listeners.focus();
  assert.equal(active.reads.length,2,'fresh read does not wait for aborted transport');
  active.reads[0].resolve({ok:true,json:async()=>({version:1,identity,rootThread:'thread-1',revision:999,event:event()})});await flush();
  assert.equal(active.stores(),0,'old body cannot write storage');
  active.listeners.focus();assert.equal(active.reads.length,2,'old finally cannot clear successor');
  active.reads[1].resolve({ok:true,json:async()=>({version:1,identity,rootThread:'thread-1',revision:1,event:event()})});await flush();
  assert.equal(active.stores(),1);assert.equal(active.reads.length,3);
  active.reads[2].resolve({ok:true,json:async()=>({reservationHolders:[]})});await flush();
  assert.equal(active.document.title,'? example');
  active.document.visibilityState='hidden';active.document.hidden=true;active.listeners.visibilitychange();
  for(const h of documents) {h.listeners.focus();for(const timer of h.timers.values())if(timer.delay===5000)timer.fn();}
  assert.equal(active.reads.length,3);assert.equal(active.document.title,'? example','cached markers survive hiding');
  assert.equal(documents.reduce((total,h)=>total+h.reads.length,0),3);
});

const browserId = 'd'.repeat(64), browserKey = 'workspace-browser-notifications.v1', cleanupKey = `${browserKey}.cleanup`;
const endpoint = 'https://fcm.googleapis.com/private-endpoint?exact=value';
const endpointDigest = require('node:crypto').createHash('sha256').update(endpoint).digest('hex');
function notificationLocks(state) {
  return state.actionLocks ||= {request(_name, work) {
    const next = (state.actionTail || Promise.resolve()).then(work);
    state.actionTail = next.catch(()=>{}); return next;
  }};
}
async function notificationHarness(state, storage = new Map()) {
  const gateStorage = state.gateStorage ||= testStoreHarness();
  const controls = Object.fromEntries(['status', 'enable', 'disable', 'test'].map(name => [name, {disabled: false, addEventListener(type, callback) {this[type] = callback;}}]));
  const menu = Object.fromEntries(['delivery', 'menu-error'].map(name => [name, {disabled: true, textContent: '', addEventListener(type, callback) {this[type] = callback;}}]));
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
  registration.active = {state:'activated',postMessage(_message,ports){ports[0].reply({version:1,deliveryEpoch:state.oldWorker?undefined:1});}};
  registration.update = async () => {};
  const service = lifecycleTarget({controller: registration.active, getRegistration: async () => registration,
    register: async () => {state.onWorkerRegister?.(); return registration;}, ready: Promise.resolve(registration)});
  const addListener = service.addEventListener;
  service.addEventListener = (name, callback) => {addListener(name, callback); listeners[name] = event => service.dispatch(name, event);};
  const context = {document: {visibilityState: 'visible', addEventListener: (name, callback) => listeners[name] = callback,
    querySelectorAll: selector => [controls[selector.slice(19, -1)] || menu[selector.slice(19, -1)]].filter(Boolean), getElementById: () => null},
    addEventListener: (name, callback) => listeners[name] = callback,
    navigator: {serviceWorker: service,locks:state.noLocks?undefined:notificationLocks(state)}, workspaceAttentionStore:gateStorage.store,
    MessageChannel:function(){this.port1={close(){}};this.port2={reply:value=>queueMicrotask(()=>this.port1.onmessage({data:value}))};},
    Notification: {permission: 'granted', requestPermission: async () => {state.permissionCalls = (state.permissionCalls || 0) + 1; return 'granted';}},
    PushManager: function() {}, isSecureContext: true, AbortController, setTimeout, clearTimeout, atob, Uint8Array, TextEncoder,
    crypto: require('node:crypto').webcrypto,
    localStorage: {getItem: key => storage.get(key) || null, setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key)},
    fetch: async (url, options = {}) => {
      const body = options.body && JSON.parse(options.body); calls.push({url, body, savedId: storage.get(browserKey)});
      if (url.endsWith('/register')) {
        if (state.registerResponse) return state.registerResponse(body);
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
      return {ok: true, json: async () => ({available: true, deliveryEpoch:state.oldServer?undefined:1, publicKey: 'Ag', registered: state.serverRecords ? state.serverRecords.has(queriedId) : state.registered && queriedId === browserId})};
    },
  };
  vm.runInNewContext(source('browser-notifications.js'), context);
  for (let i = 0; i < 100; i++) await Promise.resolve();
  const flush = async () => {for (let i = 0; i < 300; i++) await Promise.resolve();};
  return {controls, menu, calls, storage, listeners, context, registration, flush,gateStorage};
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
    const state = {subscribed: true, registered: true, replacementId, failSelector,
      serverRecords: new Map([[browserId, 'https://fcm.googleapis.com/old-endpoint'],[replacementId,endpoint]]), outbox: new Set([browserId,replacementId])};
    const h = await notificationHarness(state, new Map([[browserKey, browserId]]));
    assert.equal(h.storage.get(browserKey), browserId, 'retained lost reply leaves a stale saved ID');
    assert.equal(state.subscribed, true); assert.equal(state.serverRecords.size, 2); assert.equal(state.outbox.size, 2);
    await h.controls.disable.click();
    assert.equal(state.subscribed, false, 'one failed deletion cannot block local unsubscribe');
    const deletions = h.calls.filter(call => call.url.endsWith('/disable'));
    assert.equal(deletions.length, 2, 'fixed current digest plus saved ID bound');
    assert.deepEqual(deletions.map(call => Object.keys(call.body)[0]).sort(), ['endpointDigest', 'registrationId']);
    assert.equal(h.calls.filter(call => call.url.endsWith('/register')).length, 0, 'Disable does not register');
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
    assert.equal(state.subscribed, !failure, 'uncertain old cleanup blocks subscription creation');
    if (failure) {
      assert.equal(h.storage.get(browserKey), browserId, 'unknown deletion must retain the old ID');
      assert.deepEqual(JSON.parse(h.storage.get(cleanupKey)), {registrationId:browserId});
      assert.equal(h.controls.status.textContent, 'Disabling is incomplete. Retry Disable.');
      assert.equal(h.controls.disable.disabled, false); assert.equal(h.controls.enable.disabled, true);
      assert.equal(state.serverRecords.has(replacementId), false); assert.equal(state.outbox.has(replacementId), false);
      assert.equal(state.serverRecords.has(browserId), failure === 'failed');
      const reloaded = await notificationHarness(state, h.storage);
      assert.equal(reloaded.calls.some(call => call.body), false, 'reload preserves obligations without deletion');
      state.failSelector = null;
      await reloaded.controls.disable.click();
      assert.equal(reloaded.calls.filter(call => call.url.endsWith('/disable')).length, 1);
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
    const before = structuredClone(records);
    const tx = {abort() {state.aborts = (state.aborts || 0) + 1; records.clear(); before.forEach((v,k) => records.set(k,v)); tx.aborted = true; tx.onabort?.();}}; let pending = 0;
    const complete = () => {
      if (tx.aborted) return;
      if (state.commitFailure) {tx.abort(); return;}
      tx.oncomplete?.();
    };
    const request = action => {
      const req = {}; pending++;
      queueMicrotask(() => {
        if (tx.aborted) return;
        if (state.readFailure) {req.onerror?.(); tx.abort(); return;}
        req.result = action(); req.onsuccess?.();
        if (--pending === 0) queueMicrotask(() => state.holdCommit ? state.completion = complete : complete());
      });
      return req;
    };
    tx.objectStore = () => ({get: key => request(() => structuredClone(records.get(key))),
      put: value => request(() => records.set(value.identity, structuredClone(value))), getAll: () => request(() => [...records.values()].map(value => structuredClone(value))),
      delete: key => records.delete(key)});
    return tx;
  }};
  const context = {indexedDB: {open(name, version) {
    assert.equal(name, 'workspace-session-attention'); versions.push(version);
    const req = {}; queueMicrotask(() => {req.result = database; req.onsuccess();}); return req;
  }}, Date, setTimeout, clearTimeout};
  vm.runInNewContext(source('session-attention-store.js'), context);
  return {store: context.workspaceAttentionStore, records, state, versions,reload(){vm.runInNewContext(source('session-attention-store.js'),context);return context.workspaceAttentionStore;}};
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
    assert.deepEqual(JSON.parse(JSON.stringify(h.capabilities())), {version:1,deliveryEpoch:1});
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

function lifecycleTarget(fields = {}) {
  const handlers = new Map();
  return {...fields, addEventListener(name, callback) {if (!handlers.has(name)) handlers.set(name, new Set()); handlers.get(name).add(callback);},
    removeEventListener(name, callback) {handlers.get(name)?.delete(callback);},
    dispatch(name, event) {[...handlers.get(name) || []].forEach(callback => callback(event));},
    listenerCount() {return [...handlers.values()].reduce((sum, set) => sum + set.size, 0);}};
}
async function feedbackHarness(storage = testStoreHarness(), options = {}) {
  const controls = Object.fromEntries(['status','enable','disable','test','test-status','provider-status','worker-status','sound','sound-status'].map(name => [name,
    {disabled: false, textContent: '', addEventListener(type, callback) {this[type] = callback;}}]));
  const menuControls = options.menu ? Object.fromEntries(['delivery', 'sound', 'menu-error'].map(name => [name,
    {disabled: true, textContent: '', addEventListener(type, callback) {this[type] = callback;}}])) : {};
  const listeners = {}, timers = new Map(), calls = [], saved = new Map(options.off ? [] : [[browserKey, browserId]]);
  let now = Date.now(), nextTimer = 0;
  const state = {descriptor: testDescriptor(now), registered: !options.off, subscribed: !options.off, available: options.available ?? true, workerVersion: options.workerVersion ?? 1, update: options.update, noReply: options.noReply};
  const subscription = {endpoint, options: {}, toJSON: () => ({endpoint, keys: {p256dh: 'public', auth: 'auth'}}), unsubscribe: async () => {state.subscribed = false; return true;}};
  const registration = lifecycleTarget({update: async () => {state.updateChecks = (state.updateChecks || 0) + 1; if (state.update) await state.update();},
    active: lifecycleTarget({state: 'activated', postMessage(_message, ports) {state.probes = (state.probes || 0) + 1; if (!state.noReply) ports[0].reply({version: state.workerVersion,deliveryEpoch:1});}}),
    pushManager: {getSubscription: async () => state.subscribed ? subscription : null,
      subscribe: async () => {state.subscribes = (state.subscribes || 0) + 1; state.subscribed = true; return subscription;}}});
  if (options.waitingWorker) registration.waiting = lifecycleTarget({state: 'installed'});
  const service = lifecycleTarget({getRegistration: async () => options.initialLookup ? options.initialLookup(registration) : options.noRegistration ? null : registration, controller: options.uncontrolled ? null : registration.active,
    register: async () => {state.registrations = (state.registrations || 0) + 1; return registration;}, ready: Promise.resolve(registration)});
  const addServiceListener = service.addEventListener;
  service.addEventListener = (name, callback) => {addServiceListener(name, callback); listeners[name] = event => service.dispatch(name, event);};
  const context = {document: {visibilityState: 'visible', getElementById: () => ({querySelector: selector => controls[selector.slice(19, -1) || 'status']}),
    addEventListener: (name, callback) => listeners[name] = callback}, navigator: {serviceWorker: service,locks:notificationLocks(state)}, addEventListener: (name, callback) => listeners[name] = callback,
    Date: class extends Date {static now() {return now;}}, Notification: {permission: options.permission || 'granted', requestPermission: async () => {state.permissions = (state.permissions || 0) + 1; if (state.permissionReply) return state.permissionReply(); return 'granted';}, prototype: options.noSilent ? {} : {silent: false}}, PushManager: function() {}, isSecureContext: options.unsupported ? false : true,
    workspaceAttentionStore: options.oldStore ? {} : storage.store, AbortController, Uint8Array, TextEncoder, atob, crypto: require('node:crypto').webcrypto,
    MessageChannel: function() {this.port1 = {close() {}}; this.port2 = {reply: value => queueMicrotask(() => this.port1.onmessage({data: value}))};},
    setTimeout(callback, delay) {const id = ++nextTimer; timers.set(id, {callback, at: now + delay}); return id;}, clearTimeout: id => timers.delete(id),
    localStorage: {getItem: key => saved.get(key) || null, setItem: (key,value) => saved.set(key,value), removeItem: key => saved.delete(key)},
    fetch: async (url, config = {}) => {calls.push({url, config});
      if (url.endsWith('/test') && state.testReply) return state.testReply();
      if (url.endsWith('/disable')) state.registered = false;
      if (url.endsWith('/register')) {state.registered = true; return {ok: true, json: async () => ({registrationId: browserId})};}
      return {ok: true, json: async () => url.endsWith('/test') ? {ok: true, test: options.oldServer ? null : state.descriptor} : {available: state.available, deliveryEpoch:1, registered: state.registered, publicKey: 'Ag', result: 'provider_accepted'}};},
  };
  context.document.querySelectorAll = selector => {
    const name = selector.slice(19, -1);
    return [options.menuOnly ? null : controls[name], menuControls[name]].filter(Boolean);
  };
  context.document.getElementById = name => name === 'sidebar-menu' ? {addEventListener: (_name, callback) => listeners.menutoggle = callback} : null;
  vm.runInNewContext(source('browser-notifications.js'), context);
  const flush = async () => {for (let i = 0; i < 300; i++) await Promise.resolve();};
  await flush();
  return {controls: options.menuOnly ? menuControls : controls, menuControls, storage, state, timers, calls, context, listeners, registration, service, async advance(ms) {now += ms;
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
    const old = await feedbackHarness(testStoreHarness(), options);
    if (options.oldStore) {assert.equal(old.controls.test.disabled,true); continue;}
    await old.controls.test.click();
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
    if (options.waitingWorker) {assert.match(old.controls['worker-status'].textContent, /keep your tabs open/); await old.advance(10001);}
    assert.match(old.controls['worker-status'].textContent, options.waitingWorker ? /could not be confirmed/ : /Detailed test results are unavailable/);
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

test('worker install skips waiting and activation claims documents without touching delivery or state', async () => {
  const h = workerHarness();
  await h.lifecycle('install'); await h.lifecycle('activate');
  assert.deepEqual(h.calls, [{skipWaiting: true}, {claim: true}]);
  assert.equal(h.notifications.length, 0); assert.equal(h.records.size, 0);
});

test('waiting predecessor is replaced and capability is checked only after this document gets the successor controller', async () => {
  const h = await feedbackHarness(testStoreHarness(), {waitingWorker: true});
  assert.equal(h.state.updateChecks, 1, 'same root registration update fetches a successor');
  assert.equal(h.state.probes || 0, 0, 'old active capability cannot certify a waiting update');
  assert.match(h.controls['worker-status'].textContent, /keep your tabs open/);
  let probes = 0;
  const successor = lifecycleTarget({state: 'installing', postMessage(_message, ports) {probes++; ports[0].reply({version: 1});}});
  h.registration.waiting = null; h.registration.installing = successor; h.registration.dispatch('updatefound');
  successor.state = 'activated'; h.registration.installing = null; h.registration.active = successor; successor.dispatch('statechange');
  await h.flush(); assert.equal(probes, 0, 'registration.active is insufficient while document controller is old');
  h.service.controller = successor; h.service.dispatch('controllerchange'); await h.flush();
  assert.equal(probes, 1); assert.equal(h.controls['worker-status'].textContent, '');
  assert.equal(h.registration.listenerCount(), 0); assert.equal(successor.listenerCount(), 0); assert.equal(h.timers.size, 0);
  assert.equal(h.calls.some(call => call.config.method === 'POST'), false, 'update makes no subscription or permission mutation');
});

test('worker transition deadlines and failures keep diagnostics limited and remove observers', async () => {
  for (const options of [{waitingWorker: true}, {uncontrolled: true}, {update: () => new Promise(() => {})},
    {update: async () => {throw Error('update failed');}}, {noReply: true}]) {
    const h = await feedbackHarness(testStoreHarness(), options);
    await h.advance(10001);
    assert.match(h.controls['worker-status'].textContent, /could not be confirmed|Detailed test results are unavailable/);
    assert.doesNotMatch(h.controls['worker-status'].textContent, /Close.*tabs/);
    assert.equal(h.registration.listenerCount(), 0); assert.equal(h.registration.waiting?.listenerCount() || 0, 0);
    assert.equal(h.registration.active.listenerCount(), 0); assert.equal(h.timers.size, 0);
    assert.equal(h.state.updateChecks, 1, 'no repeated update loop');
    assert.equal(h.calls.some(call => call.config.method === 'POST'), false);
  }
});

test('stale worker readiness cannot overwrite a newer refresh, action or page retirement', async () => {
  for (const invalidation of ['refresh', 'test', 'pagehide', 'revoke']) {
    const h = await feedbackHarness(testStoreHarness(), {waitingWorker: true});
    const predecessor = h.registration.waiting;
    if (invalidation === 'refresh') h.service.dispatch('controllerchange');
    else if (invalidation === 'test') await h.controls.test.click();
    else if (invalidation === 'pagehide') h.listeners.pagehide();
    else h.listeners.storage({key: browserKey, newValue: null});
    await h.flush();
    const status = h.controls['worker-status'].textContent;
    h.registration.waiting = null; predecessor.state = 'redundant'; predecessor.dispatch('statechange'); await h.flush();
    if (invalidation !== 'refresh') {
      assert.equal(h.controls['worker-status'].textContent, status);
      assert.equal(h.registration.listenerCount(), 0);
    } else {
      // The replacement observer may complete; the canceled observer's terminal
      // state cannot publish a timeout over its confirmed controller result.
      h.service.dispatch('controllerchange'); await h.flush();
      assert.equal(h.controls['worker-status'].textContent, '');
    }
    h.listeners.pagehide(); await h.flush(); assert.equal(h.timers.size, 0);
  }
});

test('a new refresh waits for the already-started successor fetch before trusting the old controller', async () => {
  let release;
  const h = await feedbackHarness(testStoreHarness(), {update: () => new Promise(resolve => {release = resolve;})});
  h.service.dispatch('controllerchange'); await h.flush();
  assert.equal(h.state.updateChecks, 1, 'refresh shares the one current document update check');
  assert.equal(h.state.probes || 0, 0, 'old capable controller is not proof while successor fetch is outstanding');
  release(); await h.flush();
  assert.equal(h.state.probes, 1); assert.equal(h.controls['worker-status'].textContent, '');
  assert.equal(h.timers.size, 0);
});

test('Test and Disable retire readiness progress without a lingering Updating status past its deadline', async () => {
  for (const waitingWorker of [true, false]) for (const action of ['test', 'disable']) {
    const h = await feedbackHarness(testStoreHarness(), {waitingWorker, noReply: !waitingWorker});
    assert.match(h.controls['worker-status'].textContent, /^Updating browser notifications/);
    await h.controls[action].click(); await h.flush();
    assert.equal(h.controls['worker-status'].textContent,
      'The notification update could not be confirmed. Refresh this page to try again. Test results may be limited.');
    await h.advance(10001);
    assert.doesNotMatch(h.controls['worker-status'].textContent, /Updating/);
    assert.match(h.controls['worker-status'].textContent, /could not be confirmed/);
    assert.equal(h.registration.listenerCount(), 0);
    assert.equal(h.registration.waiting?.listenerCount() || 0, 0);
    assert.equal(h.state.updateChecks, 1, 'action cancellation does not start an update loop');
    h.listeners.pagehide(); await h.flush(); assert.equal(h.timers.size, 0);
  }
});


const soundIdentity = 'browser-notification-sound';
const flushStorage = async () => {for (let i = 0; i < 80; i++) await Promise.resolve();};
test('sound singleton defaults only on absence, rejects corrupt data and survives attention pruning', async () => {
  const h = testStoreHarness();
  assert.equal(await h.store.readSound(), true);
  for (const choice of [false, true]) {
    assert.equal(await h.store.writeSound(choice), choice);
    assert.equal(await h.store.readSound(), choice);
    assert.deepEqual(h.records.get(soundIdentity), {identity: soundIdentity, playSound: choice, at: Number.MAX_SAFE_INTEGER});
  }
  for (const corrupt of [null, {identity: soundIdentity}, {identity: soundIdentity, playSound: 'true'},
    {identity: soundIdentity, playSound: true, at: 0}, {identity: soundIdentity, playSound: true, at: Number.MAX_SAFE_INTEGER - 1},
    {identity: soundIdentity, playSound: true, at: Number.MAX_SAFE_INTEGER, extra: false}]) {
    h.records.set(soundIdentity, corrupt);
    await assert.rejects(h.store.readSound());
  }
  await assert.rejects(h.store.writeSound('true'));
  await assert.rejects(h.store.update(soundIdentity));
  await h.store.writeSound(false);
  for (let i = 0; i < 514; i++) await h.store.update(`attention-${i}`);
  assert.equal(h.records.size, 513, 'preference is outside the 512 attention-record budget');
  assert.equal(await h.store.readSound(), false);
  // Exercise the exact deployed predecessor pruning algorithm, with valid
  // attention keys preceding the reserved singleton in IndexedDB key order.
  const old = testStoreHarness(); await old.store.writeSound(false);
  for (let i = 0; i < 520; i++) old.records.set(i.toString(16).padStart(64, '0'), {identity: i.toString(16).padStart(64, '0'), at: Date.now() + i});
  const all = [...old.records.values()].sort((a,b) => a.identity.localeCompare(b.identity));
  assert.equal(all.at(-1).identity, soundIdentity);
  all.sort((a,b) => b.at-a.at).slice(512).forEach(item => old.records.delete(item.identity));
  assert.equal(old.records.size, 512); assert.equal(await old.store.readSound(), false);
  assert.deepEqual(h.versions, [1]);
});

test('sound storage waits for commit and bounds read, open and aborted write failures', async () => {
  const h = testStoreHarness(); await h.store.writeSound(true);
  h.state.holdCommit = true;
  let settled = false;
  const write = h.store.writeSound(false).then(value => {settled = true; return value;});
  await flushStorage(); assert.equal(settled, false);
  h.state.completion(); assert.equal(await write, false);
  const read = h.store.readSound().then(value => {settled = true; return value;}); settled = false;
  await flushStorage(); assert.equal(settled, false, 'request success is not transaction success');
  h.state.completion(); assert.equal(await read, false);
  await assert.rejects(h.store.writeSound(true));
  assert.equal(h.state.aborts, 1); assert.equal(h.records.get(soundIdentity).playSound, false);
  await assert.rejects(h.store.readSound());
  h.state.holdCommit = false; h.state.commitFailure = true;
  await assert.rejects(h.store.writeSound(true));
  h.state.commitFailure = false; assert.equal(await h.store.readSound(), false);
  h.state.readFailure = true; await assert.rejects(h.store.readSound());
  h.state.readFailure = false; h.state.failed = true; await assert.rejects(h.store.readSound());
  const context = {setTimeout, clearTimeout, indexedDB: {open: () => ({})}};
  vm.runInNewContext(source('session-attention-store.js'), context);
  await assert.rejects(context.workspaceAttentionStore.readSound(), 'opening storage is bounded too');
});

test('one sound snapshot covers trusted, neutral, inaccessible and malformed push displays exactly', async () => {
  for (const choice of [true, false, 'failed']) for (const branch of ['trusted', 'neutral', 'inaccessible', 'malformed']) {
    const h = workerHarness(); h.sound.value = choice; h.sound.failed = choice === 'failed';
    if (branch === 'neutral') h.status({fresh: false});
    if (branch === 'inaccessible') h.inaccessible();
    await h.push(branch === 'malformed' ? null : event());
    assert.equal(h.notifications.length, 1); assert.equal(h.soundReads(), 1);
    if (choice === true) assert.equal(Object.hasOwn(h.notifications[0], 'silent'), false, 'native defaults omit silent');
    else assert.equal(h.notifications[0].silent, true);
    assert.equal(h.notifications[0].renotify, false);
  }
});

test('Test sound options persist without tabs; failed reads still display and rejected silent calls never retry audibly', async () => {
  for (const choice of [true, false, 'failed', 'corrupt', 'stalled']) {
    const h = displayHarness();
    if (typeof choice === 'boolean') await h.storage.store.writeSound(choice);
    else if (choice === 'corrupt') h.storage.records.set(soundIdentity, {identity: soundIdentity, playSound: 1});
    else h.storage.store.readSound = () => choice === 'stalled' ? new Promise(() => {}) : Promise.reject(Error('private sound canary'));
    await h.push(testDescriptor());
    assert.equal(h.shown.length, 1);
    assert.equal(Object.hasOwn(h.shown[0], 'silent'), choice !== true);
    if (choice !== true) assert.equal(h.shown[0].silent, true);
  }
  const h = displayHarness(); await h.storage.store.writeSound(false); h.state.failure = {name: 'NotSupportedError'};
  await h.push(testDescriptor());
  assert.equal(h.shown.length, 1); assert.equal(h.shown[0].silent, true);
  assert.equal(h.storage.records.get(testDescriptor().identity).testOutcome.display, 'rejected');
});

test('shared sound control persists across documents and Settings revisits without changing registration', async () => {
  const storage = testStoreHarness(), first = await feedbackHarness(storage);
  assert.equal(first.controls.sound.checked, true); assert.equal(first.controls.sound.disabled, false);
  first.controls.sound.checked = false; await first.controls.sound.change();
  assert.equal(await storage.store.readSound(), false);
  const reopened = await feedbackHarness(storage);
  assert.equal(reopened.controls.sound.checked, false);
  reopened.controls.sound.checked = true; await reopened.controls.sound.change();
  first.listeners['session-section-change']({detail: 'settings'}); await first.flush();
  assert.equal(first.controls.sound.checked, true, 'revisit reads the last committed cross-document choice');
  assert.equal(first.calls.some(call => call.config.method === 'POST'), false);
  assert.equal(reopened.calls.some(call => call.config.method === 'POST'), false);
  await first.controls.disable.click(); assert.equal(await storage.store.readSound(), true, 'Disable retains sound preference');
  const unsupported = await feedbackHarness(storage, {noSilent: true});
  assert.equal(unsupported.controls.sound.disabled, true);
  assert.match(unsupported.controls['sound-status'].textContent, /does not support visual-only/);
  const limited = await feedbackHarness(storage, {waitingWorker: true});
  assert.match(limited.controls['sound-status'].textContent, /sound setting may not take effect/);
});

test('sound UI commits before presenting a selection, reconciles save failure and ignores stale reads', async () => {
  const storage = testStoreHarness(), h = await feedbackHarness(storage);
  storage.state.holdCommit = true;
  h.controls.sound.checked = false; const saving = h.controls.sound.change(); await h.flush();
  assert.equal(h.controls.sound.checked, true, 'attempt is not shown as committed');
  assert.equal(h.controls.sound.disabled, true);
  storage.state.completion(); await saving;
  assert.equal(h.controls.sound.checked, false);
  storage.state.holdCommit = false; storage.state.commitFailure = true;
  h.controls.sound.checked = true; await h.controls.sound.change();
  assert.equal(h.controls.sound.checked, false);
  assert.match(h.controls['sound-status'].textContent, /could not be saved/);
  storage.state.commitFailure = false;
  const original = storage.store.readSound; let release;
  storage.store.readSound = () => new Promise(resolve => {release = () => resolve(true);});
  h.listeners['session-section-change']({detail: 'settings'}); await h.flush();
  storage.store.readSound = original;
  h.controls.sound.checked = false; await h.controls.sound.change(); release(); await h.flush();
  assert.equal(h.controls.sound.checked, false, 'old read cannot overwrite a committed newer choice');
  storage.state.failed = true;
  h.listeners.pageshow(); await h.flush();
  assert.equal(h.controls.sound.disabled, true);
  assert.match(h.controls['sound-status'].textContent, /could not be read.*without sound/);
});


test('sound is sampled at the native attempt after a delayed status or Test receipt', async () => {
  const session = workerHarness(); let releaseStatus;
  session.context.fetch = () => new Promise(resolve => {releaseStatus = () => resolve({ok: true, status: 200, json: async () => ({fresh: true, enabled: true, identity, event: event(), revision: 1})});});
  const work = session.push(event()); await flushStorage();
  assert.equal(session.soundReads(), 0, 'status observation precedes preference snapshot');
  session.sound.value = false; releaseStatus(); await work;
  assert.equal(session.notifications[0].silent, true); assert.equal(session.soundReads(), 1);
  const h = displayHarness(); await h.storage.store.writeSound(true);
  const save = h.storage.store.saveTest; let releaseReceipt;
  h.storage.store.saveTest = (descriptor, outcome) => outcome.display === 'pending' ? new Promise(resolve => {releaseReceipt = () => resolve(save(descriptor, outcome));}) : save(descriptor, outcome);
  const push = h.push(testDescriptor()); await flushStorage();
  await h.storage.store.writeSound(false); releaseReceipt(); await push;
  assert.equal(h.shown.length, 1); assert.equal(h.shown[0].silent, true);
});

test('menu-only owner reads status on load/open without permission, subscription, Test or repair mutations', async () => {
  const storage = testStoreHarness(); await storage.store.saveTest(testDescriptor());
  const h = await feedbackHarness(storage, {menu: true, menuOnly: true});
  assert.equal(h.controls.test, undefined, 'full panel and Test may be absent');
  assert.equal(h.calls.every(call => !call.config.method), true);
  h.listeners.menutoggle({newState: 'open'}); await h.flush();
  h.listeners.visibilitychange(); await h.flush();
  assert.equal(h.calls.every(call => !call.config.method), true);
  assert.equal(h.state.permissions || 0, 0); assert.equal(h.state.registrations || 0, 0); assert.equal(h.state.subscribes || 0, 0);
  assert.equal(h.timers.size, 0, 'menu does not resume panel Test polling');
  storage.state.failed = true;
  h.listeners.menutoggle({newState: 'open'}); await h.flush();
  assert.equal(h.controls['menu-error'].textContent, 'Could not check notifications.');
  assert.equal(h.controls.sound.disabled, true);
});

test('menu and panel share committed sound, action locks, cleanup and saved preference across delivery off/on', {timeout: 2000}, async () => {
  const h = await feedbackHarness(testStoreHarness(), {menu: true}), menu = h.menuControls;
  menu.sound.checked = false; await menu.sound.change();
  assert.equal(menu.sound.checked, false); assert.equal(h.controls.sound.checked, false);
  h.storage.state.holdCommit = true;
  h.controls.sound.checked = true; const saving = h.controls.sound.change(); await h.flush();
  assert.equal(menu.sound.disabled, true); assert.equal(menu.delivery.disabled, true); assert.equal(h.controls.test.disabled, true);
  const before = h.calls.length; menu.delivery.checked = false; await menu.delivery.change(); assert.equal(h.calls.length, before, 'competing actions ignored during sound commit');
  h.storage.state.completion(); await saving; h.storage.state.holdCommit = false;
  assert.equal(menu.sound.checked, true); assert.equal(h.controls.sound.checked, true);
  menu.sound.checked = false; await menu.sound.change();
  const releases = []; let started;
  const startedDeleting = new Promise(resolve => {started = resolve;});
  const fetch = h.context.fetch;
  h.context.fetch = async (...args) => args[0].endsWith('/disable') ? new Promise(resolve => {
    releases.push(async () => resolve(await fetch(...args))); if (releases.length === 2) started();
  }) : fetch(...args);
  menu.delivery.checked = false; const disabling = menu.delivery.change(); await startedDeleting;
  assert.equal(h.controls.enable.disabled, true); assert.equal(menu.sound.disabled, true);
  await h.controls.disable.click(); menu.delivery.checked = true; await menu.delivery.change();
  await Promise.all(releases.map(release => release())); await disabling;
  assert.match(h.controls.status.textContent, /^Disabled on this browser/); assert.equal(menu.delivery.checked, false); assert.equal(menu.delivery.indeterminate, false);
  assert.equal(await h.storage.store.readSound(), false);
  h.context.fetch = fetch;
  menu.delivery.checked = true; await menu.delivery.change();
  assert.match(h.controls.status.textContent, /^Enabled on this browser/); assert.equal(menu.delivery.checked, true);
  assert.equal(h.state.permissions, 1); assert.equal(h.state.registrations, 1); assert.equal(h.state.subscribes, 1);
  assert.equal(await h.storage.store.readSound(), false);
});

test('cross-tab registration or cleanup revocation replaces every Enabled status and retired menu reads stay fenced', async () => {
  for (const invalidation of [{key: browserKey, newValue: null}, {key: browserKey, newValue: 'f'.repeat(64)},
    {key: cleanupKey, newValue: JSON.stringify({endpointDigest})}]) for (const rejects of [false, true]) {
    const h = await feedbackHarness(testStoreHarness(), {menu: true});
    await h.controls.test.click(); await h.advance(2000);
    assert.equal(h.controls.status.textContent, 'Enabled on this browser.');
    assert.equal(h.menuControls.delivery.checked, true);
    let finish, reads = 0;
    h.context.fetch = () => {reads++; return new Promise((resolve, reject) => {finish = () => rejects ? reject(Error('retired read failed')) :
      resolve({ok: true, json: async () => ({available: true, registered: true})});});};
    h.listeners.controllerchange(); await h.flush();
    assert.equal(reads, 1, 'a status observation is pending before revocation');
    h.listeners.storage(invalidation); await h.flush();
    const expected = 'Browser setup is incomplete. Disable it or enable notifications again.';
    assert.equal(h.controls.status.textContent, expected); assert.equal(h.menuControls['menu-error'].textContent, 'Notification setup needs attention.'); assert.equal(h.menuControls.delivery.indeterminate, true);
    assert.equal(h.controls.test.disabled, true); assert.equal(h.controls.disable.disabled, false); assert.equal(h.menuControls.delivery.disabled, true);
    assert.equal(h.controls['test-status'].textContent, '');
    assert.equal(h.storage.records.get(h.state.descriptor.identity).testPending, undefined, 'revocation still cancels the exact saved check');
    h.listeners.menutoggle({newState: 'open'}); h.listeners.visibilitychange(); h.listeners.pageshow(); await h.flush();
    assert.equal(reads, 1, 'opening the menu cannot restart a retired observation');
    finish(); await h.flush(); await h.advance(20000);
    assert.equal(h.controls.status.textContent, expected); assert.equal(h.menuControls['menu-error'].textContent, 'Notification setup needs attention.'); assert.equal(h.menuControls.delivery.indeterminate, true);
    assert.equal(reads, 1, 'late old completion cannot refresh, repair or resume');
  }
});

test('global delivery off and silent preference do not stop actual title attention, reservations or focused acknowledgement', async () => {
  const h = await feedbackHarness(testStoreHarness(), {menu: true, menuOnly: true});
  h.controls.sound.checked = false; await h.controls.sound.change(); h.controls.delivery.checked = false; await h.controls.delivery.change();
  assert.equal(h.controls.delivery.checked, false); assert.equal(h.controls.delivery.indeterminate, false);
  let current = event(), focus = false, refresh;
  const listeners = {}, acknowledgements = [];
  h.context.document.body = {dataset: {session: 'example', sessionObservation: identity, rootThreadId: 'thread-1'}};
  h.context.document.title = 'example · Workspace'; h.context.document.hasFocus = () => focus;
  h.context.document.addEventListener = (name, callback) => listeners[name] = callback;
  h.context.window = {addEventListener: (name, callback) => listeners[name] = callback};
  h.context.navigator.serviceWorker.getRegistration = async () => ({active: {postMessage: message => acknowledgements.push(message)}});
  h.context.setTimeout = (callback, delay) => {if (delay === 5000) refresh = callback; return 1;};
  h.context.fetch = async (url, options = {}) => {
    assert.equal(options.method, undefined, 'off browser never publishes presence or mutates session mute');
    return {ok: true, json: async () => url.endsWith('/attention') ? {version: 1, identity, rootThread: 'thread-1', event: current, revision: current.revision, fresh: true, enabled: true} :
      {reservationHolders: [{slug: 'example', observationIdentity: identity, machines: [{machine: 'one', state: 'recovery_hold'}, {machine: 'two', state: 'deployed_review'}]}]}};
  };
  vm.runInNewContext(source('session-attention.js'), h.context); await h.flush();
  assert.equal(h.context.document.title, '? R2 example · Workspace');
  current = {...event(2, 'done'), id: 'e'.repeat(64)}; refresh(); await h.flush();
  assert.equal(h.context.document.title, '✓ R2 example · Workspace');
  assert.equal(h.storage.records.get(identity).seen.length, 0);
  focus = true; listeners.focus(); await h.flush();
  assert.equal(h.context.document.title, 'R2 example · Workspace');
  assert.equal(h.storage.records.get(identity).seen.includes(current.id), true);
  assert.equal(acknowledgements[0].identity, identity);
  assert.equal(await h.storage.store.readSound(), false);
});


test('matching session click focuses once without navigating, opening, changing member or losing document state', async () => {
  for (const path of ['/example', '/example/']) for (const view of ['?member=reviewer0#settings', '#repositories', '?member=implementer0#codex']) {
    const h = workerHarness(); let release, focusCalls = 0, navigateCalls = 0, completed = false;
    const client = {url: `https://workspace.example${path}${view}`, draft: 'unsent draft',
      focus: () => {focusCalls++; return new Promise(resolve => {release = resolve;});},
      navigate: async () => {navigateCalls++; throw Error('must never navigate a retained document');}};
    h.windows.push(client);
    const click = h.click('/example/?member=implementer0#codex', identity).then(() => {completed = true;});
    await flushStorage();
    assert.equal(focusCalls, 1); assert.equal(completed, false, 'click awaits the selected focus promise');
    assert.equal(navigateCalls, 0); assert.equal(h.calls.some(call => call.openWindow), false);
    release(client); await click;
    assert.equal(client.url, `https://workspace.example${path}${view}`); assert.equal(client.draft, 'unsent draft');
    assert.equal(h.calls.length, 0, 'focus adds no fetch, message, acknowledgement or open');
  }
});

test('click prefers the existing target-member view, otherwise retains the matching session view', async () => {
  for (const target of ['lead', 'implementer0']) {
    const h = workerHarness(), focused = [];
    const other = target === 'lead' ? 'implementer0' : 'lead';
    for (const member of [other, target]) h.windows.push({url: `https://workspace.example/example/${member === 'lead' ? '' : `?member=${member}`}#settings`,
      focus: async () => focused.push(member), navigate: () => assert.fail('navigation is forbidden')});
    await h.click(`/example/${target === 'lead' ? '' : `?member=${target}`}#codex`, identity);
    assert.deepEqual(focused, [target]); assert.equal(h.calls.length, 0);
  }
});

test('click matching keeps origin, strict session path and home boundaries and canonical no-match navigation', async () => {
  for (const path of ['https://foreign.example/example/', 'https://workspace.example/example-other/', 'https://workspace.example/example/child/',
    'https://workspace.example/example//', 'https://workspace.example/exam%70le/', 'https://workspace.example/']) {
    const h = workerHarness(); h.windows.push({url: path, focus: () => assert.fail('foreign/nonmatching view'), navigate: () => assert.fail('must not navigate')});
    await h.click('/example/?member=implementer0#codex', identity);
    assert.equal(h.calls.at(-1).openWindow, `https://workspace.example/example/?member=implementer0&attentionIdentity=${identity}#codex`);
  }
  const root = workerHarness(); root.windows.push({url: 'https://workspace.example/example/', focus: () => assert.fail('session is not home')});
  await root.click('/'); assert.equal(root.calls.at(-1).openWindow, 'https://workspace.example/');
  const home = workerHarness(); let focused = 0;
  home.windows.push({url: 'https://workspace.example/?view=other#settings', focus: async () => focused++});
  await home.click('/'); assert.equal(focused, 1); assert.equal(home.calls.length, 0);
  const invalid = workerHarness();
  for (const route of ['//foreign.example/example/', 'https://foreign.example/example/', '/example/child/', '/example//', '/example/?member=invalid#codex', '/example/#settings']) await invalid.click(route, identity);
  assert.equal(invalid.calls.length, 0);
});

test('rejected focus or a client closed after enumeration never retries by opening a duplicate tab', async () => {
  for (const name of ['InvalidAccessError', 'InvalidStateError']) {
    const h = workerHarness(); let focused = 0;
    h.windows.push({url: 'https://workspace.example/example/#settings', focus: async () => {focused++; throw {name};}, navigate: () => assert.fail('navigation forbidden')});
    await assert.rejects(h.click('/example/#codex', identity), error => error.name === name);
    assert.equal(focused, 1); assert.equal(h.calls.length, 0);
  }
});


test('compact menu initializes without sinks and distinguishes confirmed on/off, blocked and mixed observations', async () => {
  for (const [options, checked, mixed, disabled, message] of [
    [{}, true, false, false, ''],
    [{off: true, noRegistration: true}, false, false, false, ''],
    [{off: true, noRegistration: true, permission: 'denied'}, false, false, true, 'Notifications are blocked.'],
    [{off: true, noRegistration: true, available: false}, false, false, true, 'Notification delivery is unavailable.'],
    [{unsupported: true}, false, true, true, 'Notifications are unavailable here.'],
  ]) {
    const h = await feedbackHarness(testStoreHarness(), {...options, menu: true, menuOnly: true});
    assert.equal(h.controls.status, undefined); assert.equal(h.controls.enable, undefined); assert.equal(h.controls.disable, undefined);
    assert.equal(h.controls.delivery.checked, checked); assert.equal(h.controls.delivery.indeterminate, mixed); assert.equal(h.controls.delivery.disabled, disabled);
    assert.equal(h.controls['menu-error'].textContent, message); assert.equal(h.controls['menu-error'].hidden, !message);
    assert.equal(h.calls.some(call => call.config.method), false); assert.equal(h.state.permissions || 0, 0);
    if (options.off) assert.equal(h.controls.sound.disabled, false, 'Sound is independent of delivery');
  }
  let releaseInitial;
  const initial = await feedbackHarness(testStoreHarness(), {menu: true, menuOnly: true,
    initialLookup: registration => new Promise(resolve => {releaseInitial = () => resolve(registration);})});
  assert.equal(initial.controls.delivery.indeterminate, true); assert.equal(initial.controls.delivery.disabled, true);
  assert.equal(initial.controls['menu-error'].textContent, '', 'initial observation is not a failure');
  releaseInitial(); await initial.flush(); assert.equal(initial.controls.delivery.checked, true);
  const h = await feedbackHarness(testStoreHarness(), {menu: true, menuOnly: true});
  h.context.fetch = async () => {throw Error('private status canary');};
  h.listeners.menutoggle({newState: 'open'}); await h.flush();
  assert.equal(h.controls.delivery.indeterminate, true); assert.equal(h.controls.delivery.disabled, true);
  assert.equal(h.controls['menu-error'].textContent, 'Could not check notifications.');
  assert.equal(h.controls['menu-error'].textContent.includes('canary'), false);
});

test('compact toggle restores confirmed values immediately during delayed Disable and Enable and blocks competing actions', async () => {
  const h = await feedbackHarness(testStoreHarness(), {menu: true}), toggle = h.menuControls.delivery;
  const fetch = h.context.fetch, releases = []; let deleting;
  const started = new Promise(resolve => {deleting = resolve;});
  h.context.fetch = async (...args) => args[0].endsWith('/disable') ? new Promise(resolve => {
    releases.push(async () => resolve(await fetch(...args))); if (releases.length === 2) deleting();
  }) : fetch(...args);
  toggle.checked = false; const disabling = toggle.change();
  assert.equal(toggle.checked, true); assert.equal(toggle.disabled, true); assert.equal(toggle.indeterminate, false);
  assert.equal(h.controls.enable.disabled, true); assert.equal(h.menuControls.sound.disabled, true);
  toggle.checked = true; await toggle.change();
  await started; await Promise.all(releases.map(release => release())); await disabling;
  assert.equal(toggle.checked, false); assert.equal(toggle.disabled, false); assert.equal(toggle.indeterminate, false);
  assert.equal(h.menuControls['menu-error'].textContent, '');
  let permit;
  h.context.fetch = async (...args) => args[0].endsWith('/register') ? new Promise(resolve => {permit = async () => resolve(await fetch(...args));}) : fetch(...args);
  toggle.checked = true; const enabling = toggle.change(); await h.flush();
  assert.equal(toggle.checked, false); assert.equal(toggle.disabled, true); assert.equal(h.controls.disable.disabled, true);
  const count = h.state.permissions; await h.controls.enable.click(); assert.equal(h.state.permissions, count);
  await permit(); await enabling;
  assert.equal(toggle.checked, true); assert.equal(toggle.disabled, false); assert.equal(h.menuControls['menu-error'].textContent, '');
});

test('compact cleanup stays mixed for failed current-digest or old-ID removal and only full panel retry can confirm off', async () => {
  for (const failSelector of ['registrationId', 'endpointDigest']) {
    const replacementId = 'e'.repeat(64);
    const state = {subscribed: true, registered: true, replacementId, failSelector,
      serverRecords: new Map([[browserId, 'https://fcm.googleapis.com/old-endpoint'], [replacementId, endpoint]]), outbox: new Set([browserId, replacementId])};
    const h = await notificationHarness(state, new Map([[browserKey, browserId]]));
    h.menu.delivery.checked = false; await h.menu.delivery.change();
    assert.equal(state.subscribed, false); assert.equal(state.serverRecords.size, 1); assert.equal(state.outbox.size, 1);
    assert.equal(h.menu.delivery.indeterminate, true); assert.equal(h.menu.delivery.disabled, true);
    assert.equal(h.menu['menu-error'].textContent, 'Notification setup needs attention.');
    assert.equal(h.controls.disable.disabled, false, 'detailed recovery remains available');
    state.failSelector = null; await h.controls.disable.click();
    assert.equal(state.serverRecords.size, 0); assert.equal(state.outbox.size, 0);
    assert.equal(h.menu.delivery.checked, false); assert.equal(h.menu.delivery.indeterminate, false);
  }
  const h = await notificationHarness({subscribed: true, registered: true, localFailure: true}, new Map([[browserKey, browserId]]));
  h.menu.delivery.checked = false; await h.menu.delivery.change();
  assert.equal(h.menu.delivery.indeterminate, true); assert.equal(h.menu.delivery.disabled, true);
  assert.equal(h.controls.disable.disabled, false);
});

test('compact projection cannot enable from pending, contradictory, stale or retired observations', async () => {
  const h = await feedbackHarness(testStoreHarness(), {menu: true}), toggle = h.menuControls.delivery;
  const fetch = h.context.fetch, pending = [];
  h.context.fetch = () => new Promise((resolve, reject) => pending.push({resolve, reject}));
  h.listeners.menutoggle({newState: 'open'}); await h.flush();
  assert.equal(toggle.disabled, true, 'pending GET cannot authorize a stale action');
  const permissions = h.state.permissions || 0; toggle.checked = false; await toggle.change();
  assert.equal(toggle.checked, true); assert.equal(h.state.permissions || 0, permissions);
  h.listeners.menutoggle({newState: 'open'}); await h.flush();
  pending[1].resolve({ok: true, json: async () => ({available: true, registered: false})}); await h.flush();
  assert.equal(toggle.indeterminate, true, 'local subscription plus absent server registration is incomplete');
  assert.equal(h.menuControls['menu-error'].textContent, 'Notification setup needs attention.');
  pending[0].resolve({ok: true, json: async () => ({available: true, registered: true})}); await h.flush();
  assert.equal(toggle.indeterminate, true, 'old success cannot replace a newer contradictory observation');
  h.context.fetch = fetch;
  h.listeners.storage({key: browserKey, newValue: null}); await h.flush();
  h.listeners.menutoggle({newState: 'open'}); await h.flush();
  assert.equal(toggle.indeterminate, true); assert.equal(toggle.disabled, true);
});

test('compact notices use domain priority and clear only on that domain current success, without parsing detailed text', async () => {
  const h = await feedbackHarness(testStoreHarness(), {menu: true});
  h.storage.state.failed = true; h.listeners.menutoggle({newState: 'open'}); await h.flush();
  assert.equal(h.menuControls['menu-error'].textContent, 'Could not check notifications.');
  h.listeners.storage({key: browserKey, newValue: null}); await h.flush();
  assert.equal(h.menuControls['menu-error'].textContent, 'Notification setup needs attention.');
  h.storage.state.failed = false; h.menuControls.sound.checked = false; await h.menuControls.sound.change();
  assert.equal(h.menuControls['menu-error'].textContent, 'Notification setup needs attention.', 'sound success cannot erase delivery uncertainty');
  const clean = await feedbackHarness(testStoreHarness(), {menu: true});
  clean.storage.state.commitFailure = true; clean.menuControls.sound.checked = false; await clean.menuControls.sound.change();
  assert.equal(clean.menuControls['menu-error'].textContent, 'Could not save the sound setting.');
  clean.controls.status.textContent = 'arbitrary backend/private status';
  clean.listeners.controllerchange(); await clean.flush();
  assert.equal(clean.menuControls['menu-error'].textContent, 'Could not check notifications.', 'unreadable delivery gate takes priority over sound error');
  clean.storage.state.commitFailure = false; clean.listeners.controllerchange(); await clean.flush();
  clean.listeners['session-section-change']({detail: 'settings'}); await clean.flush();
  assert.equal(clean.menuControls['menu-error'].textContent, '');
  const worker = await feedbackHarness(testStoreHarness(), {menu: true, waitingWorker: true});
  assert.equal(worker.menuControls['menu-error'].textContent, '', 'in-progress activation is not an error');
  await worker.advance(10001);
  assert.equal(worker.menuControls['menu-error'].textContent, 'Notification update unconfirmed.');
  const unsupported = await feedbackHarness(testStoreHarness(), {menu: true, noSilent: true});
  assert.equal(unsupported.menuControls['menu-error'].textContent, 'Sound control is unavailable.');
});

test('late delivery action results cannot certify a retired compact checkbox or clear its error', async () => {
  const h = await feedbackHarness(testStoreHarness(), {menu: true, off: true}), toggle = h.menuControls.delivery;
  const fetch = h.context.fetch; let release;
  h.context.fetch = async (...args) => args[0].endsWith('/register') ? new Promise(resolve => {release = async () => resolve(await fetch(...args));}) : fetch(...args);
  toggle.checked = true; const action = toggle.change(); await h.flush();
  h.listeners.storage({key: browserKey, newValue: 'f'.repeat(64)}); await h.flush();
  await release(); await action; await h.flush();
  assert.equal(toggle.indeterminate, true); assert.equal(toggle.disabled, true);
  assert.equal(h.menuControls['menu-error'].textContent, 'Notification setup needs attention.');
});


test('compact action errors stay finite and current retry success clears only its own failure', async () => {
  const h = await feedbackHarness(testStoreHarness(), {menu: true, off: true}), toggle = h.menuControls.delivery;
  h.state.permissionReply = async () => {throw Error('private permission failure canary');};
  toggle.checked = true; await toggle.change();
  assert.equal(toggle.checked, false); assert.equal(toggle.indeterminate, false);
  assert.equal(toggle.disabled, false, 'pre-native failure preserves confirmed off and permits retry');
  assert.equal(h.menuControls['menu-error'].textContent, 'Could not update notifications.');
  delete h.state.permissionReply;
  const enable = h.controls.enable.click(); await enable;
  assert.equal(toggle.checked, true); assert.equal(h.menuControls['menu-error'].textContent, '');
  const blocked = await feedbackHarness(testStoreHarness(), {menu: true, off: true});
  blocked.state.permissionReply = async () => 'denied';
  blocked.menuControls.delivery.checked = true; await blocked.menuControls.delivery.change();
  assert.equal(blocked.menuControls['menu-error'].textContent, 'Notifications are blocked.');
  assert.equal(blocked.menuControls.delivery.checked, false);
});

for (const method of ['getSubscription', 'subscribe']) {
  test(`compact Enable ${method} failure cannot certify off from an earlier absence read`, async () => {
    const h = await feedbackHarness(testStoreHarness(), {menu: true, off: true}), toggle = h.menuControls.delivery;
    assert.equal(toggle.checked, false); assert.equal(toggle.indeterminate, false); assert.equal(toggle.disabled, false);
    let rejectNative, nativeCalls = 0;
    h.registration.pushManager[method] = () => {
      nativeCalls++;
      return new Promise((_, reject) => {rejectNative = reject;});
    };
    toggle.checked = true; const action = toggle.change(); await h.flush();
    assert.equal(nativeCalls, 1); assert.equal(typeof rejectNative, 'function');
    assert.equal(toggle.checked, false, 'busy action keeps the previous confirmed value');
    assert.equal(toggle.indeterminate, false); assert.equal(toggle.disabled, true);
    rejectNative(Error('private native failure canary')); await action;
    assert.equal(toggle.checked, false); assert.equal(toggle.indeterminate, true, 'failed native operation leaves absence unconfirmed');
    assert.equal(toggle.disabled, true);
    assert.equal(h.menuControls['menu-error'].textContent, 'Could not update notifications.');
    assert.equal(h.menuControls['menu-error'].hidden, false);
    assert.equal(h.calls.some(call => call.url.endsWith('/register') || call.url.endsWith('/disable')), false,
      'no handle was returned and no server registration or cleanup was attempted');
    assert.equal(h.state.subscribed, false);
    assert.equal(h.controls.enable.disabled, false, 'the full panel retains explicit recovery');
    h.state.permissionReply = async () => {throw Error('private permission failure after unknown native result');};
    await h.controls.enable.click();
    assert.equal(nativeCalls, 1, 'the second attempt failed before entering the native boundary');
    assert.equal(toggle.indeterminate, true, 'a pre-native retry failure cannot revive the retired absence proof');
    assert.equal(toggle.disabled, true); assert.equal(h.menuControls['menu-error'].textContent, 'Could not update notifications.');
  });
}

test('delivery gate commits CAS epochs in the existing v1 store and survives attention pruning', async () => {
  const h=testStoreHarness(), token='action-one';
  assert.equal(await h.store.readDeliveryGate(),null);
  await h.store.writeDeliveryGate({enabled:false,actionToken:token,registrationId:''},null);
  const enabled=await h.store.writeDeliveryGate({enabled:true,actionToken:token,registrationId:browserId},token);
  assert.equal(enabled.enabled,true);
  assert.equal(await h.store.writeDeliveryGate({enabled:false,actionToken:'stale',registrationId:''},null),null);
  await h.store.writeDeliveryGate({enabled:false,actionToken:'action-two',registrationId:''});
  assert.equal(await h.store.writeDeliveryGate({enabled:true,actionToken:token,registrationId:browserId},token),null);
  assert.equal((await h.store.readDeliveryGate()).enabled,false);
  await assert.rejects(h.store.update('browser-notification-delivery'),/Reserved/);
  await h.store.writeSound(false);
  for(let i=0;i<520;i++) h.records.set(`old-${i}`,{identity:`old-${i}`,at:i});
  await h.store.update(identity);
  assert.equal((await h.store.readDeliveryGate()).actionToken,'action-two');
  assert.equal((await h.reload().readDeliveryGate()).actionToken,'action-two','fresh context reads durable managed state');
  assert.equal(await h.store.readSound(),false);
  assert.deepEqual(h.versions,[1,1], 'reload reopens the existing database version without an upgrade');
  h.records.delete('browser-notification-delivery');
  await assert.rejects(h.store.readDeliveryGate(),/cannot save notification settings/,'managed context cannot return to legacy');
  await h.store.writeDeliveryGate({enabled:false,actionToken:'recovered',registrationId:''});
  h.state.holdCommit=true;
  const write=h.store.writeDeliveryGate({enabled:false,actionToken:'pending',registrationId:''});
  let settled=false; void write.then(()=>{settled=true;});
  for(let i=0;i<20;i++) await Promise.resolve();
  assert.equal(settled,false); h.state.holdCommit=false; h.state.completion(); await write;
});

test('managed worker drops off, old and missing epochs before every fallback and Test branch', async () => {
  const old=browserId, fresh='e'.repeat(64);
  const payloads=[event(),{...event(),expiresAt:1},testPayload(testDescriptor()),{version:99},null];
  for(const enabled of [false,true]) for(const payload of payloads) for(const epoch of [undefined,old]) {
    const h=workerHarness();
    h.context.workspaceAttentionStore.readDeliveryGate=async()=>({enabled,actionToken:'new',registrationId:fresh});
    await h.push(payload && {...payload,registrationId:epoch});
    assert.equal(h.notifications.length,0); assert.equal(h.calls.length,0); assert.equal(h.soundReads(),0);
  }
  const h=workerHarness(); let gate={enabled:true,actionToken:'one',registrationId:old};
  h.context.workspaceAttentionStore.readDeliveryGate=async()=>gate;
  await h.push({...event(),registrationId:old}); assert.equal(h.notifications.length,1);
  gate={enabled:false,actionToken:'off',registrationId:''};
  await h.push({...event(),registrationId:old});
  gate={enabled:true,actionToken:'two',registrationId:fresh};
  await h.push({...event(),registrationId:old}); assert.equal(h.notifications.length,1);
  h.inaccessible(); await h.push({...event(),registrationId:fresh});
  assert.equal(h.notifications.length,2); assert.equal(h.notifications.at(-1).title,'Workspace update','current epoch keeps neutral fallbacks');
});

test('managed worker rechecks Disable after status and sound awaits and fails closed on gate errors', async () => {
  for(const stage of ['status','sound']) {
    const h=workerHarness(); let gate={enabled:true,actionToken:'one',registrationId:browserId}, release;
    h.context.workspaceAttentionStore.readDeliveryGate=async()=>gate;
    if(stage==='status') h.context.fetch=()=>new Promise(resolve=>{release=()=>resolve({status:503,ok:false});});
    else h.context.workspaceAttentionStore.readSound=()=>new Promise(resolve=>{release=()=>resolve(true);});
    const work=h.push({...event(),registrationId:browserId});
    for(let i=0;i<30&&!release;i++) await Promise.resolve(); assert.equal(typeof release,'function');
    gate={enabled:false,actionToken:'off',registrationId:''}; release(); await work;
    assert.equal(h.notifications.length,0);
  }
  const h=workerHarness(); h.context.workspaceAttentionStore.readDeliveryGate=async()=>{throw Error('private gate canary');};
  await h.push({...event(),registrationId:browserId}); assert.equal(h.notifications.length,0);
  const storage=testStoreHarness(), t=displayHarness(storage);
  await storage.store.writeDeliveryGate({enabled:true,actionToken:'active',registrationId:browserId});
  let release;
  storage.store.readSound=()=>new Promise(resolve=>{release=()=>resolve(true);});
  const work=t.push({...testDescriptor(),registrationId:browserId});
  for(let i=0;i<100&&!release;i++) await Promise.resolve();
  await storage.store.writeDeliveryGate({enabled:false,actionToken:'off',registrationId:''}); release(); await work;
  assert.equal(t.shown.length,0,'Test has the same final display fence');
});

test('protected Enable rejects old server, old controlling worker and missing Web Locks', async () => {
  for(const option of ['oldServer','oldWorker','noLocks']) {
    const h=await notificationHarness({[option]:true,subscribed:false,registered:false});
    await h.controls.enable.click();
    assert.match(h.controls.status.textContent,/Notification protection is unavailable/);
    assert.equal(h.calls.filter(c=>c.url.endsWith('/register')).length,0);
    assert.equal(h.storage.has(browserKey),false);
    assert.equal(h.menu.delivery.checked,false);
  }
});

test('continuous enabled registration retains its epoch and pending outbox', async () => {
  const gateStorage=testStoreHarness();
  await gateStorage.store.writeDeliveryGate({enabled:true,actionToken:'continuous',registrationId:browserId});
  const state={gateStorage,subscribed:true,registered:true,outbox:new Set([browserId])};
  const h=await notificationHarness(state,new Map([[browserKey,browserId]]));
  await h.controls.enable.click();
  assert.match(h.controls.status.textContent,/^Enabled/);
  assert.equal(h.calls.some(c=>c.url.endsWith('/disable')),false);
  assert.equal(Object.hasOwn(h.calls.find(c=>c.url.endsWith('/register')).body,'requireFresh'),false);
  assert.equal((await gateStorage.store.readDeliveryGate()).actionToken,'continuous');
  assert.equal(state.outbox.has(browserId),true);
});

test('fresh Enable cannot adopt a late old registration and bounds cleanup retries', async () => {
  for(const repeated of [false,true]) {
    let attempts=0;
    const state={subscribed:true,registered:false,registerResponse:async body=>{
      assert.equal(body.requireFresh,true); attempts++;
      return repeated||attempts===1 ? {ok:false,status:409,json:async()=>({error:'existing endpoint'})} :
        {ok:true,json:async()=>({registrationId:browserId})};
    }};
    const h=await notificationHarness(state);
    await h.controls.enable.click();
    assert.equal(attempts,2); assert.equal(h.calls.filter(c=>c.url.endsWith('/disable')).length,2);
    const gate=await h.gateStorage.store.readDeliveryGate();
    assert.equal(gate.enabled,!repeated); assert.equal(h.storage.has(browserKey),!repeated);
  }
});

test('Disable fences a delayed old Enable across tabs before local storage or unsubscribe side effects', {timeout: 2000}, async t => {
  for(const stage of ['permission','registration']) {
    const state={subscribed:stage==='registration',registered:false}; const local=new Map();
    const first=await notificationHarness(state,local); let release, entered;
    const stageEntered=new Promise(resolve=>{entered=resolve;});
    if(stage==='permission') first.context.Notification.requestPermission=()=>new Promise(resolve=>{release=()=>resolve('granted');entered();});
    else state.registerResponse=()=>new Promise(resolve=>{release=()=>{state.registered=true;resolve({ok:true,json:async()=>({registrationId:browserId})});};entered();});
    const enabling=first.controls.enable.click(); await first.flush();
    t.diagnostic(`Waiting for deferred Enable ${stage} stage.`);
    await stageEntered;
    assert.equal(typeof release,'function',stage);
    const second=await notificationHarness(state,local);
    const disabling=second.controls.disable.click(); await second.flush();
    assert.equal((await state.gateStorage.store.readDeliveryGate()).enabled,false);
    release(); await Promise.all([enabling,disabling]);
    assert.equal(local.has(browserKey),false); assert.equal(state.subscribed,false);
    assert.equal((await state.gateStorage.store.readDeliveryGate()).enabled,false);
    assert.doesNotMatch(first.controls.status.textContent,/^Enabled/);
    assert.match(second.controls.status.textContent,/^Disabled/);
    if(stage==='permission') assert.equal(first.calls.some(c=>c.url.endsWith('/register')),false);
  }
});

test('Disable storage failure still cleans both transports but stays incomplete and retryable', async () => {
  const h=await notificationHarness({subscribed:true,registered:true},new Map([[browserKey,browserId]]));
  h.gateStorage.state.commitFailure=true;
  await h.controls.disable.click();
  assert.equal(h.storage.has(browserKey),false); assert.equal(h.calls.filter(c=>c.url.endsWith('/disable')).length,2);
  assert.match(h.controls.status.textContent,/blocking could not be saved/); assert.equal(h.controls.disable.disabled,false);
  h.gateStorage.state.commitFailure=false; await h.controls.disable.click();
  assert.match(h.controls.status.textContent,/^Disabled/);
});
