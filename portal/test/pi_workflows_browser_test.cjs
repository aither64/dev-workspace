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
