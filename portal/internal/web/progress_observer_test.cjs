const assert = require('node:assert/strict');
const tick = () => new Promise(resolve => setImmediate(resolve));
(async () => {
  const {progressPresentation, mountProgressObserver} = await import('./static/progress-observer.mjs');
  const first = {available: true, enabled: true, warning: false, concern: {key: 'concern-1', summary: '<unsafe>', recommendation: 'Run one check.', response: 'Adjust: deliver.'}};
  assert.equal(progressPresentation(first).warning, false);
  assert.match(progressPresentation(first).message, /Lead response: Adjust: deliver/);
  assert.equal(progressPresentation({...first, warning: true}).warning, true);
  assert.match(progressPresentation({available: true, enabled: true}).status, /Idle sessions stay quiet/);
  assert.equal(progressPresentation({available: false}).available, false);
  assert.match(progressPresentation({available: true, enabled: false}).status, /paused/);
  assert.match(progressPresentation({...first, unavailable: 'Model unavailable'}).status, /unavailable/);
  const storage = new Map();
  const sessionStorage = {getItem: key => storage.get(key), setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key)};
  function mount({slug = 'example', readOnly = false, failRead = false, unavailableStorage = '', lostSave = false} = {}) {
    const elements = new Map(), pageEvents = {}, browserEvents = {};
    const page = {hidden: false, getElementById: id => elements.get(id), addEventListener: (event, fn) => {pageEvents[event] = fn;}};
    for (const id of ['progress-observer', 'progress-observer-enabled', 'progress-observer-status', 'progress-observer-concern', 'progress-observer-warning', 'progress-observer-warning-toggle', 'progress-observer-warning-hide', 'progress-observer-warning-message', 'progress-observer-announcement', 'progress-observer-interval', 'progress-observer-interval-form', 'progress-observer-save', 'progress-observer-now']) {
      elements.set(id, {events: {}, attributes: {}, textWrites: 0, text: '', classList: {toggle() {}},
        addEventListener(event, fn) {this.events[event] = fn;}, setAttribute(name, value) {this.attributes[name] = value;},
        focus() {page.activeElement = this;}, reportValidity() {return true;},
        get textContent() {return this.text;}, set textContent(value) {this.text = value; this.textWrites++;},
        set innerHTML(_) {throw new Error('Untrusted progress text used as HTML');}});
    }
    const state = {backend: {...first, intervalMinutes: 15, warning: true}, failRead, writes: 0, reads: 0, bodies: []};
    let interval;
    const browser = {setTimeout, clearTimeout, setInterval(fn) {interval = fn; return 1;}, clearInterval() {}, addEventListener: (event, fn) => {browserEvents[event] = fn;},
      get sessionStorage() {
        if (unavailableStorage === 'access') throw new Error('Storage access denied');
        if (unavailableStorage === 'write') return {...sessionStorage, setItem() {throw new Error('Storage full');}, removeItem() {throw new Error('Storage full');}};
        return sessionStorage;
      }};
    mountProgressObserver({document: page, window: browser, slug, policy: {readDeadlineMs: 10000, detailsMs: 15000}, readOnly,
      createNotice: () => ({begin() {}, success() {}, failure() {}, destroy() {}}),
      request: async (_, options = {}) => {
        if (options.method === 'POST') {
          state.writes++; const body = JSON.parse(options.body); state.bodies.push(body);
          state.backend = {...state.backend, ...(body.checkNow ? {running: true} : body)};
          if (lostSave && state.writes === 1) throw new Error('Lost acknowledgement after save');
        } else {state.reads++; if (state.failRead) throw new Error('Read unavailable');}
        return state.backend;
      }});
    return {elements, page, pageEvents, browserEvents, state, refresh: async () => {interval(); await tick();}};
  }
  const fixture = mount({lostSave: true});
  const {elements, page, pageEvents, browserEvents, state} = fixture;
  await tick();
  const toggle = elements.get('progress-observer-enabled');
  assert.equal(toggle.checked, true); assert.equal(toggle.disabled, false);
  assert.match(elements.get('progress-observer-warning-message').textContent, /<unsafe>/);
  toggle.checked = false; await toggle.events.change(); await tick();
  assert.equal(state.writes, 1, 'uncertain preference write must not retry');
  assert.equal(toggle.checked, false, 'follow-up read must observe saved preference');
  const input = elements.get('progress-observer-interval');
  input.value = '7'; input.events.input();
  await fixture.refresh(); assert.equal(input.value, '7', 'polling overwrote an interval edit');
  elements.get('progress-observer-interval-form').events.submit({preventDefault() {}}); await tick();
  assert.equal(state.backend.intervalMinutes, 7); assert.equal(state.backend.enabled, false);
  assert.equal(elements.get('progress-observer-now').disabled, false, 'paused checks prevented an explicit check');
  elements.get('progress-observer-now').events.click(); await tick();
  assert.deepEqual(state.bodies.at(-1), {checkNow: true});
  assert.equal(elements.get('progress-observer-now').disabled, true, 'running check allowed a duplicate');
  assert.equal(state.backend.enabled, false, 'manual check unpaused automatic checks');
  page.hidden = true; const priorReads = state.reads; await fixture.refresh(); assert.equal(state.reads, priorReads);
  page.hidden = false; pageEvents.visibilitychange(); await tick(); assert.ok(state.reads > priorReads);
  browserEvents.pagehide();

  const local = mount({readOnly: true}); await tick();
  const element = suffix => local.elements.get('progress-observer-' + suffix);
  const preference = 'workspace-portal.progress-concern-hidden.example';
  assert.equal(element('enabled').disabled, true);
  assert.equal(element('warning-toggle').hidden, false);
  assert.equal(element('warning-toggle').attributes['aria-expanded'], 'true');
  assert.equal(element('warning').hidden, false);
  assert.equal(element('announcement').textContent, 'New progress concern');
  const announcements = element('announcement').textWrites, messageWrites = element('warning-message').textWrites;
  await local.refresh();
  assert.equal(element('announcement').textWrites, announcements, 'poll repeated the live announcement');
  assert.equal(element('warning-message').textWrites, messageWrites, 'unchanged concern text was replaced');
  element('warning-hide').events.click();
  assert.equal(element('warning').hidden, true);
  assert.equal(element('warning-toggle').hidden, false, 'hidden concern lost its discoverable control');
  assert.equal(element('warning-toggle').attributes['aria-expanded'], 'false');
  assert.equal(local.page.activeElement, element('warning-toggle'));
  assert.equal(storage.get(preference), 'concern-1', 'preference must contain only the key');
  local.state.backend = {...local.state.backend, enabled: false, concern: {...first.concern, count: 4, summary: '<script>literal</script>', response: 'New response'}};
  await local.refresh();
  local.browserEvents.focus(); await tick();
  assert.equal(element('warning').hidden, true, 'same-key update reopened hidden details');
  assert.match(element('concern').textContent, /<script>literal<\/script>/, 'Settings lost the full plain-text concern');
  assert.match(element('warning-message').textContent, /New response/);
  assert.equal(element('announcement').textWrites, announcements);
  assert.equal(local.state.writes, 0, 'local Hide changed backend settings');
  local.browserEvents.pagehide();
  const reload = mount({failRead: true}); await tick();
  const reloadedPanel = reload.elements.get('progress-observer-warning');
  assert.equal(storage.get(preference), 'concern-1', 'initial loading/read failure cleared Hide');
  reload.state.failRead = false; await reload.refresh();
  assert.equal(reloadedPanel.hidden, true, 'same tab reload lost Hide');
  const otherSession = mount({slug: 'another'}); await tick();
  assert.equal(otherSession.elements.get('progress-observer-warning').hidden, false, 'Hide leaked to another session');
  otherSession.browserEvents.pagehide();
  reload.elements.get('progress-observer-warning-toggle').events.click();
  assert.equal(reloadedPanel.hidden, false); assert.equal(storage.has(preference), false);
  reload.elements.get('progress-observer-warning-toggle').events.click();
  assert.equal(reloadedPanel.hidden, true, 'triangle did not toggle details');
  reload.state.failRead = true; await reload.refresh();
  assert.equal(reloadedPanel.hidden, true); assert.equal(storage.get(preference), 'concern-1');
  reload.state.failRead = false;
  reload.state.backend = {...reload.state.backend, concern: {...first.concern, key: 'concern-2'}};
  await reload.refresh();
  assert.equal(reloadedPanel.hidden, false, 'new concern did not open');
  reload.elements.get('progress-observer-warning-hide').events.click();
  reload.state.backend = {...reload.state.backend, warning: false, concern: null};
  await reload.refresh();
  assert.equal(storage.has(preference), false, 'authoritative resolution retained old preference');
  assert.equal(reload.elements.get('progress-observer-warning-toggle').hidden, true);
  assert.equal(reloadedPanel.hidden, true); assert.equal(reload.state.writes, 0);
  reload.browserEvents.pagehide();
  for (const unavailableStorage of ['access', 'write']) {
    const fallback = mount({unavailableStorage}); await tick();
    fallback.elements.get('progress-observer-warning-hide').events.click(); await fallback.refresh();
    assert.equal(fallback.elements.get('progress-observer-warning').hidden, true, 'storage failure lost in-memory Hide');
    fallback.elements.get('progress-observer-warning-toggle').events.click();
    assert.equal(fallback.elements.get('progress-observer-warning').hidden, false);
    assert.equal(fallback.state.writes, 0); fallback.browserEvents.pagehide();
  }
  const unkeyed = mount(); await tick();
  unkeyed.state.backend = {...unkeyed.state.backend, concern: {...first.concern, key: ''}}; await unkeyed.refresh();
  unkeyed.elements.get('progress-observer-warning-hide').events.click(); await unkeyed.refresh();
  assert.equal(unkeyed.elements.get('progress-observer-warning').hidden, true);
  assert.equal(storage.has(preference), false, 'missing concern key acquired a durable identity');
  unkeyed.browserEvents.pagehide();
  console.log('Progress settings and local concern Hide/reopen contracts passed');
})().catch(error => {console.error(error); process.exitCode = 1;});
