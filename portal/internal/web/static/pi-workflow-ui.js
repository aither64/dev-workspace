import {mountPiQuestions} from './pi-questions.js';
import {mountPiPlan} from './pi-plan.js';

const uuid = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/;
const validDescriptor = value => value && Object.keys(value).sort().join(',') === 'backend,basePath,member,namespace' &&
  value.backend === 'pi' && typeof value.basePath === 'string' && /^\/api\/sessions\/\d{4}-\d{2}-\d{2}-[a-z0-9][a-z0-9-]*$/.test(value.basePath) &&
  typeof value.member === 'string' && /^(?:lead|[a-z][a-z0-9]{0,31}[0-9]+)$/.test(value.member) && typeof value.namespace === 'string' &&
  value.namespace.length > 0 && value.namespace.length <= 4096 && !/[\x00-\x1f\x7f]/.test(value.namespace);
const aborted = () => Object.assign(new Error('Workflow view is no longer current'), {name: 'AbortError'});

// This owns only the supplied Pi roots. The page owner skips Codex listeners
// before mounting and supplies an exact server-authored durable descriptor.
export function mountPiWorkflows({descriptor, roots = {}, fetchRequest, storage, createQuestionCard, onChange = () => {}} = {}) {
  if (!validDescriptor(descriptor) || typeof fetchRequest !== 'function') return {refresh: async () => {}, destroy() {}};
  let destroyed = false, mutationEpoch = 0, planRead = 0, latestPlanRead = 0, refreshID = 0;
  let questions, plan, questionSnapshot, planSnapshot, planState, dismissed = '', readError = '';
  const requests = new Set(), listeners = [];
  const namespace = `pi:${descriptor.basePath}:${descriptor.member}:${descriptor.namespace}`;
  const doc = Object.values(roots).find(root => root?.ownerDocument)?.ownerDocument || globalThis.document;
  const node = (tag, text = '', className = '') => {
    const value = doc.createElement(tag); value.textContent = text; if (className) value.className = className; return value;
  };
  const listen = (target, action) => {
    target.addEventListener('click', action); listeners.push([target, action]); return target;
  };
  const button = (text, action, quiet = true) => {
    const value = node('button', text, quiet ? 'quiet' : ''); value.type = 'button'; return listen(value, action);
  };
  const url = suffix => `${descriptor.basePath}/${suffix}?member=${encodeURIComponent(descriptor.member)}`;
  const statusID = id => { if (!uuid.test(id)) throw Error('Invalid workflow request'); return encodeURIComponent(id); };
  async function request(suffix, {method = 'GET', body, rejectedMode = false} = {}) {
    if (destroyed) throw aborted();
    const controller = new AbortController(); requests.add(controller);
    if (method !== 'GET') { mutationEpoch++; disableControls(); }
    try {
      const response = await fetchRequest(url(suffix), {method, cache: 'no-store', credentials: 'same-origin',
        signal: controller.signal, headers: {'Content-Type': 'application/json'},
        ...(body === undefined ? {} : {body: JSON.stringify(body)})});
      if (destroyed) throw aborted();
      let data;
      try { data = await response.json(); } catch (_) { throw Error('Could not read the workflow response.'); }
      if (destroyed) throw aborted();
      const rejected = rejectedMode && response.status === 409 && data?.outcome === 'rejected' &&
        !Object.hasOwn(data, 'policy') && Object.keys(data).length === 1;
      if (!response.ok && !rejected) throw Error('The workflow request could not be completed.');
      return data;
    } finally { requests.delete(controller); }
  }
  const questionAPI = {
    read: async () => {
      const value = await request('pi-questions');
      if (!Number.isSafeInteger(value?.revision) || value.revision < 0 || !Array.isArray(value.questions)) throw Error('Question data is unavailable.');
      questionSnapshot = value; return value;
    },
    respond: value => request('pi-questions/responses', {method: 'POST', body: value}),
    responseStatus: id => request(`pi-questions/responses/${statusID(id)}`),
  };
  const planAPI = {
    read: async () => {
      const epoch = mutationEpoch, sequence = ++planRead;
      const value = await request('pi-plan');
      if (epoch !== mutationEpoch || sequence < latestPlanRead) throw aborted();
      if (!Number.isSafeInteger(value?.revision) || value.revision < 0 || !Array.isArray(value.proposals) || !Array.isArray(value.acceptances)) throw Error('Plan data is unavailable.');
      latestPlanRead = sequence; planSnapshot = value; return value;
    },
    accept: value => request('pi-plan/acceptances', {method: 'POST', body: value}),
    acceptanceStatus: id => request(`pi-plan/acceptances/${statusID(id)}`),
    setMode: value => request('pi-plan/mode', {method: 'POST', body: value, rejectedMode: true}),
  };
  const modeButtons = [];
  if (roots.mode) {
    roots.mode.replaceChildren(); roots.mode.hidden = true;
    for (const mode of ['plan', 'default']) {
      const control = button(mode === 'plan' ? 'Plan' : 'Default', () => { void plan?.selectMode(mode); });
      if (mode === 'plan') control.classList.add('plan-mode');
      control.dataset.piMode = mode; roots.mode.append(control); modeButtons.push(control);
    }
  }
  let planText, planHeading, planStatus, implement, check, keep;
  let feedbackText, refreshControl;
  if (roots.modeStatus) {
    feedbackText = node('span');
    refreshControl = button('Refresh', () => { void refresh({explicit: true}); });
  }
  if (roots.plan) {
    planHeading = node('strong', 'Ready to implement this plan?');
    planText = node('pre'); planText.style.whiteSpace = 'pre-wrap';
    planStatus = node('p'); planStatus.setAttribute('role', 'status');
    const content = node('div'); content.append(planHeading, planText, planStatus);
    implement = button('Implement here', () => {
      const selected = planState?.snapshot.proposals.at(-1); if (selected) void plan.accept(selected.planId);
    }, false);
    check = button('Check request', () => { void checkRequest(); });
    keep = button('Keep planning', () => {
      dismissed = planState?.snapshot.proposals.at(-1)?.planId || ''; renderPlan(roots.plan, planState);
    });
    const actions = node('div', '', 'approval-actions'); actions.append(implement, check, keep);
    roots.plan.replaceChildren(content, actions); roots.plan.hidden = true;
  }
  function renderPlan(_root, state) {
    if (destroyed || !state) return;
    planState = state;
    for (const control of modeButtons) {
      control.disabled = !state.canSelectMode;
      const active = control.dataset.piMode === state.snapshot.mode;
      control.classList.toggle('active', active); control.setAttribute('aria-pressed', String(active));
    }
    if (roots.mode) roots.mode.hidden = !state.snapshot.canSelectMode && !state.modeUncertain;
    renderFeedback(state);
    const selected = state.snapshot.proposals.at(-1);
    const accepted = state.snapshot.acceptances.find(a => a.planId === selected?.planId);
    if (roots.plan) {
      roots.plan.hidden = !selected || selected.planId === dismissed || accepted?.phase === 'accepted';
      if (selected) {
        planText.textContent = selected.text;
        planStatus.textContent = state.error || readError || (accepted?.phase === 'accepted' ? 'Implementation request accepted.' : accepted ? 'Implementation request saved.' : '');
        implement.hidden = Boolean(accepted || state.pending);
        implement.disabled = state.busy || state.modeUncertain || !selected.complete || !selected.canAccept;
        check.hidden = !accepted && !state.pending;
        check.disabled = state.busy;
        keep.disabled = state.busy;
      }
    }
    onChange();
  }
  function renderFeedback(state) {
    if (!roots.modeStatus) return;
    feedbackText.textContent = state?.error || readError;
    refreshControl.disabled = Boolean(state?.busy);
    roots.modeStatus.replaceChildren(feedbackText, ...((state?.modeUncertain || readError) ? [refreshControl] : []));
    roots.modeStatus.hidden = !feedbackText.textContent;
  }
  async function checkRequest() {
    if (destroyed || planState?.busy) return;
    if (planState?.pending) { await plan.check(); return; }
    const selected = planState?.snapshot.proposals.at(-1);
    const accepted = planState?.snapshot.acceptances.find(a => a.planId === selected?.planId);
    if (!accepted) return;
    try { await planAPI.acceptanceStatus(accepted.acceptanceId); await refresh({explicit: true}); }
    catch (error) { showReadError(error); }
  }
  const questionChanged = () => {
    if (destroyed) return;
    if (roots.questions && questions) {
      questions.renderPinned(roots.questions); roots.questions.hidden = roots.questions.children.length === 0;
    }
    onChange();
  };
  if (roots.questions && typeof createQuestionCard === 'function') {
    roots.questions.replaceChildren(); roots.questions.hidden = true;
    questions = mountPiQuestions({root: roots.questions, api: questionAPI, storage, namespace, onChange: questionChanged,
      renderCard: props => {
        const card = createQuestionCard(props), [groupID, itemID] = props.id.split(':');
        const correction = button('Edit answer', () => { questions.correct(groupID, itemID); });
        const actions = node('div', '', 'approval-actions'); actions.append(correction);
        return {root: card.root, update() {
          card.update();
          const record = questionSnapshot?.questions.find(q => q.id === groupID);
          const state = props.state();
          correction.hidden = !record?.answers?.[itemID];
          correction.disabled = !record?.canRespond || state.busy || !state.locked;
          if (actions.parentElement !== card.root) card.root.append(actions);
        }};
      }});
  }
  plan = mountPiPlan({root: roots.plan, api: planAPI, storage, namespace, render: renderPlan});
  function disableControls() {
    if (questionSnapshot) questions?.update({...questionSnapshot, questions: questionSnapshot.questions.map(q => ({...q, canRespond: false}))});
    if (planSnapshot) plan?.update({...planSnapshot, canSelectMode: false, proposals: planSnapshot.proposals.map(p => ({...p, canAccept: false}))});
  }
  function showReadError(error) {
    if (destroyed || error?.name === 'AbortError') return;
    readError = 'Could not load workflow data. Refresh to check again.';
    disableControls();
    renderFeedback(planState);
  }
  async function refresh({explicit = false} = {}) {
    if (destroyed) return;
    const id = ++refreshID;
    const reads = [];
    if (questions) reads.push(questions.refresh());
    reads.push(explicit ? plan.refresh() : planAPI.read().then(value => { if (!destroyed) plan.update(value); }));
    const results = await Promise.allSettled(reads);
    if (destroyed || id !== refreshID) return;
    const failure = results.find(result => result.status === 'rejected' && result.reason?.name !== 'AbortError');
    if (failure) showReadError(failure.reason);
    else { readError = ''; renderPlan(roots.plan, planState); }
  }
  void refresh();
  return {refresh, destroy() {
    if (destroyed) return;
    destroyed = true;
    for (const controller of requests) controller.abort();
    requests.clear(); questions?.destroy(); plan.destroy();
    for (const [target, action] of listeners) target.removeEventListener('click', action);
    for (const root of Object.values(roots)) if (root) { root.replaceChildren(); root.hidden = true; }
  }};
}
