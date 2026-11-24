import {mountPiQuestions} from './pi-questions.js';
import {mountPiPlan} from './pi-plan.js?v=2';

const uuid = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/;
const validDescriptor = value => value && Object.keys(value).sort().join(',') === 'backend,basePath,member,namespace' &&
  value.backend === 'pi' && typeof value.basePath === 'string' && /^\/api\/sessions\/\d{4}-\d{2}-\d{2}-[a-z0-9][a-z0-9-]*$/.test(value.basePath) &&
  typeof value.member === 'string' && /^(?:lead|[a-z][a-z0-9]{0,31}[0-9]+)$/.test(value.member) && typeof value.namespace === 'string' &&
  value.namespace.length > 0 && value.namespace.length <= 4096 && !/[\x00-\x1f\x7f]/.test(value.namespace);
const aborted = () => Object.assign(new Error('Workflow view is no longer current'), {name: 'AbortError'});

// This owns only the supplied Pi roots. The page owner skips Codex listeners
// before mounting and supplies an exact server-authored durable descriptor.
export function mountPiWorkflows({descriptor, roots = {}, fetchRequest, storage, createQuestionCard, conversation = null, onChange = () => {}} = {}) {
  if (!validDescriptor(descriptor) || typeof fetchRequest !== 'function') return {refresh: async () => {}, destroy() {}};
  let destroyed = false, mutationEpoch = 0, planRead = 0, latestPlanRead = 0, refreshID = 0;
  let questions, plan, questionSnapshot, planSnapshot, planState, dismissed = '', readError = '';
  let captureInflight = false, decisionVisible = false, feedbackOwner = 'mode', statusReadFailed = false;
  const requests = new Set(), listeners = [];
  const statusChecks = new Set();
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
      let value;
      try { value = await request('pi-plan'); }
      catch (error) {
        if (!destroyed && error?.name !== 'AbortError' && epoch === mutationEpoch && sequence >= latestPlanRead) statusReadFailed = true;
        throw error;
      }
      if (epoch !== mutationEpoch || sequence < latestPlanRead) throw aborted();
      if (!Number.isSafeInteger(value?.revision) || value.revision < 0 || !Array.isArray(value.proposals) || !Array.isArray(value.acceptances)) {
        statusReadFailed = true; throw Error('Plan data is unavailable.');
      }
      latestPlanRead = sequence; statusReadFailed = false; planSnapshot = value; return value;
    },
    accept: value => request('pi-plan/acceptances', {method: 'POST', body: value}),
    acceptanceStatus: async id => {
      try { const value = await request(`pi-plan/acceptances/${statusID(id)}`); statusReadFailed = false; return value; }
      catch (error) { if (!destroyed && error?.name !== 'AbortError') statusReadFailed = true; throw error; }
    },
    setMode: value => request('pi-plan/mode', {method: 'POST', body: value, rejectedMode: true}),
  };
  const modeButtons = [];
  if (roots.mode) {
    roots.mode.replaceChildren(); roots.mode.hidden = true;
    for (const mode of ['plan', 'default']) {
      const control = button(mode === 'plan' ? 'Plan' : 'Default', async () => {
        feedbackOwner = 'mode'; onChange({decisionVisible,proposals:planState ? proposalDisplays(planState) : [],selectionPending:true});
        if (await plan?.selectMode(mode) && !destroyed) onChange({decisionVisible,proposals:planState ? proposalDisplays(planState) : [],selectionConfirmed:{collaborationMode:mode}});
      });
      if (mode === 'plan') control.classList.add('plan-mode');
      control.dataset.piMode = mode; roots.mode.append(control); modeButtons.push(control);
    }
  }
  let planStatus, implement, keep;
  let feedbackText, refreshControl;
  if (roots.modeStatus) {
    feedbackText = node('span');
    refreshControl = button('Refresh', () => { void refresh({explicit: true}); });
  }
  if (roots.plan) {
    planStatus = roots.plan.querySelector('#plan-status');
    implement = roots.plan.querySelector('#plan-implement-same');
    keep = roots.plan.querySelector('#plan-keep-planning');
    const newSession = roots.plan.querySelector('#plan-implement-new');
    const help = roots.plan.querySelector('#plan-help');
    if (newSession) newSession.hidden = true;
    if (help) help.hidden = true;
    if (implement && keep && planStatus) {
      listen(implement, async () => {
        const selected = planState?.snapshot.proposals.at(-1); if (!selected || !decisionVisible) return;
        feedbackOwner = 'plan'; captureInflight = true;
        try { await plan.accept(selected.planId); }
        finally { captureInflight = false; renderPlan(roots.plan, planState); }
      });
      listen(keep, () => {
        dismissed = planState?.snapshot.proposals.at(-1)?.planId || ''; renderPlan(roots.plan, planState);
      });
    }
    roots.plan.hidden = true;
  }
  const phaseMessages = {
    prepared: 'Implementation request saved. Waiting to send.',
    mode_pending: 'Implementation preparation is unconfirmed.',
    dispatch_pending: 'Implementation request delivery is unconfirmed.',
    held: 'Implementation request on hold. Delivery is unconfirmed.',
    accepted: 'Implementation request accepted.',
  };
  const proposalDisplays = state => state.snapshot.proposals.map(proposal => {
    const matches = value => value?.planId === proposal.planId && value.revision === proposal.revision && value.textSha256 === proposal.textSha256;
    const saved = state.snapshot.acceptances.find(matches), local = matches(state.pending) ? state.pending : null;
    const request = saved || local;
    return {...proposal, request: request ? {...request,
      phase: saved?.phase || (captureInflight && state.busy ? 'saving' : 'unconfirmed'),
      statusText: saved ? (Object.hasOwn(phaseMessages, saved.phase) ? phaseMessages[saved.phase] : 'Implementation request status is unavailable.') :
        captureInflight && state.busy ? 'Saving implementation request...' : 'Implementation request status is unconfirmed.',
      warning: statusReadFailed ? 'Could not refresh plan status. Showing the last known information.' : '',
      disabled: state.busy || statusChecks.has(request.acceptanceId)} : null};
  });
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
    decisionVisible = Boolean(roots.plan && selected && ((captureInflight && state.busy && state.pending) ||
      (!state.pending && !accepted && selected.planId !== dismissed && selected.complete && selected.canAccept && !state.modeUncertain &&
        state.snapshot.mode === 'plan' && conversation?.ready && conversation.status === 'idle' &&
        conversation.latestTurnId && conversation.planIds?.includes(selected.planId))));
    if (roots.plan && planStatus && implement && keep) {
      roots.plan.hidden = !decisionVisible;
      if (selected) {
        planStatus.textContent = captureInflight && state.busy ? 'Saving implementation request...' :
          (feedbackOwner === 'plan' && !state.pending && !accepted ? state.error : '');
        planStatus.hidden = !planStatus.textContent;
        implement.hidden = Boolean(accepted || state.pending);
        implement.disabled = !decisionVisible || state.busy || state.modeUncertain || !selected.complete || !selected.canAccept;
        keep.disabled = state.busy;
      }
    }
    onChange({decisionVisible, proposals: planState ? proposalDisplays(planState) : []});
  }
  function renderFeedback(state) {
    if (!roots.modeStatus) return;
    feedbackText.textContent = (feedbackOwner === 'mode' || state?.modeUncertain ? state?.error : '') || readError;
    refreshControl.disabled = Boolean(state?.busy);
    roots.modeStatus.replaceChildren(feedbackText, ...((state?.modeUncertain || readError) ? [refreshControl] : []));
    roots.modeStatus.hidden = !feedbackText.textContent;
  }
  async function checkRequest(original) {
    if (destroyed || planState?.busy || statusChecks.has(original.acceptanceId)) return;
    feedbackOwner = 'plan';
    statusChecks.add(original.acceptanceId); renderPlan(roots.plan, planState);
    try {
      if (planState?.pending?.acceptanceId === original.acceptanceId) await plan.check();
      else { await planAPI.acceptanceStatus(original.acceptanceId); await refresh({explicit: true}); }
    }
    catch (error) { showReadError(error); }
    finally { statusChecks.delete(original.acceptanceId); renderPlan(roots.plan, planState); }
  }
  const questionChanged = () => {
    if (destroyed) return;
    if (roots.questions && questions) {
      questions.renderPinned(roots.questions); roots.questions.hidden = roots.questions.children.length === 0;
    }
    onChange({decisionVisible, proposals: planState ? proposalDisplays(planState) : []});
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
  return {refresh, updateConversation(value) {
    if (JSON.stringify(conversation) === JSON.stringify(value)) return;
    conversation = value; renderPlan(roots.plan, planState);
  }, checkRequest(id) {
    const original = [...(planState?.snapshot.acceptances || []), planState?.pending].find(value => value?.acceptanceId === id);
    if (original) return checkRequest({...original});
  }, destroy() {
    if (destroyed) return;
    destroyed = true;
    for (const controller of requests) controller.abort();
    requests.clear(); questions?.destroy(); plan.destroy();
    for (const [target, action] of listeners) target.removeEventListener('click', action);
    if (planStatus) { planStatus.textContent = ''; planStatus.hidden = true; }
    decisionVisible = false;
    for (const control of [implement, keep]) if (control) control.disabled = true;
    for (const [name, root] of Object.entries(roots)) if (root) {
      if (name !== 'plan') root.replaceChildren();
      root.hidden = true;
    }
  }};
}
