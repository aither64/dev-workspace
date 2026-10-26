const assert = require('node:assert/strict');
const tick = () => new Promise(resolve => setImmediate(resolve));
(async () => {
  const {progressPresentation, mountProgressObserver} = await import('./static/progress-observer.mjs');
  const first = {available: true, enabled: true, warning: false, concern: {summary: '<unsafe>', recommendation: 'Run one check.', response: 'Adjust: deliver.'}};
  assert.equal(progressPresentation(first).warning, false);
  assert.match(progressPresentation(first).message, /Lead response: Adjust: deliver/);
  assert.equal(progressPresentation({...first, warning: true}).warning, true);
  assert.match(progressPresentation({available: true, enabled: true}).status, /Idle sessions stay quiet/);
  assert.equal(progressPresentation({available: false}).available, false);
  assert.match(progressPresentation({available: true, enabled: false}).status, /paused/);
  assert.match(progressPresentation({...first, unavailable: 'Model unavailable'}).status, /unavailable/);
  const elements = new Map(), pageEvents = {}, browserEvents = {};
  for (const id of ['progress-observer', 'progress-observer-enabled', 'progress-observer-status', 'progress-observer-concern', 'progress-observer-warning', 'progress-observer-interval', 'progress-observer-interval-form', 'progress-observer-save', 'progress-observer-now']) {
    elements.set(id, {events: {}, classList: {toggle() {}}, addEventListener(event, fn) {this.events[event] = fn;},
      reportValidity() { return true; },
      set innerHTML(_) {throw new Error('Untrusted progress text used as HTML');}});
  }
  const page = {hidden: false, getElementById: id => elements.get(id), addEventListener: (event, fn) => {pageEvents[event] = fn;}};
  let interval, backend = {...first, intervalMinutes: 15, warning: true}, writes = 0, reads = 0;
  const bodies = [];
  const browser = {setTimeout, clearTimeout, setInterval(fn) {interval = fn; return 1;}, clearInterval() {}, addEventListener: (event, fn) => {browserEvents[event] = fn;}};
  mountProgressObserver({document: page, window: browser, slug: 'example', policy: {readDeadlineMs: 10000, detailsMs: 15000}, readOnly: false,
    createNotice: () => ({begin() {}, success() {}, failure() {}, destroy() {}}),
    request: async (_, options = {}) => {
      if (options.method === 'POST') {
        writes++; const body = JSON.parse(options.body); bodies.push(body);
        if (body.checkNow) backend = {...backend, running: true};
        else backend = {...backend, ...body};
        if (writes === 1) throw new Error('Lost acknowledgement after save');
        return backend;
      }
      reads++; return backend;
    }});
  await tick();
  const toggle = elements.get('progress-observer-enabled');
  assert.equal(toggle.checked, true); assert.equal(toggle.disabled, false);
  assert.match(elements.get('progress-observer-warning').textContent, /<unsafe>/);
  toggle.checked = false; await toggle.events.change(); await tick();
  assert.equal(writes, 1, 'uncertain preference write must not retry');
  assert.equal(toggle.checked, false, 'follow-up read must observe saved preference');
  const input = elements.get('progress-observer-interval');
  input.value = '7'; input.events.input();
  interval(); await tick(); assert.equal(input.value, '7', 'polling overwrote an interval edit');
  elements.get('progress-observer-interval-form').events.submit({preventDefault() {}}); await tick();
  assert.equal(backend.intervalMinutes, 7); assert.equal(backend.enabled, false);
  assert.equal(elements.get('progress-observer-now').disabled, false, 'paused checks prevented an explicit check');
  elements.get('progress-observer-now').events.click(); await tick();
  assert.deepEqual(bodies.at(-1), {checkNow: true});
  assert.equal(elements.get('progress-observer-now').disabled, true, 'running check allowed a duplicate');
  assert.equal(backend.enabled, false, 'manual check unpaused automatic checks');
  page.hidden = true; const priorReads = reads; interval(); await tick(); assert.equal(reads, priorReads);
  page.hidden = false; pageEvents.visibilitychange(); await tick(); assert.ok(reads > priorReads);
  browserEvents.pagehide();
})().catch(error => {console.error(error); process.exitCode = 1;});
