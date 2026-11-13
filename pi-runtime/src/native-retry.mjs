import { statSync } from 'node:fs';
import { isAbsolute, normalize } from 'node:path';
import { isDeepStrictEqual as equal } from 'node:util';
import { syncHistoryPrefix } from './persistence.mjs';

const provider = 'dev-workspace-router';
const hex = /^[0-9a-f]{64}$/;
const id = /^[a-z0-9][a-z0-9_-]{0,62}$/;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const operation = /^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/;
const levels = ['off', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'];
const categories = ['configuration', 'authentication', 'refresh_storage', 'model_unavailable', 'rate_limit', 'overload', 'quota_billing', 'transport', 'structured_policy', 'context_limit', 'text_refusal', 'cancelled', 'unknown'];
const text = (value) => typeof value === 'string' && value.length > 0 && Buffer.byteLength(value) <= 4096 && !/[\x00-\x1f\x7f]/.test(value);
const exact = (value, keys) => value && typeof value === 'object' && !Array.isArray(value)
  && equal(Object.keys(value).sort(), [...keys].sort());
class NativeProofError extends Error {}
class NativeRoutingRefusal extends Error {}
function requireValue(condition, reason) { if (!condition) throw new NativeProofError(reason); }
// Compare the complete public entry's persisted representation, as undefined
// optional live-message properties are intentionally absent after JSON storage.
const persistedEqual = (a, b) => JSON.stringify(a) === JSON.stringify(b);
function freeze(value) { if (value && typeof value === 'object') { for (const item of Object.values(value)) freeze(item); Object.freeze(value); } return value; }
const copy = (value) => structuredClone(value);
const counters = (state) => ({ ChainIndex: state.chainIndex, ExtraAttempts: state.extraAttempts, Hops: state.hops });
const targetModel = (runtime, target) => runtime.getModel(target.Provider, target.Model);
function targetOK(target) {
  return exact(target, ['Provider', 'AccountRef', 'Model', 'Thinking']) && id.test(target.Provider)
    && target.Provider !== provider && id.test(target.AccountRef) && text(target.Model) && levels.includes(target.Thinking);
}
function submissionOK(ref, conversation) {
  return exact(ref, ['Conversation', 'OperationID', 'Action', 'Digest']) && equal(ref.Conversation, conversation)
    && operation.test(ref.OperationID) && ref.Action === 'send' && hex.test(ref.Digest);
}

// A leaf SDK component. Production must inject original receipt/deadline/effect
// authority; public native history alone never establishes host Accepted.
export function createNativeRetryRouting({ sdk, modelRuntime, sessionManager, policy: inputPolicy,
  runtimeRef, handle, prepareNativeRetry, certifyFailure, certifyEffects,
  syncHistory = syncHistoryPrefix, onHold }) {
  const policy = freeze(copy(inputPolicy));
  requireValue(exact(policy, ['Conversation', 'Settings', 'CatalogDigest', 'PolicyDigest', 'Targets', 'MaxExtraAttempts', 'MaxHops', 'FallbackOn']), 'native policy shape');
  requireValue(hex.test(policy.CatalogDigest) && hex.test(policy.PolicyDigest) && policy.Settings?.PolicyRevision === policy.PolicyDigest
    && Number.isSafeInteger(policy.Settings.Revision) && policy.Settings.Revision > 0, 'native policy identity');
  requireValue(Array.isArray(policy.Targets) && policy.Targets.length > 0 && policy.Targets.length <= 8 && policy.Targets.every(targetOK)
    && new Set(policy.Targets.map((t) => JSON.stringify([t.Provider, t.AccountRef, t.Model]))).size === policy.Targets.length, 'native physical targets');
  requireValue(Number.isInteger(policy.MaxExtraAttempts) && policy.MaxExtraAttempts >= 0 && policy.MaxExtraAttempts <= 7
    && Number.isInteger(policy.MaxHops) && policy.MaxHops >= 0 && policy.MaxHops <= policy.MaxExtraAttempts
    && Array.isArray(policy.FallbackOn) && policy.FallbackOn.every((c) => categories.includes(c))
    && new Set(policy.FallbackOn).size === policy.FallbackOn.length, 'native policy bounds');
  const primary = policy.Targets[0];
  requireValue(exact(policy.Conversation, ['Session', 'Backend', 'Member', 'Incarnation', 'NativeID'])
    && exact(policy.Conversation.Session, ['Workspace', 'Slug', 'Incarnation']) && policy.Conversation.Backend === 'pi'
    && exact(policy.Settings, ['Provider', 'AccountRef', 'Model', 'Thinking', 'PolicyRevision', 'Revision'])
    && exact(handle, ['Conversation', 'Generation']) && exact(runtimeRef, ['Session', 'InstanceID', 'RequestID', 'Digest', 'Generation'])
    && ['Provider', 'AccountRef', 'Model', 'Thinking'].every((k) => primary[k] === policy.Settings[k])
    && equal(handle?.Conversation, policy.Conversation) && handle.Generation === runtimeRef?.Generation
    && equal(runtimeRef.Session, policy.Conversation.Session) && hex.test(handle.Generation)
    && typeof policy.Conversation.Session.Workspace === 'string' && isAbsolute(policy.Conversation.Session.Workspace)
    && normalize(policy.Conversation.Session.Workspace) === policy.Conversation.Session.Workspace
    && /^[0-9]{4}-[0-9]{2}-[0-9]{2}-[a-z0-9][a-z0-9-]*$/.test(policy.Conversation.Session.Slug)
    && hex.test(policy.Conversation.Session.Incarnation) && hex.test(policy.Conversation.Incarnation)
    && /^(lead|[a-z][a-z0-9]{0,31}[0-9]+)$/.test(policy.Conversation.Member)
    && uuid.test(policy.Conversation.NativeID) && uuid.test(runtimeRef.InstanceID) && uuid.test(runtimeRef.RequestID) && hex.test(runtimeRef.Digest)
    && sessionManager.getSessionId() === policy.Conversation.NativeID, 'native subject mismatch');
  const subject = freeze(copy({ Runtime: runtimeRef, Handle: handle }));
  const selection = freeze({ Provider: provider, Model: `fallback-${policy.PolicyDigest}`, Thinking: primary.Thinking, PolicyDigest: policy.PolicyDigest });
  const partial = new WeakMap(); const responses = new Map();
  let session; let unsubscribe; let active; let held = false; let stopped = false;
  const dispatches = []; const charges = [];
  const stateType = sdk.VIRTUAL_MODEL_STATE_ENTRY;
  requireValue(typeof stateType === 'string', 'public native state export unavailable');

  async function refuse(reason, stop = false) {
    if (stop) stopped = true; else held = true;
    try { await onHold?.({ Outcome: stop ? 'stop' : 'held', Reason: reason }); } catch { /* Diagnostic failure grants no route. */ }
    throw new NativeRoutingRefusal(stop ? 'native routing stopped' : 'native routing held');
  }
  function checkSession() {
    const retry = session?.settingsManager.getRetrySettings(); const transport = session?.settingsManager.getProviderRetrySettings();
    requireValue(session && session.model?.provider === provider && session.model.id === selection.Model && session.thinkingLevel === primary.Thinking
      && retry.enabled === true && Number.isInteger(retry.maxRetries) && retry.maxRetries >= 0 && retry.maxRetries <= policy.MaxExtraAttempts
      && transport.maxRetries === 0 && session.settingsManager.getCompactionEnabled() === false, 'native private settings changed');
  }
  function snapshot() {
    const file = sessionManager.getSessionFile(); const stat = statSync(file);
    const disk = sdk.SessionManager.open(file);
    const branch = disk.getBranch();
    requireValue(stat.isFile() && disk.getSessionId() === policy.Conversation.NativeID && disk.getCwd() === sessionManager.getCwd()
      && persistedEqual(branch, sessionManager.getBranch()) && persistedEqual(disk.getEntries(), branch), 'native history mismatch');
    let parent = null;
    for (const entry of branch) { requireValue(entry.parentId === parent, 'nonlinear native history'); parent = entry.id; }
    return { file, stat, disk, branch, leaf: disk.getLeafId() };
  }
  function latestState(view) {
    return view.branch.findLast((e) => e.type === 'custom' && e.customType === stateType
      && e.data?.provider === provider && e.data.modelId === selection.Model);
  }
  function checkState(state) {
    requireValue(exact(state, ['schema', 'input', 'nativeSessionId', 'userEntryId', 'policyDigest', 'settingsRevision', 'chainIndex', 'extraAttempts', 'hops', 'lastFailureEntryId'])
      && state.schema === 1 && exact(state.input, ['operationId', 'action', 'digest'])
      && equal(state.input, { operationId: active.Submission.OperationID, action: active.Submission.Action, digest: active.Submission.Digest })
      && state.nativeSessionId === policy.Conversation.NativeID && state.userEntryId === active.witness.entryId
      && state.policyDigest === policy.PolicyDigest && state.settingsRevision === policy.Settings.Revision, 'native input state mismatch');
    requireValue(Number.isInteger(state.chainIndex) && state.chainIndex >= 0 && state.chainIndex < policy.Targets.length
      && state.chainIndex === state.extraAttempts && state.chainIndex === state.hops
      && state.extraAttempts <= policy.MaxExtraAttempts && state.hops <= policy.MaxHops
      && (state.chainIndex === 0 ? state.lastFailureEntryId === '' : text(state.lastFailureEntryId)), 'native counter mismatch');
  }
  function checkUser(view) {
    const user = view.disk.getEntry(active.witness.entryId);
    requireValue(user && equal(user, active.user) && user.type === 'message' && user.message.role === 'user', 'original native user changed');
    requireValue(view.branch.filter((e) => e.type === 'message' && e.message.role === 'user' && view.branch.indexOf(e) >= view.branch.findIndex((e) => e.id === user.id)).length === 1, 'additional native user');
  }
  function charge(previous, state) {
    requireValue(!held && !stopped && Date.now() < active.deadline, 'original native deadline/cancellation');
    checkState(state);
    requireValue(persistedEqual(snapshot().branch, previous.branch), 'history changed before charge');
    const entryId = sessionManager.appendCustomEntry(stateType, { provider, modelId: selection.Model, state: copy(state) });
    const entry = sessionManager.getEntry(entryId); const persisted = snapshot();
    requireValue(entry?.parentId === previous.leaf && persistedEqual(persisted.disk.getEntry(entryId), entry)
      && equal(entry.data, { provider, modelId: selection.Model, state }) && persisted.leaf === entryId
      && persistedEqual(persisted.branch, [...previous.branch, entry])
      && persisted.stat.dev === previous.stat.dev && persisted.stat.ino === previous.stat.ino, 'native charge verification failed');
    // No await between independent whole-entry/branch verification and both
    // file+directory storage barriers. Never return route.state a second time.
    syncHistory(persisted.file, persisted.stat);
    charges.push({ entryId, state: copy(state) });
    return entry;
  }
  async function bounded(task, signal) {
    requireValue(active && Date.now() < active.deadline && !signal?.aborted, 'original native deadline/cancellation');
    const timeout = AbortSignal.timeout(Math.max(1, active.deadline - Date.now()));
    const combined = signal ? AbortSignal.any([signal, timeout]) : timeout;
    let aborted;
    const failure = new Promise((_, reject) => { aborted = () => reject(new Error('native authority deadline')); combined.addEventListener('abort', aborted, { once: true }); });
    try { return await Promise.race([Promise.resolve().then(() => task(combined)), failure]); }
    finally { combined.removeEventListener('abort', aborted); }
  }
  async function effects(view, signal) {
    requireValue(typeof certifyEffects === 'function', 'effect authority unavailable');
    const proof = await bounded((s) => certifyEffects({ Submission: copy(active.Submission), Generation: active.Generation, branch: copy(view.branch), signal: s }), signal);
    requireValue(exact(proof, ['PriorEffectsComplete', 'NoPendingEffects']) && proof.PriorEffectsComplete === true && proof.NoPendingEffects === true, 'effects unproved');
    return proof;
  }
  function dispatch(state, signal) {
    checkSession();
    requireValue(!held && !stopped && Date.now() < active.deadline && !signal?.aborted, 'original native deadline/cancellation');
    const target = policy.Targets[state.chainIndex]; const model = targetModel(modelRuntime, target);
    requireValue(model && model.api !== 'pi-virtual', 'physical model unavailable');
    dispatches.push({ Provider: target.Provider, AccountRef: target.AccountRef, Model: target.Model, Thinking: target.Thinking });
    return { model, thinkingLevel: target.Thinking };
  }
  async function route(request) {
    try {
      requireValue(!held && !stopped && active && Date.now() < active.deadline && !request.signal?.aborted, 'native routing unavailable');
      checkSession();
      requireValue(['user', 'continuation', 'retry'].includes(request.reason), 'direct or summary routing unavailable');
      if (request.reason === 'user') {
        requireValue(!active.started, 'second user route unavailable');
        const admitted = await bounded((signal) => active.admit({ Submission: copy(active.Submission), Generation: active.Generation, deadline: active.deadline, signal }), request.signal);
        requireValue(exact(admitted, ['Submission', 'Generation', 'Accepted', 'Witness']) && admitted.Accepted === true
          && equal(admitted.Submission, active.Submission) && admitted.Generation === active.Generation, 'original Accepted receipt unavailable');
        const view = snapshot(); const witness = admitted.Witness; const user = view.disk.getEntry(witness?.entryId);
        requireValue(exact(witness, ['sessionId', 'entryId', 'parentId', 'previousLeafId', 'effectiveMessage'])
          && user?.type === 'message' && user.message.role === 'user' && witness.sessionId === policy.Conversation.NativeID
          && user.parentId === witness.parentId && text(witness.effectiveMessage) && Array.isArray(user.message.content)
          && user.message.content.length === 1 && user.message.content[0].type === 'text' && user.message.content[0].text === witness.effectiveMessage, 'original user witness unavailable');
        let parent = user.parentId;
        while (parent !== witness.previousLeafId) { const entry = view.disk.getEntry(parent); requireValue(entry?.type === 'message' && entry.message.role === 'system', 'original user ancestry'); parent = entry.parentId; }
        active.witness = freeze(copy(witness)); active.user = freeze(copy(user)); active.started = true;
        checkUser(view); await effects(view, request.signal);
        const state = { schema: 1, input: { operationId: active.Submission.OperationID, action: active.Submission.Action, digest: active.Submission.Digest }, nativeSessionId: policy.Conversation.NativeID,
          userEntryId: witness.entryId, policyDigest: policy.PolicyDigest, settingsRevision: policy.Settings.Revision, chainIndex: 0, extraAttempts: 0, hops: 0, lastFailureEntryId: '' };
        charge(view, state); return dispatch(state, request.signal);
      }
      requireValue(active.started, 'native input not initialized');
      const view = snapshot(); checkUser(view);
      const stateEntry = latestState(view); requireValue(stateEntry && equal(stateEntry.data.state, request.state), 'native branch state mismatch');
      const state = copy(stateEntry.data.state); checkState(state);
      const proof = await effects(view, request.signal);
      if (request.reason === 'continuation') { requireValue(persistedEqual(snapshot().branch, view.branch), 'continuation history changed'); return dispatch(state, request.signal); }
      const failed = request.failed;
      requireValue(failed && targetModel(modelRuntime, policy.Targets[state.chainIndex])?.id === failed.model?.id
        && failed.model.provider === policy.Targets[state.chainIndex].Provider && failed.message?.stopReason === 'error', 'failed physical response unavailable');
      const entry = view.branch.findLast((e) => e.type === 'message' && persistedEqual(e.message, failed.message));
      requireValue(entry && view.branch.indexOf(entry) > view.branch.indexOf(stateEntry) && entry.id !== state.lastFailureEntryId, 'failed entry already charged or unknown');
      const seen = partial.get(failed.message) ?? responses.get(failed.message.responseId);
      requireValue(seen && !seen.partial && !seen.tool && failed.message.content.every((b) => b.type === 'text' && b.text === ''), 'partial or tool failure');
      requireValue(typeof certifyFailure === 'function' && typeof prepareNativeRetry === 'function', 'retry authority unavailable');
      const failure = await bounded((signal) => certifyFailure({ message: copy(failed.message), entry: copy(entry), target: copy(policy.Targets[state.chainIndex]), signal }), request.signal);
      requireValue(exact(failure, ['FailureCategory', 'FailureCode', 'HTTPStatus', 'FailureEvidenceRef']) && text(failure.FailureEvidenceRef), 'failure attribution unavailable');
      if (!policy.FallbackOn.includes(failure.FailureCategory) || !((failure.FailureCategory === 'rate_limit' && failure.FailureCode === 'rate_limit_exceeded' && failure.HTTPStatus === 429)
        || (failure.FailureCategory === 'overload' && failure.FailureCode === 'overloaded' && failure.HTTPStatus === 503))) return refuse('unsupported failure', true);
      const evidence = { Submission: copy(active.Submission), Generation: active.Generation, PolicyDigest: policy.PolicyDigest, UserEntryID: active.witness.entryId, StateEntryID: stateEntry.id, FailedEntryID: entry.id,
        Counters: counters(state), Actual: copy(policy.Targets[state.chainIndex]), ...failure, Accepted: true, NativeRetry: true, FailedResponseEmpty: true, FailedToolsAbsent: true, ...proof, LinearHistory: true, Cancelled: false };
      const decision = await bounded((signal) => prepareNativeRetry({ ...copy(subject), Evidence: evidence }, { signal, deadline: active.deadline }), request.signal);
      requireValue(exact(decision, ['Outcome', 'Reason', 'Target', 'Counters']) && text(decision.Reason), 'native decision shape');
      if (decision.Outcome !== 'allow') {
        requireValue(['held', 'stop'].includes(decision.Outcome) && decision.Target === null && equal(decision.Counters, counters(state)), 'invalid refused decision');
        return refuse(decision.Reason, decision.Outcome === 'stop');
      }
      requireValue(!request.signal?.aborted && Date.now() < active.deadline && state.chainIndex + 1 < policy.Targets.length
        && state.extraAttempts < policy.MaxExtraAttempts && state.hops < policy.MaxHops
        && equal(decision.Target, policy.Targets[state.chainIndex + 1]) && equal(decision.Counters, { ChainIndex: state.chainIndex + 1, ExtraAttempts: state.extraAttempts + 1, Hops: state.hops + 1 }), 'invalid native allowance');
      const next = { ...state, chainIndex: state.chainIndex + 1, extraAttempts: state.extraAttempts + 1, hops: state.hops + 1, lastFailureEntryId: entry.id };
      charge(view, next); return dispatch(next, request.signal);
    } catch (error) {
      if (error instanceof NativeRoutingRefusal) throw error;
      return refuse(error instanceof NativeProofError ? error.message : 'native proof or charge unavailable');
    }
  }
  requireValue(policy.Targets.every((t) => targetModel(modelRuntime, t)?.api !== 'pi-virtual' && targetModel(modelRuntime, t)), 'physical target unavailable');
  requireValue(!modelRuntime.getModel(provider, selection.Model), 'native routing selection collision');
  modelRuntime.registerVirtualModel({ provider, id: selection.Model, name: 'Private bounded fallback', thinkingLevels: [primary.Thinking], route });
  return {
    selection,
    async attach(value) {
      requireValue(!session && value.sessionManager === sessionManager && value.modelRuntime === modelRuntime && !value.isStreaming, 'native session mismatch');
      const retry = value.settingsManager.getRetrySettings(); const transport = value.settingsManager.getProviderRetrySettings();
      requireValue(retry.enabled && retry.maxRetries <= policy.MaxExtraAttempts && transport.maxRetries === 0 && value.settingsManager.getCompactionEnabled() === false, 'unbounded retry or compaction');
      session = value;
      unsubscribe = session.subscribe((event) => {
        const message = event.message;
        if (message?.role !== 'assistant') return;
        const seen = partial.get(message) ?? responses.get(message.responseId) ?? { partial: false, tool: false };
        for (const block of message.content ?? []) { if ((block.type === 'text' && block.text) || (block.type === 'thinking' && block.thinking)) seen.partial = true; if (block.type === 'toolCall') seen.tool = true; }
        partial.set(message, seen); if (message.responseId) responses.set(message.responseId, seen);
      });
      await session.setModel(modelRuntime.getModel(provider, selection.Model), { persist: false });
      session.setThinkingLevel(primary.Thinking, { persist: false });
      checkSession();
      return copy(selection);
    },
    beginInput(input) {
      requireValue(exact(input, ['Submission', 'Generation', 'deadline', 'admit']) && !active && !held && !stopped && submissionOK(input.Submission, policy.Conversation)
        && input.Generation === subject.Handle.Generation && Number.isSafeInteger(input.deadline) && input.deadline > Date.now() && typeof input.admit === 'function', 'trusted original input unavailable');
      requireValue(!sessionManager.getEntries().some((e) => e.type === 'custom' && e.customType === stateType && e.data?.state?.input?.operationId === input.Submission.OperationID), 'original input cannot resume');
      requireValue(session && !session.isStreaming, 'native session unavailable');
      checkSession(); responses.clear();
      active = { Submission: freeze(copy(input.Submission)), Generation: input.Generation, deadline: input.deadline, admit: input.admit, started: false };
    },
    endInput() { requireValue(session?.isIdle && !held && !stopped, 'native input unresolved'); active = null; },
    async cancelInput() { stopped = true; await session?.abort(); },
    inspect() { return copy({ selection, held, stopped, dispatches, charges }); },
    route,
    dispose() { requireValue(!session?.isStreaming, 'native session busy'); unsubscribe?.(); },
  };
}
