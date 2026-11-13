const {test} = require('node:test');
const assert = require('node:assert/strict');
const loadQuestions = () => import('../internal/web/static/pi-questions.js');
const loadPlan = () => import('../internal/web/static/pi-plan.js');
class Storage { values = new Map(); getItem(k) { return this.values.get(k) || null; } setItem(k,v) { this.values.set(k,v); } }
const container = () => ({children: [], append(node) { this.children.push(node); node.parentElement = this; }});
const snapshot = (revision, latestResponse = null) => ({revision, questions: [{id: 'q', revision: 1,
  canRespond: true, status: 'pending', latestResponse,
  answers: Object.fromEntries((latestResponse?.answers || []).map(a => [a.questionId, a])),
  questions: [{id: '0', title: 'Which?', options: [{id: '0', label: 'One'}]}]}]});
const settle = async () => { for (let i = 0; i < 30; i++) await Promise.resolve(); };
test('question controller retains exact lost reply and checks without resend, then explicit correction', async () => {
  const {mountPiQuestions} = await loadQuestions(); const storage = new Storage(); let card, sends = 0, checks = 0;
  let server = snapshot(1), serial = 0;
  const api = {read: async () => server, respond: async value => { sends++; server = snapshot(2, {...value, sequence: 1, status: 'pending'}); throw Error('lost'); },
    responseStatus: async id => { checks++; assert.equal(id, 'r1'); return server.questions[0].latestResponse; }};
  const controller = mountPiQuestions({root: container(), api, storage, namespace: 'exact', randomUUID: () => 'r' + (++serial),
    renderCard: props => { card = props; return {root: {}, update() {}}; }});
  controller.update(server); card.onSubmit(); await settle(); assert.equal(sends, 0, 'no implicit recommendation');
  card.onDraft({choice: '0', note: 'free text'}); card.onSubmit(); await settle(); assert.equal(sends, 1); assert.equal(card.state().locked, true);
  card.onSubmit(); await settle(); assert.equal(sends, 1);
  card.onCheck(); await settle(); assert.equal(checks, 1); assert.equal(sends, 1);
  assert.equal(controller.correct('q'), true); card.onDraft({choice: '', note: 'correction'});
  api.respond = async value => { sends++; assert.equal(value.responseId, 'r2'); assert.equal(value.supersedesResponseId, 'r1'); server = snapshot(3, {...value, sequence: 2, status: 'pending'}); };
  card.onSubmit(); await settle(); assert.equal(sends, 2);
  controller.update(snapshot(1)); assert.equal(card.state().locked, true, 'old snapshot cannot roll back');
});
test('question drafts survive stale errors and reload without automatic submit', async () => {
  const {mountPiQuestions} = await loadQuestions(); const storage = new Storage(); let card, sends = 0;
  const args = {root: container(), storage, namespace: 'reload', api: {respond: async () => { sends++; throw Error('409 stale'); }, read: async () => snapshot(1)},
    randomUUID: () => 'original', renderCard: p => { card = p; return {root: {}, update() {}}; }};
  const a = mountPiQuestions(args); a.update(snapshot(1)); card.onDraft({choice: '', note: 'kept draft'}); card.onSubmit(); await settle(); a.destroy();
  const b = mountPiQuestions({...args, root: container()}); b.update(snapshot(1)); await settle();
  assert.equal(card.draft().note, 'kept draft'); assert.equal(sends, 1); assert.equal(card.state().locked, true);
});
test('Plan controller sends exact displayed identity, never retries uncertain acceptance or enables unsupported controls', async () => {
  const {mountPiPlan} = await loadPlan(); const storage = new Storage(); let sends = 0, state;
  const api = {read: async () => value, accept: async payload => { sends++; assert.deepEqual(payload, {acceptanceId: 'acceptance', planId: 'plan', revision: 1, textSha256: 'exact-digest'}); throw Error('lost'); }, acceptanceStatus: async () => ({phase: 'held'})};
  let value = {revision: 1, canSelectMode: false, proposals: [{planId: 'plan', revision: 1, text: 'exact\r\n bytes', textSha256: 'exact-digest', complete: true, canAccept: false}], acceptances: []};
  const c = mountPiPlan({storage, namespace: 'plan', api, randomUUID: () => 'acceptance', render: (_, v) => { state = v; }});
  c.update(value); await c.accept('plan'); assert.equal(sends, 0); assert.equal(state.snapshot.canSelectMode, false);
  value = {...value, revision: 2, proposals: [{...value.proposals[0], canAccept: true}]}; c.update(value); await c.accept('plan'); await c.accept('plan'); assert.equal(sends, 1);
  await c.check(); assert.equal(sends, 1); assert.equal(state.pending.acceptanceId, 'acceptance');
});
test('answering one item leaves unanswered cards usable and corrections explicit', async () => {
  const {mountPiQuestions} = await loadQuestions(); const storage = new Storage(), cards = new Map(); let count = 0;
  let value = snapshot(1); value.questions[0].questions.push({id: '1', title: 'Second?', options: []});
  value.questions[0].answers = {};
  const api = {read: async () => value, respond: async payload => {
    const q = value.questions[0]; const old = q.latestResponse?.responseId || '';
    assert.equal(payload.supersedesResponseId, old); count++;
    value = {...value, revision: value.revision + 1, questions: [{...q,
      latestResponse: {...payload, status: 'pending'}, answers: {...q.answers,
        ...Object.fromEntries(payload.answers.map(a => [a.questionId, a]))}}]};
  }};
  const c = mountPiQuestions({root: container(), api, storage, namespace: 'partial', randomUUID: () => 'r' + count,
    renderCard: p => { cards.set(p.question.title, p); return {root: {}, update() {}}; }});
  c.update(value); const first = cards.get('Which?'), second = cards.get('Second?');
  first.onDraft({choice: '0', note: ''}); first.onSubmit(); await settle();
  assert.equal(first.state().locked, true); assert.equal(second.state().locked, false);
  second.onDraft({choice: '', note: 'second answer'}); second.onSubmit(); await settle(); assert.equal(count, 2);
  first.onSubmit(); await settle(); assert.equal(count, 2);
  assert.equal(c.correct('q', '0'), true); first.onDraft({choice: '', note: 'correction'}); first.onSubmit(); await settle();
  assert.equal(count, 3); assert.equal(value.questions[0].answers['1'].text, 'second answer');
});

const modeSnapshot = (mode = 'plan', revision = 3, digest = 'a'.repeat(64)) => ({revision: 1,
  proposals: [], acceptances: [], canSelectMode: true, mode, toolPolicyRevision: revision, toolPolicyDigest: digest});
const deferred = () => { let resolve, reject; const promise = new Promise((a, b) => {resolve = a; reject = b;}); return {promise, resolve, reject}; };
test('manual mode captures displayed CAS once and reads authoritative state without optimistic mode', async () => {
  const {mountPiPlan} = await loadPlan(); const result = deferred(), storage = new Storage(); let state, sends = 0;
  let server = modeSnapshot();
  const api = {setMode: async payload => {sends++; assert.deepEqual(payload, {mode: 'default', expectedRevision: 3, expectedDigest: 'a'.repeat(64)}); return result.promise;},
    read: async opts => {assert.equal(opts.cache, 'no-store'); return server;}};
  const c = mountPiPlan({api, storage, namespace: 'mode', render: (_, value) => {state = value;}});
  c.update(server); const pendingMode = c.selectMode('default');
  assert.equal(state.snapshot.mode, 'plan'); assert.equal(state.busy, true);
  await c.selectMode('plan'); assert.equal(sends, 1);
  server = modeSnapshot('default', 4, 'b'.repeat(64));
  result.resolve({outcome: 'applied', policy: {mode: 'default', revision: 4, digest: 'b'.repeat(64)}});
  await pendingMode; assert.equal(state.snapshot.mode, 'default'); assert.equal(state.canSelectMode, true);
  assert.equal(storage.values.size, 0, 'mode creates no persisted request or acceptance');
});
test('manual mode hides unsupported or invalid controls and excludes pending Plan acceptance', async () => {
  const {mountPiPlan} = await loadPlan(); let state, sends = 0;
  const api = {setMode: async () => {sends++;}, read: async () => modeSnapshot()};
  let c = mountPiPlan({api: {}, render: (_, value) => {state = value;}});
  c.update(modeSnapshot()); assert.equal(state.canSelectMode, false); await c.selectMode('default');
  c = mountPiPlan({api, render: (_, value) => {state = value;}});
  for (const value of [modeSnapshot('bad'), modeSnapshot('plan', 0), modeSnapshot('plan', 1.5), modeSnapshot('plan', Number.MAX_SAFE_INTEGER + 1), modeSnapshot('plan', 3, 'bad')]) {
    c.update(value); assert.equal(state.canSelectMode, false); await c.selectMode('default');
  }
  const storage = new Storage(); storage.setItem('pi-plan:pending:acceptance', JSON.stringify({acceptanceId: 'saved'}));
  c = mountPiPlan({api, storage, namespace: 'pending', render: (_, value) => {state = value;}});
  c.update(modeSnapshot()); await c.selectMode('default'); assert.equal(sends, 0); assert.equal(state.canSelectMode, false);
});
test('unknown mode cannot be cleared by pushed or delayed old GETs and never replays after reconnect', async () => {
  const {mountPiPlan} = await loadPlan();
  for (const networkLoss of [false, true]) {
    let state, sends = 0; const old = deferred(), during = deferred(), effect = deferred(), storage = new Storage();
    const reads = [old.promise, during.promise]; let server = modeSnapshot();
    const api = {setMode: async () => {sends++; return effect.promise;}, read: async opts => {
      assert.equal(opts.cache, 'no-store'); return reads.length ? reads.shift() : server;
    }};
    const args = {api, storage, namespace: 'uncertain', render: (_, value) => {state = value;}};
    const c = mountPiPlan(args); c.update(server);
    const beforeRequest = c.refresh(), changing = c.selectMode('default'), beforeSettlement = c.refresh();
    if (networkLoss) effect.reject(Error('lost')); else effect.resolve({outcome: 'unknown'});
    await changing; assert.equal(state.modeUncertain, true); assert.equal(state.canSelectMode, false);
    server = modeSnapshot('default', 4, 'b'.repeat(64)); c.update(server);
    assert.equal(state.modeUncertain, true, 'pushed update is not an explicit refresh');
    during.resolve(server); await beforeSettlement; assert.equal(state.modeUncertain, true, 'read begun before settlement cannot clear');
    old.resolve(modeSnapshot()); await beforeRequest;
    assert.equal(state.snapshot.mode, 'default', 'pre-request GET cannot restore an old policy');
    await c.selectMode('default'); assert.equal(sends, 1);
    await c.refresh(); assert.equal(state.modeUncertain, false); assert.equal(state.canSelectMode, true);
    c.destroy(); const reconnected = mountPiPlan(args); reconnected.update(server); await settle();
    assert.equal(sends, 1, 'reconnect/update do not POST a mode request'); reconnected.destroy();
  }
});
