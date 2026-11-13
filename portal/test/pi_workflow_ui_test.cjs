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
  values = new Map(); reads = []; writes = [];
  get length() {return this.values.size;} key(index) {return [...this.values.keys()][index] || null;}
  getItem(key) {this.reads.push(key); return this.values.get(key) || null;}
  setItem(key, value) {this.writes.push(key); this.values.set(key, value);}
  removeItem(key) {this.values.delete(key);}
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

const app = require('../internal/web/static/app.js');
const {createPiWorkflowPage, createPiConversationScope} = app;
const pageScope = 'pi-workflow-v1:' + 'd'.repeat(64);
const {readFileSync} = require('node:fs'), vm = require('node:vm');
const appSource = readFileSync(require.resolve('../internal/web/static/app.js'), 'utf8');
const section = (start, end) => {
  const a = appSource.indexOf(start), b = appSource.indexOf(end, a);
  assert(a > 0 && b > a, start); return appSource.slice(a, b);
};
const pageBody = value => ({dataset: {sessionBackend: 'pi', session: '2026-11-11-test', selectedMember: 'implementer4', conversationScope: pageScope, piWorkflow: JSON.stringify(value || {...descriptor(), namespace: pageScope})}});
const pageArgs = (f, body = pageBody()) => ({...f.args(), body, load});

test('page backend and selected durable scope gate descriptor parsing and module loading', async () => {
  const f = fixture(); let loads = 0;
  const body = pageBody(), args = {...pageArgs(f, body), load: async () => {loads++; return load();}};
  for (const backend of ['codex', '', 'unknown']) {
    body.dataset.sessionBackend = backend;
    Object.defineProperty(body.dataset, 'piWorkflow', {configurable: true, get() {throw Error('non-Pi descriptor was parsed');}});
    const page = createPiWorkflowPage(args); await page.refresh(); page.destroy();
  }
  Object.defineProperty(body.dataset, 'piWorkflow', {configurable: true, writable: true, value: ''});
  body.dataset.sessionBackend = 'pi';
  for (const text of ['', '{', 'null', JSON.stringify({...descriptor(), backend: 'codex'}),
    JSON.stringify({...descriptor(), basePath: '/api/sessions/2026-11-11-other'}),
    JSON.stringify({...descriptor(), member: 'lead'}), JSON.stringify({...descriptor(), namespace: ''}),
    JSON.stringify({...descriptor(), extra: true})]) {
    body.dataset.piWorkflow = text; const page = createPiWorkflowPage(args); await page.refresh(); page.destroy();
  }
  assert.equal(loads, 0); assert.equal(f.calls.length, 0); assert.equal(f.cards.length, 0);
});

test('actual initialization registers legacy question, mode and Plan handlers only for explicit Codex', async () => {
  for (const backend of ['pi', 'codex', '', 'unknown']) {
    const f = fixture(), body = pageBody(); body.dataset.sessionBackend = backend;
    const controls = new Map(['plan-actions', 'plan-keep-planning', 'plan-implement-same', 'plan-implement-new',
      'plan-session-dialog', 'plan-session-form', 'codex-mode-status'].map(id => [id, f.roots.plan.ownerDocument.createElement('div')]));
    controls.get('plan-session-dialog').querySelector = () => null;
    let questionHandlers = 0, notices = 0, modeReads = 0, heuristics = 0;
    const context = vm.createContext({body, document: {getElementById: id => controls.get(id), querySelector: () => null},
      conversationAssets: {createAsyncQuestions() {questionHandlers++;}, createRefreshNotice() {notices++; return {}; }},
      optionalQuestionStorage: f.storage, conversationID: 'conversation', selectedMember: '', interactive: true,
      executionHeld: false, liveSettingsSaving: false, client: {modes: async () => {modeReads++; return [];}},
      applyCurrentSettings() {}, renderCollaborationModes() {},
      currentCompletedPlan() {heuristics++; return null;}, composerView: {setPlanVisible() {}},
      updateManagedFormSubmitEligibility() {}, planRenderGeneration: 0, settingsWriteGeneration: 0});
    vm.runInContext(section('  const codexBackend =', '  const piBackend =') +
      section('  const optionalQuestions =', '  const mountPageWorkflows =') +
      section('  const planActions =', '  async function retryPlanImplementation(') +
      section('  if (planActions) document.getElementById("plan-implement-same")', '  deliveryRetries =') +
      section('  const planSessionDialog =', '  // Both settings popups') +
      section('  const modeStatus = codexBackend', '  const appendFileChanges =') +
      section('  const renderPlanActions =', '  let transcriptUserScroll =') +
      '\nglobalThis.loadModes = loadCollaborationModes; globalThis.renderPlan = renderPlanActions;', context);
    await context.loadModes(); await context.renderPlan({});
    const codex = backend === 'codex';
    assert.equal(questionHandlers, Number(codex)); assert.equal(notices, Number(codex));
    assert.equal(modeReads, Number(codex)); assert.equal(heuristics, Number(codex));
    for (const id of ['plan-keep-planning', 'plan-implement-same', 'plan-implement-new']) {
      assert.equal(controls.get(id).handlers.get('click')?.size || 0, Number(codex), backend + '/' + id);
    }
    assert.equal(controls.get('plan-session-form').handlers.get('submit')?.size || 0, Number(codex));
  }
});

test('ordinary settings refresh leaves Pi modes owned by workflow proof and keeps unrelated settings updates', () => {
  for (const codexBackend of [false, true]) {
    const f = fixture(), button = f.roots.mode.ownerDocument.createElement('button');
    button.dataset.codexMode = 'plan'; button.disabled = true;
    f.roots.modeStatus.textContent = 'workflow owns this status'; f.roots.mode.hidden = true;
    f.roots.modeStatus.dataset.catalogNotice = 'true';
    let restores = 0, previews = 0, forks = 0;
    const context = vm.createContext({codexBackend, collaborationModes: [{mode: 'plan'}], currentMode: 'plan',
      modeCatalogState: 'ready', interactive: true, liveSettingsSaving: false, threadActive: false,
      document: {querySelectorAll: selector => selector === '[data-codex-mode]' ? [button] : [],
        getElementById: id => id === 'fork-open' ? {toggleAttribute() {forks++;}} : id === 'codex-mode' ? f.roots.mode : f.roots.modeStatus},
      restoreManagedCreationDrafts() {restores++;}, updateCreationCLI() {previews++;}});
    vm.runInContext(section('  const applyCurrentSettings =', '  const loadModels =') + '\napplyCurrentSettings();', context);
    assert.deepEqual([restores, previews, forks], [1, 1, 1]);
    assert.equal(button.disabled, !codexBackend);
    assert.equal(f.roots.mode.hidden, !codexBackend);
    assert.equal(f.roots.modeStatus.textContent, codexBackend ? '' : 'workflow owns this status');
  }
});

function mountedPage(f) {
  const body = pageBody(), events = new Map(), hooks = [], layouts = [];
  const roots = new Map(Object.entries(f.roots).map(([key, root]) => [({questions: 'optional-questions', plan: 'plan-actions', mode: 'codex-mode', modeStatus: 'codex-mode-status'})[key], root]));
  const piPageScope = createPiConversationScope({body, fetchRequest: f.fetchRequest});
  const context = vm.createContext({body, piBackend: true, piPageScope, piWorkflows: null, invalidatePiConversation: () => piPageScope.invalidate(), optionalQuestionPanel: f.roots.questions, optionalQuestionStorage: f.storage,
    document: {getElementById: id => roots.get(id)}, fetch: f.fetchRequest,
    createPiWorkflowPage: args => createPiWorkflowPage({...args, load}),
    composerView: {setPlanVisible(value) {layouts.push(value);}}, conversationAssets: {createQuestionCard: f.createQuestionCard,
      createConversationSync: value => {hooks.push(value); return {destroy() {}};}, renderConnectionStatus() {}},
    addEventListener: (name, action) => events.set(name, action), sync: null,
    interactive: true, executionHeld: false, client: {eventsPath: () => '/events'},
    renderThread() {}, threadActive: false, refreshPending() {}, refreshQueue() {}, refreshActivity() {},
    scheduleHistoryRepair() {}, loadCollaborationModes() {}});
  vm.runInContext(section('  const mountPageWorkflows =', '  const transcriptViews =') +
    section('  let lastSyncStatus =', '  setInterval(() => { void refreshPending();') +
    '\nglobalThis.startSync = restartConversationSync;', context);
  context.startSync();
  return {body, events, hooks, context, layouts};
}

test('actual page mount uses one raw-fetch path for card, mode rejection and exact Plan acceptance', async () => {
  const f = fixture(); let modes = 0, answers = 0, plans = 0;
  f.post = (path, value) => {
    if (path.includes('/pi-plan/mode?')) {modes++; return response(409, {outcome: 'rejected'});}
    if (path.includes('/pi-questions/responses?')) {answers++; return response(202, value);}
    assert(path.includes('/pi-plan/acceptances?')); plans++;
    assert.equal(value.planId, planID); assert.equal(value.textSha256, f.plan.proposals[0].textSha256);
    f.plan.acceptances = [{...value, phase: 'prepared'}]; return response(202, f.plan.acceptances[0]);
  };
  const page = mountedPage(f); await settle();
  assert.equal(f.cards.length, 1); assert.equal(f.calls.length, 2);
  assert.equal(find(f.roots.plan, n => n.tagName === 'pre').textContent, f.plan.proposals[0].text);
  textButton(f.roots.mode, 'Default').click(); await settle();
  assert.equal(f.roots.modeStatus.textContent, 'The mode change was rejected.');
  page.hooks[0].apply({}, {isCurrent: () => true}); await settle();
  f.cards[0].onDraft({choice: '0', note: 'one answer'}); f.cards[0].onSubmit(); await settle();
  page.hooks[0].apply({}, {isCurrent: () => true}); await settle();
  textButton(f.roots.plan, 'Implement here').click(); await settle();
  assert.deepEqual([modes, answers, plans], [1, 1, 1]);
  page.events.get('pagehide')();
  assert.equal(page.layouts.at(-1), false, 'teardown restores the composer after hiding the decision panel');
});

test('actual apply and reconnect hooks retain uncertainty until the helper explicit Refresh', async () => {
  const f = fixture(); let posts = 0;
  f.post = () => {posts++; throw Error('lost ACK');};
  const page = mountedPage(f); await settle(); textButton(f.roots.mode, 'Default').click(); await settle();
  f.plan = planView('default', 2, 'b'.repeat(64));
  page.hooks[0].apply({}, {isCurrent: () => true});
  page.hooks[0].onStateChange({status: 'connected'}); await settle();
  assert.equal(textButton(f.roots.mode, 'Plan').disabled, true);
  const before = f.calls.length; page.hooks[0].apply({}, {isCurrent: () => false}); await settle();
  assert.equal(f.calls.length, before, 'superseded conversation cannot refresh workflows');
  textButton(f.roots.modeStatus, 'Refresh').click(); await settle();
  assert.equal(textButton(f.roots.mode, 'Plan').disabled, false); assert.equal(posts, 1);
  page.events.get('pagehide')();
});

test('page scope replacement detaches old reads and keeps drafts isolated without resend', async () => {
  const f = fixture(), body = pageBody(), old = deferred(); let hold = false;
  const raw = f.fetchRequest, signals = [];
  let page = createPiWorkflowPage({...pageArgs(f, body), fetchRequest: (path, opts) => {
    if (hold) {signals.push(opts.signal); return old.promise;}
    return raw(path, opts);
  }});
  await page.refresh(); await settle(); f.cards[0].onDraft({choice: '0', note: 'first scope draft'});
  hold = true; const reading = page.refresh(); await settle();
  body.dataset.conversationScope = 'pi-workflow-v1:' + 'e'.repeat(64);
  body.dataset.piWorkflow = JSON.stringify({...descriptor(), namespace: body.dataset.conversationScope}); hold = false;
  await page.refresh(); page.destroy(); page = createPiWorkflowPage(pageArgs(f, body)); await page.refresh(); await settle();
  assert(signals.every(signal => signal.aborted)); assert.equal(f.cards.at(-1).draft().note, '');
  old.resolve(response(200, planView())); await reading;
  assert.equal(f.cards.at(-1).draft().note, '');
  page.destroy(); body.dataset.conversationScope = pageScope; body.dataset.piWorkflow = JSON.stringify({...descriptor(), namespace: pageScope});
  page = createPiWorkflowPage(pageArgs(f, body)); await page.refresh(); await settle();
  assert.equal(f.cards.at(-1).draft().note, 'first scope draft');
  assert(f.calls.every(call => call.opts.method === 'GET')); page.destroy();
});

test('page teardown and invalidated selection cannot mount from an old module completion', async () => {
  const f = fixture(), module = deferred(), body = pageBody(); let mounts = 0;
  const page = createPiWorkflowPage({...pageArgs(f, body), load: () => module.promise});
  const first = page.refresh(); page.invalidate();
  module.resolve({mountPiWorkflows() {mounts++; return {refresh() {}, destroy() {}};}}); await first;
  await page.refresh(); assert.equal(mounts, 0, 'same descriptor cannot prove replacement selection');
  body.dataset.piWorkflow = JSON.stringify({...descriptor(), namespace: 'new-selection'});
  await page.refresh(); assert.equal(mounts, 0, 'new workflow descriptor cannot replace ordinary page scope'); page.destroy(); await page.refresh();
  const late = deferred(), stopped = createPiWorkflowPage({...pageArgs(f), load: () => late.promise});
  const pending = stopped.refresh(); stopped.destroy(); late.resolve({mountPiWorkflows() {throw Error('late mount');}}); await pending;
  const changed = deferred(), selected = pageBody();
  const moving = createPiWorkflowPage({...pageArgs(f, selected), load: () => changed.promise});
  const opening = moving.refresh(); selected.dataset.selectedMember = 'lead';
  changed.resolve({mountPiWorkflows() {throw Error('selection changed during import');}}); await opening; moving.destroy();
});

test('frozen Pi fetch covers existing options, details and P6 while EventSource gets one query', async () => {
  const body = pageBody(), calls = [];
  const scope = createPiConversationScope({body, fetchRequest: async (path, options) => {
    calls.push({path, options}); return response(200, {});
  }});
  const abort = new AbortController(), options = {method: 'POST', body: 'exact', signal: abort.signal,
    credentials: 'same-origin', headers: new Headers({'Content-Type': 'application/json', 'X-Keep': 'yes'})};
  await scope.fetch('/message', options);
  assert.equal(calls[0].options.headers.get('X-Workspace-Conversation-Scope'), pageScope);
  assert.equal(calls[0].options.headers.get('X-Keep'), 'yes'); assert.equal(calls[0].options.signal, abort.signal);
  assert.equal(calls[0].options.body, 'exact'); assert.equal(options.headers.has('X-Workspace-Conversation-Scope'), false);
  const client = app.createSessionClient('2026-11-11-test', app.createRequest(scope.fetch), {}, 'implementer4', 'observation');
  await client.details({signal: abort.signal, headers: {'X-Keep': 'details'}});
  assert.equal(calls[1].options.headers.get('X-Session-Observation'), 'observation');
  assert.equal(calls[1].options.headers.get('X-Workspace-Conversation-Scope'), pageScope);
  assert.equal(calls[1].options.headers.get('X-Keep'), 'details'); assert(calls[1].path.endsWith('/details?member=implementer4'));
  for (let i = 0; i < 2; i++) {
    const path = new URL(scope.eventsPath('/events?keep=1'), 'https://portal.example');
    assert.deepEqual(path.searchParams.getAll('conversationScope'), [pageScope]); assert.equal(path.searchParams.get('keep'), '1');
  }
  assert.throws(() => scope.eventsPath('/events?conversationScope=old'), /identity changed/);
  assert.throws(() => scope.fetch('/message', {headers: {'X-Workspace-Conversation-Scope': 'wrong'}}), /identity changed/);
  const duplicate = new Headers([['X-Workspace-Conversation-Scope', pageScope], ['X-Workspace-Conversation-Scope', pageScope]]);
  assert.throws(() => scope.fetch('/message', {headers: duplicate}), /identity changed/);
  body.dataset.conversationScope = 'pi-workflow-v1:' + 'e'.repeat(64);
  await assert.rejects(scope.fetch('/message'), /identity changed/); assert.equal(calls.length, 2);
  const f = fixture(), page = mountedPage(f); await settle();
  assert(f.calls.every(call => call.opts.headers.get('X-Workspace-Conversation-Scope') === pageScope));
  page.events.get('pagehide')();
});

// The established durable-store port is pure here. Its prefix is supplied by
// the shipped app helpers; this fake only reads/writes the injected storage.
app.configureDurableAttemptStore(({storage, prefix, decode, encode}) => ({
  available() {return Boolean(storage);},
  load() {
    if (!storage) return null;
    return [...Array(storage.length)].map((_, i) => storage.key(i)).filter(key => key?.startsWith(prefix))
      .map(key => decode(key.slice(prefix.length), JSON.parse(storage.getItem(key))));
  },
  store(value) {if (!storage) return false; storage.setItem(prefix + value.id, JSON.stringify(encode(value))); return true;},
  remove(id) {if (!storage) return false; storage.removeItem(prefix + id); return true;},
  clear() {return true;},
}));

function ordinaryComposer({backend = 'pi', storage = new Storage(), held = false} = {}) {
  const f = fixture(), body = pageBody(); body.dataset.sessionBackend = backend;
  const scope = createPiConversationScope({body, fetchRequest: f.fetchRequest});
  const doc = f.roots.plan.ownerDocument, form = doc.createElement('form'), textarea = doc.createElement('textarea');
  textarea.value = 'text before identity'; form.elements = {message: textarea};
  const queue = doc.createElement('button'), interrupt = doc.createElement('button');
  const nodes = new Map([['message-form', form], ['message-queue', queue], ['interrupt', interrupt]]);
  const effects = [], replies = [], failures = [], events = new Map(), alerts = []; let ui = 0, uploads = 0, refreshes = 0;
  const call = action => (...args) => {effects.push({action, args}); const reply = deferred(); replies.push(reply); return reply.promise;};
  const client = {message: call('send'), queueMessage: call('queue'), interrupt: call('interrupt'),
    isCurrentConversation: () => backend !== 'pi' || scope.current()};
  const context = vm.createContext({...app, body, piPageScope: scope, piBackend: backend === 'pi', codexBackend: backend === 'codex',
    document: {getElementById: id => nodes.get(id), hidden: false},
    sessionStorage: storage, localStorage: storage, crypto: webcrypto, client, currentThreadId: backend === 'codex' ? 'codex-thread' : '',
    conversationID: '2026-11-11-test~implementer4', slug: '2026-11-11-test', interactive: true, executionHeld: held,
    composerReady: false, composerUploads: null, composerUploadReady: true, messageDispatcher: null,
    bindMessageComposer() {}, deactivateMessageComposer() {}, pendingMessages: new Map(), inFlightMessageIDs: new Set(),
    dispatchOperations: new Map(), sendAttemptStorage: backend === 'codex' ? storage : null, requestInputDraftStorage: null,
    deliveryRetries: {stop() {}, schedule() {}}, messageDispatchInFlight: false, planImplementationInFlight: false,
    threadActive: false, liveSettingsSaving: false, optionalQuestions: null, submissionStatus: null,
    failedMessageDrafts: new Map(), conversationAssets: {sameAttachments: (a, b) => JSON.stringify(a) === JSON.stringify(b),
      mountUploads() {uploads++; return {count: () => 0, ids: () => [], lock() {}, clear() {}};}},
    canSubmitExplicitInput: () => context.interactive && !context.executionHeld &&
      (backend === 'codex' || scope.current() && context.composerReady),
    updateMessageActions() {ui++;}, renderMessageReceipts() {ui++;}, followTranscript() {ui++;},
    scheduleRefresh() {refreshes++;}, alert: value => alerts.push(value), refreshSessionDetails() {}, refreshQueue() {}, acknowledgeTranscriptMessages() {},
    beginSubmission: value => value, submissionIdentity: (threadID, id, action = 'send') => ({threadID, id, action}),
    clearSubmissionFailure() {}, showSubmissionFailure(error) {failures.push(error);},
    invalidatePiConversation() {scope.invalidate(); context.deactivateMessageComposer(); context.composerReady = false; context.interactive = false; context.executionHeld = true;},
    addEventListener: (name, action) => events.set(name, action),
  });
  vm.runInContext(section('  const acceptMessageReceipt =', '  const retryMessageReceipt =') +
    section('  const form = document.getElementById("message-form");', '  queueStart?.addEventListener') +
    section('  const renderThread =', '    threadStatusBadge.selectThread(currentThreadId);') +
    '\n}; globalThis.adopt = renderThread;', context);
  const submit = async () => {
    const event = {prevented: false, preventDefault() {this.prevented = true;}};
    await Promise.all([...(form.handlers.get('submit') || [])].map(action => action(event))); return event;
  };
  return {body, scope, storage, context, client, form, textarea, interrupt, effects, replies, failures, events, alerts, submit,
    get ui() {return ui;}, get uploads() {return uploads;}, get refreshes() {return refreshes;}, adopt: (threadId = 'actual-neutral-thread', current = true) => context.adopt({threadId}, () => current),
    invalidate() {context.invalidatePiConversation();}};
}

test('Pi composer waits for current scoped identity, preserves text and sends once without uploads', async () => {
  const f = ordinaryComposer();
  f.body.dataset.piWorkflow = '';
  assert.equal((await f.submit()).prevented, true); assert.equal(f.effects.length, 0);
  assert.equal(f.storage.reads.length, 0); assert.equal(f.storage.writes.length, 0); assert.equal(f.uploads, 0);
  f.adopt('old', false); assert.equal(f.context.composerReady, false);
  f.adopt(); f.adopt(); assert.equal(f.context.composerReady, true); assert.equal(f.textarea.value, 'text before identity');
  assert.equal(f.form.handlers.get('submit').size, 2, 'one preidentity guard and one active send handler');
  const sending = f.submit(); await settle(); assert.equal(f.effects.length, 1, f.failures.map(error => error.message).join('; '));
  assert.equal(f.effects[0].action, 'send'); assert.deepEqual(Array.from(f.effects[0].args.at(-1)), []);
  f.replies[0].resolve({steered: false}); await sending; await settle();
  assert.equal(f.uploads, 0); assert(f.storage.writes.length > 0);
  assert(f.storage.reads.concat(f.storage.writes).every(key => key.includes('.pi.' + encodeURIComponent(pageScope) + '.actual-neutral-thread')));
  assert.equal(app.messageReceiptLabel({state: 'accepted'}, 'pi'), 'Sent');
  assert.equal(app.messageReceiptLabel({state: 'accepted'}), 'Sent to Codex');
});

test('Pi exact-key restoration stays paused and replacement with equal thread cannot replay or change drafts', async () => {
  const store = new Storage(), id = '55555555-5555-4555-8555-555555555555';
  const prefix = 'workspace-portal.message-dispatch.2026-11-11-test.';
  store.setItem(prefix + 'old', JSON.stringify({schema: 1, entries: [{id, action: 'send', message: 'old', attachmentIds: [], state: 'waiting'}]}));
  store.setItem(prefix + 'pi.' + encodeURIComponent(pageScope) + '.actual-neutral-thread', JSON.stringify({schema: 1,
    entries: [{id, action: 'send', message: 'saved', attachmentIds: [], state: 'unknown'}]}));
  const f = ordinaryComposer({storage: store}); f.adopt(); await settle();
  assert.equal(f.effects.length, 0); assert.equal(f.context.messageDispatcher.entries[0].message, 'saved');
  const fresh = ordinaryComposer(); fresh.adopt(); const sending = fresh.submit(); await settle();
  fresh.textarea.value = 'later draft'; fresh.body.dataset.conversationScope = 'pi-workflow-v1:' + 'e'.repeat(64);
  fresh.adopt(); const ui = fresh.ui;
  fresh.replies[0].resolve({steered: false}); await sending; await settle();
  assert.equal(fresh.effects.length, 1); assert.equal(fresh.textarea.value, 'later draft'); assert.equal(fresh.ui, ui);
  assert.equal(fresh.context.composerReady, false); assert.equal(fresh.form.handlers.get('submit').size, 1);
  const saved = [...fresh.storage.values].find(([key]) => key.includes('.send-attempt.'));
  assert(saved[0].includes(encodeURIComponent(pageScope))); assert.equal(JSON.parse(saved[1]).state, 'accepted');
  const changedThread = ordinaryComposer(); changedThread.adopt(); changedThread.adopt('replacement');
  assert.equal(changedThread.context.composerReady, false); assert.equal(changedThread.effects.length, 0);
  const codex = ordinaryComposer({backend: 'codex'}); assert.equal(codex.context.composerReady, true); assert.equal(codex.uploads, 1);
  assert(codex.storage.reads.every(key => !key.includes('.pi.')));
});

function detailsLane(f = ordinaryComposer()) {
  const warnings = [], calls = [], recoveries = []; let reply = {sessionBackend: 'pi', conversationScope: pageScope,
    conversationInteractive: true, conversationHeld: false, readyMembers: ['implementer4'], repositoriesHTML: ''};
  const selector = {options: [{value: 'lead'}, {value: 'implementer4'}], replaceChildren() {throw Error('unexpected member retarget');}};
  Object.assign(f.context, {detailsTimer: null, detailsRead: null, artifactList: {}, pageReads: app.createReadScope(),
    refreshPolicy: {detailsDeadlineMs: 1000, detailsMs: 5000}, threadStatusBadge: {capture: () => ({}), recovery() {}},
    teamSettingsGeneration: 0, selectedMember: 'implementer4',
    document: {hidden: false, getElementById: id => id === 'codex-member' ? selector : null, querySelector: () => null},
    lastRepositoriesHTML: '', lastClustersHTML: '', lastServicesHTML: '', lastTeamHTML: '', repositoryReviewLoading: null,
    applyCount() {}, applyTeamCount() {}, refreshLifecycleTarget() {}, reconcileCompletedArchive: () => false,
    applyRecoveryStatus: (...args) => recoveries.push(args), restartConversationSync() {}, updateTeamWork() {}, refreshTeamStats() {},
    detailsNotice: {success() {}, failure: error => warnings.push(error)}, setTimeout: () => 1, clearTimeout() {},
    location: {replace() {throw Error('Pi redirected to lead');}},
  });
  const scoped = createPiConversationScope({body: f.body, fetchRequest: async (path, opts) => {
    calls.push({path, opts}); return response(200, await reply);
  }});
  f.context.client.details = app.createSessionClient('2026-11-11-test', app.createRequest(scoped.fetch), {}, 'implementer4', 'observation').details;
  vm.runInContext(section('  const refreshSessionDetails =', '  const suspendPageReads =') + '\nglobalThis.details = refreshSessionDetails;', f.context);
  return {...f, warnings, calls, recoveries, details: () => f.context.details(), set payload(value) {reply = value;}};
}

test('Pi details preserves snapshot-bound composer, scoped transport and strict ordinary flags without P6', async () => {
  const f = detailsLane(); await f.details();
  assert.equal(f.context.composerReady, false, 'details cannot adopt first thread identity');
  f.adopt(); await f.details();
  assert.equal(f.context.currentThreadId, 'actual-neutral-thread'); assert.equal(f.context.composerReady, true);
  assert.equal(f.context.interactive, true); assert.equal(f.context.executionHeld, false); assert.equal(f.recoveries.length, 0);
  assert.equal(f.calls[0].opts.headers.get('X-Workspace-Conversation-Scope'), pageScope);
  assert.equal(f.calls[0].opts.headers.get('X-Session-Observation'), 'observation');
  assert(f.calls[0].path.endsWith('member=implementer4'));
  f.payload = {sessionBackend: 'pi', conversationScope: pageScope, conversationInteractive: false, conversationHeld: true,
    readyMembers: ['implementer4'], repositoriesHTML: ''}; await f.details();
  assert.equal(f.context.interactive, false); assert.equal(f.context.executionHeld, true);
  f.payload = {sessionBackend: 'pi', conversationScope: pageScope, conversationInteractive: 'true', conversationHeld: false};
  await f.details(); assert.equal(f.context.interactive, false); assert.equal(f.context.executionHeld, true); assert(f.warnings.length);
  assert.equal(f.effects.length, 0); assert.equal(f.uploads, 0);
});

test('Pi stale details, scope replacement and selected-member removal never retarget or unlock', async () => {
  for (const change of ['scope', 'member', 'late', 'late-error']) {
    const f = detailsLane(); f.adopt(); const pending = deferred();
    if (change.startsWith('late')) {
      f.payload = pending.promise; const reading = f.details(); await settle(); f.invalidate();
      if (change === 'late-error') pending.reject(Error('old read failed'));
      else pending.resolve({sessionBackend: 'pi', conversationScope: pageScope, conversationInteractive: true, conversationHeld: false});
      await reading;
      assert.equal(f.warnings.length, 0, 'retired details cannot change the current warning lane');
    } else {
      f.payload = {sessionBackend: 'pi', conversationScope: change === 'scope' ? 'pi-workflow-v1:' + 'e'.repeat(64) : pageScope,
        conversationInteractive: true, conversationHeld: false, readyMembers: change === 'member' ? [] : ['implementer4']};
      await f.details();
    }
    assert.equal(f.context.composerReady, false); assert.equal(f.context.interactive, false);
    assert.equal(f.context.executionHeld, true); assert.equal(f.body.dataset.conversationScope, pageScope);
    assert.equal(f.context.currentThreadId, 'actual-neutral-thread'); assert.equal(f.effects.length, 0);
  }
  const codex = detailsLane(ordinaryComposer({backend: 'codex'}));
  codex.payload = {interactive: true, recovery: {state: 'idle'}, repositoriesHTML: '', readyMembers: ['implementer4']};
  await codex.details(); assert.equal(codex.recoveries.length, 1, 'explicit Codex keeps recovery normalization');
});

test('nonstring Pi thread and retired interrupt replies cannot enable or update the composer', async () => {
  const invalid = ordinaryComposer(); invalid.adopt(42);
  assert.equal(invalid.context.composerReady, false); assert.equal(invalid.context.currentThreadId, '');
  assert.equal(invalid.textarea.value, 'text before identity'); assert.equal(invalid.storage.reads.length, 0);
  for (const fails of [false, true]) {
    const f = ordinaryComposer(); f.adopt();
    const interrupting = [...f.interrupt.handlers.get('click')][0](); await settle();
    f.invalidate(); const ui = f.ui, refreshes = f.refreshes;
    if (fails) f.replies[0].reject(Error('late interrupt failure')); else f.replies[0].resolve({});
    await interrupting; assert.equal(f.ui, ui); assert.equal(f.refreshes, refreshes); assert.equal(f.alerts.length, 0);
  }
});

test('actual sync read rejects a retired scope before the first identity can be adopted', async () => {
  const f = fixture(), page = mountedPage(f), snapshot = deferred(); await settle();
  page.context.pagingHelpersAvailable = false; page.context.pagingUnavailable = false; page.context.settingsWriteGeneration = 0;
  page.context.client.thread = () => snapshot.promise;
  const reading = page.hooks[0].read(new AbortController().signal);
  page.context.piPageScope.invalidate(); snapshot.resolve({threadId: 'same-returned-id', entries: [], status: 'idle'});
  await assert.rejects(reading, /identity changed/); page.events.get('pagehide')();
});

test('legacy team stats requires explicit Codex and its panel despite a nonlead selector', async () => {
  for (const [codexBackend, hasPanel, expected] of [[false, false, 0], [false, true, 0], [true, false, 0], [true, true, 1]]) {
    const calls = [], panel = {classList: {contains: () => false}, querySelectorAll: () => []};
    const context = vm.createContext({codexBackend, slug: '2026-11-11-test', interactive: true, executionHeld: false,
      document: {hidden: false, getElementById: () => hasPanel ? panel : null, querySelector: () => ({value: 'implementer4'})},
      pageReads: app.createReadScope(), teamStatsRead: null,
      request: async path => {calls.push(path); return {members: []};}, refreshTeamLeadSettings() {}, updateTeamWork() {}});
    vm.runInContext(section('  const refreshTeamStats =', '  document.querySelector(\'[data-session-tab="team"]\')') +
      '\nglobalThis.refresh = refreshTeamStats;', context);
    await context.refresh(); assert.equal(calls.length, expected);
    if (expected) assert.equal(calls[0], '/api/sessions/2026-11-11-test/team-stats');
  }
});
