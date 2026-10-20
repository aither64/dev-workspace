const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');
const source = fs.readFileSync(path.join(__dirname, 'static/coordination.js'), 'utf8');
const settle = async () => { for (let i = 0; i < 12; i++) await Promise.resolve(); };

function harness(operations = false, initiallyActive = true) {
  const session = operations === 'session';
  class Element {
    constructor(tag = 'div') { this.tag = tag; this.children = []; this.value = ''; this.hidden = true; this.dataset = {}; this.className = ''; }
    set textContent(value) { this.value = value; this.children = []; }
    get textContent() { return this.value + this.children.map(child => child.textContent).join(''); }
    append(...children) { for (const child of children) { child.parent = this; this.children.push(child); } }
    replaceChildren(...children) { this.value = ''; this.children = []; this.append(...children); }
    setAttribute(key, value) { this[key] = value; }
    get classList() { return {contains: value => this.className.split(' ').includes(value)}; }
    matches(selector) {
      return selector.startsWith('details') ? this.tag === 'details' && this.dataset.coordinationKey && (!selector.endsWith('[open]') || this.open)
        : selector === 'summary' ? this.tag === 'summary'
        : selector.startsWith('.') ? this.classList.contains(selector.slice(1)) : this.tag === selector;
    }
    querySelectorAll(selector) { return this.children.flatMap(child => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]); }
    querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
    closest(selector) { return this.matches(selector) ? this : this.parent?.closest(selector); }
    focus() { document.activeElement = this; }
  }
  const panel = new Element(), warning = new Element(), cards = [], requests = [], timers = new Map();
  const tabPanel = new Element(), tab = new Element('a'); tabPanel.className = initiallyActive ? 'tab-panel active' : 'tab-panel'; tabPanel.append(panel);
  const tabLabel = new Element('span'); tabLabel.className = 'rail-label'; tab.append(tabLabel);
  if (session) Object.assign(panel.dataset, {coordinationSession: 'holder', coordinationObservation: 'incarnation-a'});
  const docListeners = new Map(), windowListeners = new Map();
  let nextTimer = 0;
  const document = {
    hidden: false, activeElement: null,
    querySelector: selector => ({'[data-coordination]': operations ? panel : null,
      '[data-coordination-warning]': warning, '[data-session-list]': operations ? null : {},
      '[data-session-tab="reservations"]': session ? tab : null})[selector],
    querySelectorAll: selector => selector === '[data-session-slug]' ? cards : [],
    createElement: tag => new Element(tag), createDocumentFragment: () => new Element(),
    addEventListener: (name, action) => docListeners.set(name, action),
  };
  function addCard(slug, observationIdentity = "incarnation-a") {
    const badge = new Element();
    cards.push({dataset: {sessionSlug: slug, sessionObservation: observationIdentity}, querySelector: () => badge});
    return badge;
  }
  const badge = addCard('holder'), unrelated = addCard('other');
  vm.runInNewContext(source, {document, URL, AbortController,
    window: {addEventListener: (name, action) => windowListeners.set(name, action)},
    fetch: (url, options) => new Promise((resolve, reject) => requests.push({url, ...options, resolve, reject})),
    setTimeout: (action, delay) => { const id = ++nextTimer; timers.set(id, {action, delay}); return id; },
    clearTimeout: id => timers.delete(id),
  });
  return {panel, warning, badge, unrelated, requests, addCard, cards, document, tab, tabLabel,
    selectTab: (visible, event = 'click') => { tabPanel.className = visible ? 'tab-panel active' : 'tab-panel'; docListeners.get(event)?.(); },
    respond: async (payload, index = requests.length - 1) => { requests[index].resolve({ok: true, json: async () => payload}); await settle(); },
    fail: async () => { requests.at(-1).resolve({ok: false}); await settle(); },
    tick: async () => { const entry = [...timers].find(([, timer]) => timer.delay === 5000); assert.ok(entry, 'poll is scheduled'); timers.delete(entry[0]); entry[1].action(); await settle(); },
    dispatch: name => docListeners.get(name)(),
    hide: () => { document.hidden = true; docListeners.get('visibilitychange')(); },
    show: () => { document.hidden = false; docListeners.get('visibilitychange')(); },
    leave: () => windowListeners.get('pagehide')(),
    pollCount: () => [...timers.values()].filter(timer => timer.delay === 5000).length,
  };
}
function payload(machines = [{machine: 'fixture:machine', state: 'deployed_review'}]) {
  return {schema: 1, workspaces: [{owners: [], jobs: [], reservations: [], foreignReservations: [], notifications: [],
    reservationHolders: machines.length ? [{ownerId: 'owner', slug: 'holder', observationIdentity: 'incarnation-a', portalUrl: 'https://workspace.test/holder/', machines}] : []}]};
}

test('failed reads retain reservations, refreshed cards inherit badges, and confirmed release removes them', async () => {
  const h = harness();
  assert.equal(h.requests.length, 1);
  assert.equal(h.requests[0].url, '/api/coordination');
  await h.respond(payload());
  assert.equal(h.badge.hidden, false);
  assert.equal(h.badge.textContent, 'machine reserved');
  assert.match(h.badge['aria-label'], /fixture:machine.*review/);
  assert.equal(h.unrelated.hidden, true);
  await h.tick(); await h.fail();
  assert.equal(h.badge.hidden, false);
  assert.equal(h.warning.hidden, false);
  const refreshed = h.addCard('holder'); h.dispatch('session-list-updated');
  assert.equal(refreshed.hidden, false);
  assert.match(refreshed.title, /unavailable/);
  await h.tick(); await h.respond(payload([]));
  assert.equal(h.badge.hidden, true);
  assert.equal(refreshed.hidden, true);
  assert.equal(h.warning.hidden, true);
});

test('hidden and navigated pages abort reads and reject late responses from an earlier page lifetime', async () => {
  const h = harness();
  h.hide(); assert.equal(h.requests[0].signal.aborted, true);
  assert.equal(h.pollCount(), 0);
  h.show(); assert.equal(h.requests.length, 2);
  await h.respond(payload([{machine: 'fixture:one', state: 'held'}, {machine: 'fixture:two', state: 'blocked_recovery'}]), 1);
  assert.equal(h.badge.textContent, '2 machines reserved');
  await h.respond(payload([]), 0);
  assert.equal(h.badge.hidden, false);
  assert.equal(h.badge.textContent, '2 machines reserved');
  await h.tick(); h.leave();
  assert.equal(h.requests.at(-1).signal.aborted, true);
  await h.respond(payload([]));
  assert.equal(h.badge.hidden, false);
  assert.equal(h.pollCount(), 0);
});

test('Operations preserves active-first history and keeps its successful display after a failed refresh', async () => {
  const h = harness(true), data = payload();
  data.workspaces[0].jobs = [
    {id: 'active-123', kind: 'merge', state: 'running'},
    {id: 'done-456', kind: 'profile_activate', state: 'succeeded'},
  ];
  await h.respond(data);
  assert.ok(h.panel.textContent.indexOf('Running and waiting') < h.panel.textContent.indexOf('Recent results'));
  assert.match(h.panel.textContent, /Update workspace application/);
  assert.match(h.panel.textContent, /Time unavailable/);
  const before = h.panel.textContent;
  await h.tick(); await h.fail();
  assert.equal(h.panel.textContent, before);
  assert.equal(h.warning.hidden, false);
});

test('named and plural badges list distinct machines and states in their tooltip', async () => {
  const h = harness();
  await h.respond(payload([{machine: 'fixture:one', shortName: 'buildhost', state: 'held'}]));
  assert.equal(h.badge.textContent, 'buildhost reserved');
  await h.tick(); await h.respond(payload([
    {machine: 'fixture:one', shortName: 'buildhost', state: 'held'},
    {machine: 'fixture:two', shortName: 'worker', state: 'deployed_review'},
    {machine: 'fixture:two', shortName: 'worker', state: 'deployed_review'},
  ]));
  assert.equal(h.badge.textContent, '2 machines reserved');
  assert.match(h.badge.title, /buildhost.*Reserved for deployment\nworker.*Held for review/);
  assert.equal(h.badge.title, h.badge['aria-label']);
});

test('multiple workspaces preserve full machine details and match same-slug badges by incarnation', async () => {
  const h = harness(true), data = payload([{id: 'reservation', machine: 'fixture:one', shortName: 'buildhost', state: 'deployed_review', deployments: 4}]);
  const other = payload([{machine: 'fixture:two', state: 'held'}]).workspaces[0];
  other.reservationHolders[0].observationIdentity = 'incarnation-b';
  other.foreignReservations = [{machine: 'fixture:one', state: 'deployed_review', portalUrl: 'https://workspace.test/holder/'}];
  data.workspaces.push(other);
  await h.respond(data);
  assert.match(h.panel.textContent, /buildhost.*4 recorded deployment steps/);
  assert.equal(h.badge.textContent, 'buildhost reserved');
});

test('session menu tracks reservations while another section is selected and renders cached details on opening', async () => {
  const h = harness('session', false);
  assert.equal(h.requests.length, 1);
  assert.equal(h.requests[0].url, '/api/sessions/holder/coordination');
  await h.respond(payload().workspaces[0]);
  assert.equal(h.tabLabel.textContent, 'Reservations (1)');
  assert.equal(h.tab.title, 'Reservations (1)');
  assert.equal(h.tab['aria-label'], h.tab.title);
  assert.equal(h.panel.textContent, '', 'closed panel is not rendered');
  await h.tick();
  const data = payload([
    {machine: 'fixture:one', state: 'held'}, {machine: 'fixture:two', state: 'blocked_recovery'},
    {machine: 'fixture:two', state: 'blocked_recovery'}, {machine: 'fixture:pending', state: 'requested'},
    {machine: 'fixture:released', state: 'released'},
  ]).workspaces[0];
  data.reservationHolders.push({slug: 'other', observationIdentity: 'incarnation-a', machines: [{machine: 'foreign', state: 'held'}]},
    {slug: 'holder', observationIdentity: 'incarnation-b', machines: [{machine: 'old', state: 'held'}]});
  data.counts = {reservations: 200};
  await h.respond(data);
  assert.equal(h.tabLabel.textContent, 'Reservations (2)', 'complete holder summary supplies the count');
  assert.equal(h.panel.textContent, '');
  h.selectTab(true, 'session-section-change');
  assert.match(h.panel.textContent, /Held for recovery/);
  assert.equal(h.requests.length, 2, 'opening renders cache without another request');
  h.selectTab(false, 'session-section-change');
  assert.equal(h.pollCount(), 1, 'leaving the panel keeps menu refreshes running');
  await h.tick(); await h.respond(payload([]).workspaces[0]);
  assert.equal(h.tabLabel.textContent, 'Reservations (0)');
  h.selectTab(true, 'session-section-change');
  assert.match(h.panel.textContent, /This session does not hold any machines/);
});

test('session menu retains successful counts through failed and malformed reads and clears warnings on recovery', async () => {
  const h = harness('session', false);
  await h.fail();
  assert.equal(h.tabLabel.textContent, 'Reservations', 'initial failure does not claim zero');
  assert.match(h.tab.title, /unavailable/);
  await h.tick(); await h.respond(payload().workspaces[0]);
  assert.equal(h.tabLabel.textContent, 'Reservations (1)');
  await h.tick(); await h.fail();
  assert.equal(h.tabLabel.textContent, 'Reservations (1)');
  assert.match(h.tab.title, /unavailable.*Last known count/);
  assert.equal(h.tab['aria-label'], h.tab.title);
  const malformed = payload().workspaces[0]; malformed.reservationHolders[0].machines = null;
  await h.tick(); await h.respond(malformed);
  assert.equal(h.tabLabel.textContent, 'Reservations (1)');
  await h.tick(); await h.respond(payload().workspaces[0]);
  assert.equal(h.tab.title, 'Reservations (1)');
  assert.equal(h.warning.hidden, true);
});

test('session pane and menu fence both machines and history by incarnation', async () => {
  const h = harness('session'), data = payload().workspaces[0];
  data.owners = [{id: 'owner', slug: 'holder', observationIdentity: 'incarnation-a', portalUrl: 'https://workspace.test/holder/'}];
  data.jobs = [{id: 'completed', ownerId: 'owner', kind: 'machine_activate', state: 'succeeded'}];
  await h.respond(data);
  assert.match(h.panel.textContent, /machine.*Held for review/);
  assert.match(h.panel.textContent, /Deploy system configuration/);
  assert.equal(h.tabLabel.textContent, 'Reservations (1)');
  data.owners[0].observationIdentity = 'incarnation-b';
  data.reservationHolders[0].observationIdentity = 'incarnation-b';
  await h.tick(); await h.respond(data);
  assert.match(h.panel.textContent, /This session does not hold any machines/);
  assert.match(h.panel.textContent, /No completed operations yet/);
  assert.doesNotMatch(h.panel.textContent, /Deploy system configuration/);
  assert.equal(h.tabLabel.textContent, 'Reservations (0)');
});

test('session menu aborts hidden and exited page reads and fences delayed responses', async () => {
  const h = harness('session', false);
  h.hide(); assert.equal(h.requests[0].signal.aborted, true);
  assert.equal(h.pollCount(), 0);
  h.show(); assert.equal(h.requests.length, 2);
  await h.respond(payload().workspaces[0], 1);
  await h.respond(payload([]).workspaces[0], 0);
  assert.equal(h.tabLabel.textContent, 'Reservations (1)');
  await h.tick(); h.leave();
  assert.equal(h.requests.at(-1).signal.aborted, true);
  await h.respond(payload([]).workspaces[0]);
  assert.equal(h.tabLabel.textContent, 'Reservations (1)');
  assert.equal(h.pollCount(), 0);
});

test('dashboard refresh preserves expanded details and focused disclosure', async () => {
  const h = harness(true), data = payload();
  data.workspaces[0].jobs = [{id: 'active', kind: 'merge', state: 'running', revision: 1}];
  await h.respond(data);
  let detail = h.panel.querySelector('details'); detail.open = true; detail.querySelector('summary').focus();
  await h.tick(); data.workspaces[0].jobs[0].revision = 2; await h.respond(data);
  detail = h.panel.querySelector('details');
  assert.equal(detail.open, true); assert.equal(h.document.activeElement, detail.querySelector('summary'));
});


test('a same-slug replacement never inherits cached ownership during failed reads', async () => {
  const h = harness();
  await h.respond(payload());
  assert.equal(h.badge.hidden, false);
  h.cards[0].dataset.sessionObservation = 'incarnation-b';
  h.dispatch('session-list-updated');
  assert.equal(h.badge.hidden, true);
  await h.tick(); await h.fail();
  assert.equal(h.badge.hidden, true);
  assert.equal(h.warning.hidden, false);
});

test('a delayed old reservation response cannot badge a replacement incarnation', async () => {
  const h = harness();
  h.cards[0].dataset.sessionObservation = 'incarnation-b';
  h.dispatch('session-list-updated');
  await h.respond(payload());
  assert.equal(h.badge.hidden, true);
  const current = payload();
  current.workspaces[0].reservationHolders[0].observationIdentity = 'incarnation-b';
  await h.tick(); await h.respond(current);
  assert.equal(h.badge.hidden, false);
});


test('background runs show automatic waiting and action reasons without changing reservation counts', async () => {
  const h = harness(true);
  const p = payload([]);
  p.workspaces[0].owners = [{id: 'owner', slug: 'holder', observationIdentity: 'incarnation-a', portalUrl: 'https://workspace.test/holder/'}];
  p.workspaces[0].integrations = [{id: 'run-1', ownerId: 'owner', phase: 'merging', continuation: {state: 'waiting'}},
    {id: 'run-2', ownerId: 'owner', phase: 'deploying', continuation: {state: 'needs_attention'}, reason: {message: 'Prepare an updated artifact.'}},
    {id: 'run-3', ownerId: 'owner', phase: 'prepare', continuation: {state: 'paused'}, handoff: {state: 'pending'}},
    {id: 'run-4', ownerId: 'owner', phase: 'complete', outcome: 'canceled', continuation: {state: 'complete'}}];
  await h.respond(p);
  assert.match(h.panel.textContent, /Background integrations/);
  assert.match(h.panel.textContent, /Waiting automatically/);
  assert.match(h.panel.textContent, /Agent action required/);
  assert.match(h.panel.textContent, /Prepare an updated artifact/);
  assert.match(h.panel.textContent, /Continuation is paused/);
  assert.match(h.panel.textContent, /Waiting for an agent to respond/);
  assert.match(h.panel.textContent, /Canceled before any operation was submitted/);
  assert.equal(h.badge.hidden, true);
});


test('resource claims show held ownership and waiting requests without command results', async () => {
  const h = harness(true), data = payload([]);
  data.workspaces[0].owners = [{id: 'claim-owner', slug: 'holder', observationIdentity: 'incarnation-a', portalUrl: 'https://workspace.test/holder/'}];
  data.workspaces[0].claims = [
    {id: 'held', ownerId: 'claim-owner', state: 'held', label: 'Reviewed deployment', repositories: [], machines: ['fixture:machine']},
    {id: 'waiting', ownerId: 'claim-owner', state: 'waiting', label: 'Publish feature', repositories: ['project'], machines: []},
  ];
  data.workspaces[0].reservations = [{id: 'held', ownerId: 'claim-owner', machine: 'fixture:machine', state: 'held', mode: 'claim'}];
  await h.respond(data);
  assert.match(h.panel.textContent, /Resource claims/);
  assert.match(h.panel.textContent, /Held until released/);
  assert.match(h.panel.textContent, /Waiting for resources/);
  assert.match(h.panel.textContent, /Publish feature/);
  assert.match(h.panel.textContent, /explicitly releases/);
  assert.doesNotMatch(h.panel.textContent, /Run session command|Exited successfully/);
  assert.equal(h.warning.hidden, true);
});
