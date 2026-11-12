const {test} = require('node:test');
const assert = require('node:assert/strict');
const {webcrypto} = require('node:crypto');
if (!globalThis.crypto) globalThis.crypto = webcrypto;
const load = () => import('../internal/web/static/pi-workflow-ui.js');
class Element {
  constructor(tagName, ownerDocument) {
    Object.assign(this, {tagName, ownerDocument, children: [], dataset: {}, style: {}, attributes: {}, handlers: new Map(), hidden: false, disabled: false, className: '', text: ''});
    const classes = () => new Set(this.className.split(/\s+/).filter(Boolean));
    this.classList = {add: name => {const values = classes(); values.add(name); this.className = [...values].join(' ');},
      toggle: (name, enabled) => {const values = classes(); enabled ? values.add(name) : values.delete(name); this.className = [...values].join(' ');}};
  }
  set textContent(value) {this.text = String(value);}
  get textContent() {return this.text + this.children.map(node => node.textContent).join('');}
  append(...nodes) {for (const node of nodes) {node.remove(); this.children.push(node); node.parentElement = this;}}
  replaceChildren(...nodes) {for (const node of this.children) node.parentElement = null; this.children = []; this.text = ''; this.append(...nodes);}
  remove() {if (this.parentElement) {const list = this.parentElement.children; list.splice(list.indexOf(this), 1); this.parentElement = null;}}
  setAttribute(name, value) {this.attributes[name] = value;}
  addEventListener(type, action) {if (!this.handlers.has(type)) this.handlers.set(type, new Set()); this.handlers.get(type).add(action);}
  removeEventListener(type, action) {this.handlers.get(type)?.delete(action);}
  click() {if (!this.disabled) for (const action of this.handlers.get('click') || []) action({target: this});}
}
class Storage {
  values = new Map(); getItem(key) {return this.values.get(key) || null;} setItem(key, value) {this.values.set(key, value);}
}
const settle = async () => {for (let i = 0; i < 100; i++) await Promise.resolve();};
const deferred = () => {let resolve, reject; const promise = new Promise((a, b) => {resolve = a; reject = b;}); return {promise, resolve, reject};};
const response = (status, value) => ({ok: status >= 200 && status < 300, status, json: async () => structuredClone(value)});
const groupID = '11111111-1111-4111-8111-111111111111', planID = '22222222-2222-4222-8222-222222222222';
const descriptor = () => ({backend: 'pi', basePath: '/api/sessions/2026-11-11-test', member: 'implementer4', namespace: 'durable-incarnations'});
const planView = (mode = 'plan', revision = 1, digest = 'a'.repeat(64)) => ({revision: 1, mode,
  toolPolicyRevision: revision, toolPolicyDigest: digest, canSelectMode: true, acceptances: [],
  proposals: [{planId: planID, revision: 1, text: 'Plan é\r\n<script>literal</script>', textSha256: 'c'.repeat(64), complete: true, canAccept: true}]});
const questionView = () => ({revision: 1, questions: [{id: groupID, revision: 1, recipient: 'implementer4', canRespond: true, status: 'pending', answers: {}, latestResponse: null,
  questions: [{id: '0', title: 'Which?', options: [{id: '0', label: 'One'}, {id: '1', label: 'Two'}]}]}]});
const find = (root, predicate) => predicate(root) ? root : root.children.map(child => find(child, predicate)).find(Boolean);
const textButton = (root, text) => find(root, value => value.tagName === 'button' && value.textContent === text);
function fixture() {
  const doc = {createElement: tag => new Element(tag, doc)};
  const roots = Object.fromEntries(['questions', 'plan', 'mode', 'modeStatus'].map(key => [key, doc.createElement('div')]));
  const cards = [], calls = [], storage = new Storage();
  let questions = questionView(), plan = planView(), post;
  const fetchRequest = async (path, opts) => {
    calls.push({path, opts}); assert.equal(opts.cache, 'no-store'); assert.equal(opts.credentials, 'same-origin');
    const url = new URL(path, 'https://portal.example'); assert.equal(url.origin, 'https://portal.example');
    assert.equal(url.searchParams.get('member'), 'implementer4'); assert.equal([...url.searchParams].length, 1);
    if (opts.method !== 'GET') return post(path, JSON.parse(opts.body), opts);
    if (url.pathname.endsWith('/pi-questions')) return response(200, questions);
    if (url.pathname.endsWith('/pi-plan')) return response(200, plan);
    throw Error('unexpected read ' + path);
  };
  const createQuestionCard = props => {cards.push(props); return {root: doc.createElement('article'), update() {}};};
  return {roots, cards, calls, storage, fetchRequest, createQuestionCard,
    get questions() {return questions;}, set questions(value) {questions = value;},
    get plan() {return plan;}, set plan(value) {plan = value;},
    set post(value) {post = value;},
    args: () => ({descriptor: descriptor(), roots, storage, fetchRequest, createQuestionCard})};
}

test('missing or invalid descriptor leaves Codex roots untouched and performs no request', async () => {
  const {mountPiWorkflows} = await load(), f = fixture();
  f.roots.plan.textContent = 'existing Codex owner';
  for (const value of [undefined, {...descriptor(), backend: 'codex'}, {...descriptor(), basePath: '//evil.example/a'}, {...descriptor(), basePath: descriptor().basePath + '?member=lead'}, {...descriptor(), member: '../lead'}, {...descriptor(), namespace: ''}]) {
    const c = mountPiWorkflows({...f.args(), descriptor: value}); await c.refresh(); c.destroy();
  }
  assert.equal(f.calls.length, 0); assert.equal(f.roots.plan.textContent, 'existing Codex owner');
});
test('mount pins cards and renders exact Plan data with existing control styles and no POST', async () => {
  const {mountPiWorkflows} = await load(), f = fixture();
  const c = mountPiWorkflows(f.args()); await settle();
  assert.equal(f.calls.length, 2); assert(f.calls.every(call => call.opts.method === 'GET'));
  assert.equal(f.roots.questions.hidden, false); assert.equal(f.cards.length, 1);
  assert.deepEqual(f.cards[0].choiceValues, ['0', '1']);
  assert.equal(find(f.roots.plan, n => n.tagName === 'pre').textContent, f.plan.proposals[0].text);
  assert.equal(textButton(f.roots.mode, 'Plan').className, 'quiet plan-mode active');
  assert.equal(textButton(f.roots.plan, 'Implement here').disabled, false);
  textButton(f.roots.plan, 'Keep planning').click(); assert.equal(f.roots.plan.hidden, true);
  await c.refresh(); assert(f.calls.every(call => call.opts.method === 'GET')); c.destroy();
});
test('mode API preserves positive 409 rejection and sends displayed CAS exactly once', async () => {
  const {mountPiWorkflows} = await load(), f = fixture(); let posts = 0;
  f.post = (path, value) => {
    assert(path.includes('/pi-plan/mode?')); posts++;
    assert.deepEqual(value, {mode: 'default', expectedRevision: 1, expectedDigest: 'a'.repeat(64)});
    return response(409, {outcome: 'rejected'});
  };
  const c = mountPiWorkflows(f.args()); await settle(); textButton(f.roots.mode, 'Default').click(); await settle();
  assert.equal(posts, 1); assert.equal(f.roots.modeStatus.textContent, 'The mode change was rejected.');
  assert.equal(textButton(f.roots.mode, 'Default').disabled, false); c.destroy();
});
test('automatic and old GETs cannot clear unknown mode; only a settled explicit refresh does', async () => {
  const {mountPiWorkflows} = await load(), f = fixture(); let posts = 0, heldRead;
  const old = deferred(), raw = f.fetchRequest;
  const fetchRequest = (path, opts) => {
    if (heldRead && opts.method === 'GET' && path.includes('/pi-plan?')) {heldRead = false; return old.promise;}
    return raw(path, opts);
  };
  f.post = () => {posts++; throw Error('lost mode ACK');};
  const c = mountPiWorkflows({...f.args(), fetchRequest}); await settle();
  heldRead = true; const delayed = c.refresh(); await settle();
  textButton(f.roots.mode, 'Default').click(); await settle();
  f.plan = planView('default', 2, 'b'.repeat(64)); await c.refresh();
  assert.equal(textButton(f.roots.mode, 'Plan').disabled, true);
  old.resolve(response(200, planView())); await delayed;
  assert.equal(textButton(f.roots.mode, 'Default').attributes['aria-pressed'], 'true');
  assert.equal(textButton(f.roots.mode, 'Plan').disabled, true);
  textButton(f.roots.modeStatus, 'Refresh').click(); await settle(); assert.equal(textButton(f.roots.mode, 'Plan').disabled, false);
  assert.equal(posts, 1); assert([...f.storage.values.keys()].every(key => key.startsWith('pi-questions:')), 'mode creates no stored request'); c.destroy();
});
test('an explicit GET started before mode settlement cannot unlock uncertainty', async () => {
  const {mountPiWorkflows} = await load(), f = fixture(), effect = deferred(), read = deferred();
  let heldRead = false; const raw = f.fetchRequest;
  const fetchRequest = (path, opts) => heldRead && opts.method === 'GET' && path.includes('/pi-plan?') ? read.promise : raw(path, opts);
  f.post = () => effect.promise;
  const c = mountPiWorkflows({...f.args(), fetchRequest}); await settle(); textButton(f.roots.mode, 'Default').click();
  heldRead = true; const early = c.refresh({explicit: true}); await settle(); effect.resolve(response(202, {outcome: 'unknown'})); await settle();
  read.resolve(response(200, planView('default', 2, 'b'.repeat(64)))); await early;
  assert.equal(textButton(f.roots.mode, 'Plan').disabled, true); c.destroy();
});
test('pending answers survive remount without resend and explicit item correction stays separate', async () => {
  const {mountPiWorkflows} = await load(), f = fixture(); let posts = 0;
  f.post = () => {posts++; throw Error('lost answer ACK');};
  let c = mountPiWorkflows(f.args()); await settle(); f.cards[0].onDraft({choice: '1', note: 'saved exact draft'}); f.cards[0].onSubmit(); await settle(); c.destroy();
  c = mountPiWorkflows(f.args()); await settle(); const card = f.cards.at(-1);
  assert.equal(posts, 1); assert.equal(card.draft().note, 'saved exact draft'); assert.equal(card.state().locked, true); c.destroy();
  const other = mountPiWorkflows({...f.args(), descriptor: {...descriptor(), namespace: 'different-conversation'}}); await settle();
  assert.equal(f.cards.at(-1).state().locked, false); assert.equal(posts, 1); other.destroy();
  const answered = fixture(), responseID = '33333333-3333-4333-8333-333333333333';
  answered.questions.questions[0].answers = {'0': {questionId: '0', choiceId: '0', text: 'first'}};
  answered.questions.questions[0].latestResponse = {responseId: responseID, status: 'delivered'};
  answered.post = (_path, value) => {assert.equal(value.supersedesResponseId, responseID); assert.notEqual(value.responseId, responseID); return response(202, value);};
  const corrected = mountPiWorkflows(answered.args()); await settle(); textButton(answered.roots.questions, 'Edit answer').click();
  assert.equal(answered.cards[0].state().locked, false); answered.cards[0].onDraft({choice: '', note: 'correction'}); answered.cards[0].onSubmit(); await settle(); corrected.destroy();
});
test('destroy aborts owned reads and ignores late completions without intervals', async () => {
  const {mountPiWorkflows} = await load(), f = fixture(), pending = deferred(); const signals = [];
  const previousInterval = globalThis.setInterval;
  globalThis.setInterval = () => {throw Error('unexpected interval');};
  try {
    const c = mountPiWorkflows({...f.args(), fetchRequest: (_path, opts) => {signals.push(opts.signal); return pending.promise;}});
    c.destroy(); assert.equal(signals.length, 2); assert(signals.every(signal => signal.aborted));
    pending.resolve(response(200, planView())); await settle();
    assert(Object.values(f.roots).every(root => root.hidden && root.children.length === 0));
  } finally {globalThis.setInterval = previousInterval;}
});

test('Plan renderer binds exact displayed identity and status checks never repeat acceptance', async () => {
  const {mountPiWorkflows} = await load(), f = fixture(); let posts = 0, statuses = 0, saved;
  const raw = f.fetchRequest;
  const fetchRequest = (path, opts) => {
    if (opts.method === 'GET' && path.includes('/pi-plan/acceptances/')) {
      statuses++; assert(path.includes(saved.acceptanceId)); assert.equal(opts.cache, 'no-store'); return response(200, {...saved, phase: 'prepared'});
    }
    return raw(path, opts);
  };
  f.post = (path, value) => {
    posts++; assert(path.includes('/pi-plan/acceptances?'));
    assert.equal(value.planId, planID); assert.equal(value.revision, 1); assert.equal(value.textSha256, 'c'.repeat(64));
    assert.match(value.acceptanceId, /^[0-9a-f-]{36}$/);
    saved = {...value, operationId: '44444444-4444-4444-8444-444444444444', phase: 'prepared'};
    f.plan.acceptances = [saved]; f.plan.proposals[0].canAccept = false;
    return response(202, saved);
  };
  const c = mountPiWorkflows({...f.args(), fetchRequest}); await settle(); textButton(f.roots.plan, 'Implement here').click(); await settle();
  assert.equal(posts, 1); assert.equal(textButton(f.roots.plan, 'Check request').hidden, false);
  textButton(f.roots.plan, 'Check request').click(); await settle();
  assert.equal(statuses, 1); assert.equal(posts, 1);
  f.plan.acceptances[0].phase = 'accepted'; await c.refresh();
  assert.equal(f.roots.plan.hidden, true, 'native acceptance ends the decision panel'); c.destroy();
});
test('failed reads disable old controls without inventing stale question history', async () => {
  const {mountPiWorkflows} = await load(), f = fixture(); let unavailable = false; const raw = f.fetchRequest;
  const c = mountPiWorkflows({...f.args(), fetchRequest: (...args) => {if (unavailable) throw Error('offline'); return raw(...args);}});
  await settle(); assert.equal(f.cards[0].state().enabled, true); unavailable = true; await c.refresh();
  assert.equal(f.cards[0].state().enabled, false); assert.equal(f.cards[0].state().statusText, 'pending');
  assert.equal(textButton(f.roots.mode, 'Plan').disabled, true); c.destroy();
});
