import { existsSync, statSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { syncHistoryPrefix } from './persistence.mjs';
import { installLinearGuards } from './lineage.mjs';
import { createToolPolicy } from './tool-policy.mjs';
import { setImmediate as immediate } from 'node:timers/promises';
import { isDeepStrictEqual } from 'node:util';
import { createHash } from 'node:crypto';
import { assertAccountBinding, checkModelsFile, assertStoredAccount } from './accounts.mjs';

export class WorkerSession {
  constructor(sdk, session, manager, options = {}) {
    this.historyDirectory = options.historyDirectory; this.syncHistory = options.syncHistory ?? syncHistoryPrefix;
    this.sdk = sdk; this.session = session; this.manager = manager;
    this.pending = null; this.held = false; this.listeners = new Set();
    session.subscribe((event) => {
      if (event.type === 'message_start' || event.type === 'message_update') this.liveMessage = event.message.role === 'assistant' ? event.message : null;
      if (event.type === 'message_end') this.liveMessage = null;
      if (event.type === 'message_end' && event.message.role === 'user' && this.pending) {
        this.pending.observed.push(event.message);
        // Pi emits before append. Leave the event callback before reading.
        void this.observePersisted(this.pending);
      }
      for (const listener of this.listeners) listener(event);
    });
  }
  notifyWorkflow() { for (const listener of this.listeners) listener({type: 'workflow_changed'}); }
  workflowObservation() {
    if (!this.toolPolicy) throw new Error('workflow observation unavailable');
    const policy = this.toolPolicy.workflow(this.session, this.generation);
    return {nativeId: this.manager.getSessionId(), model: this.session.model?.id,
      provider: this.session.model?.provider, thinking: this.session.thinkingLevel,
      proof: policy.proof, running: policy.running, activeCalls: policy.activeCalls,
      pending: !!this.pending, streaming: this.session.isStreaming,
      hostCalls: this.hostBridge?.pending.size ?? 0, held: this.held || policy.held,
      hostTools: [...this.hostTools]};
  }
  snapshot({ before } = {}) {
    const branch = this.manager.getBranch();
    const end = before ?? branch.length;
    if (!Number.isSafeInteger(end) || end < 0 || end > branch.length) throw new Error('invalid native history position');
    let start = end; let size = 0;
    while (start > 0 && end - start < 50) {
      const next = Buffer.byteLength(JSON.stringify(branch[start - 1]));
      if (next > 512 * 1024) throw new Error('native entry exceeds display bound');
      if (size + next > 512 * 1024) break;
      size += next; start--;
    }
    return { historyStart: start, historyTotal: branch.length,
      historyDigest: createHash('sha256').update(JSON.stringify(branch)).digest('hex'), nativeId: this.manager.getSessionId(), historyFile: this.manager.getSessionFile(),
      persisted: existsSync(this.manager.getSessionFile()), leafId: this.manager.getLeafId(),
      entries: branch.slice(start, end), liveMessage: this.liveMessage, busy: !!this.pending || this.session.isStreaming || !!this.hostBridge?.pending.size,
      model: this.session.model?.id, provider: this.session.model?.provider,
      thinking: this.session.thinkingLevel, held: this.held };
  }
  unknownInput(pending) {
    if (pending.finished) return;
    pending.finished = true; this.held = true; this.notifyWorkflow(); pending.resolve({ outcome: 'Unknown' });
    void this.abort().catch(() => {});
  }
  async observePersisted(pending) {
    await immediate();
    if (this.pending !== pending || pending.finished) return;
    const entries = this.manager.getEntries();
    const users = entries.filter((entry) => !pending.before.has(entry.id)
      && entry.type === 'message' && entry.message.role === 'user');
    // Exact live object identity disambiguates repeated identical text. Public
    // persisted entry reads must also agree; text/timestamps alone never admit.
    if (pending.observed.length !== 1 || users.length !== 1 || users[0].message !== pending.observed[0]) return;
    const entry = users[0];
    const text = entry.message.content;
    if (!Array.isArray(text) || text.length !== 1 || text[0].type !== 'text' || text[0].text !== pending.message) return;
    let parent = entry.parentId;
    while (parent !== pending.leaf) {
      const ancestor = this.manager.getEntry(parent);
      if (!ancestor || pending.before.has(ancestor.id) || ancestor.type !== 'message' || ancestor.message.role !== 'system') return;
      parent = ancestor.parentId;
    }
    const file = this.manager.getSessionFile();
    if (!existsSync(file)) { this.unknownInput(pending); return; }
    let observedFile;
    try { observedFile = statSync(file); } catch { this.unknownInput(pending); return; }
    if (!observedFile.isFile() || (this.historyDirectory && dirname(file) !== this.historyDirectory)) return;
    let persisted;
    try { persisted = this.sdk.SessionManager.open(file); } catch { this.unknownInput(pending); return; }
    if (persisted.getSessionId() !== this.manager.getSessionId()) return;
    const disk = persisted.getEntry(entry.id);
    if (!disk || JSON.stringify(disk) !== JSON.stringify(entry)) return;
    // No await between exact native reopen and both storage sync completions.
    try { this.syncHistory(file, observedFile); } catch {
      this.unknownInput(pending); return;
    }
    pending.finished = true;
    const witness = { sessionId: persisted.getSessionId(), entryId: entry.id, parentId: entry.parentId,
      previousLeafId: pending.leaf, effectiveMessage: pending.message };
    if (this.activeInput && this.activeInput.operationID === pending.operationID) this.activeInput.witness = witness;
    pending.resolve({ outcome: 'Accepted', witness });
  }
  async send(message, operationID) {
    if (typeof message !== 'string' || !message || Buffer.byteLength(message) > 65536) {
      return { outcome: 'NotSubmitted', refusal: 'invalid text input' };
    }
    if (this.held) return { outcome: 'Unknown' };
    if (this.toolPolicy && !this.toolPolicy.inspect(this.session, this.generation).Applied) return { outcome: 'Unknown' };
    if (this.pending || this.session.isStreaming || this.hostBridge?.pending.size) return { outcome: 'NotSubmitted', refusal: 'worker busy' };
    const pending = { operationID, before: new Set(this.manager.getEntries().map((entry) => entry.id)),
      leaf: this.manager.getLeafId(), observed: [], message, finished: false };
    const result = new Promise((resolve) => { pending.resolve = resolve; });
    this.pending = pending; this.notifyWorkflow();
    this.activeInput = operationID ? { operationID, witness: null } : null;
    this.run = this.session.prompt(message, { expandPromptTemplates: false }).catch(() => {}).finally(async () => {
      await this.observePersisted(pending);
      if (!pending.finished) { this.held = true; pending.resolve({ outcome: 'Unknown' }); }
      if (this.pending === pending) this.pending = null;
      if (this.activeInput?.operationID === operationID) this.activeInput = null;
      this.notifyWorkflow();
    });
    return result;
  }
  verifyHistory({ Witness: witness, Message: message }) {
    const file = this.manager.getSessionFile();
    if (!existsSync(file) || !statSync(file).isFile()) return { Verified: false };
    const disk = this.sdk.SessionManager.open(file);
    if (disk.getSessionId() !== witness.SessionID || disk.getCwd() !== this.manager.getCwd()) return { Verified: false };
    const entry = disk.getEntry(witness.EntryID);
    if (!entry || entry.parentId !== witness.ParentID || entry.type !== 'message' || entry.message.role !== 'user') return { Verified: false };
    const text = entry.message.content;
    if (!Array.isArray(text) || text.length !== 1 || text[0].type !== 'text' || text[0].text !== message) return { Verified: false };
    let parent = entry.parentId;
    while ((parent ?? '') !== witness.PreviousLeafID) {
      const ancestor = disk.getEntry(parent);
      if (!ancestor || ancestor.type !== 'message' || ancestor.message.role !== 'system') return { Verified: false };
      parent = ancestor.parentId;
    }
    return { Verified: true };
  }
  async abort() { await this.session.abort(); return { idle: !this.session.isStreaming, held: this.held }; }
  async hold() {
    this.held = true; this.notifyWorkflow();
    this.hostBridge?.cancelAll();
    if (this.pending && !this.pending.finished) {
      this.pending.finished = true;
      this.pending.resolve({ outcome: 'Unknown' });
    }
    return this.abort();
  }
  inspectToolPolicy() {
    if (!this.toolPolicy) throw new Error('tool policy unavailable');
    return this.toolPolicy.inspect(this.session, this.generation);
  }
  async applyToolPolicy(target) {
    if (!this.toolPolicy) throw new Error('tool policy unavailable');
    if (this.held || this.pending || this.session.isStreaming || this.hostBridge?.pending.size) return { Outcome: 'rejected', Proof: this.inspectToolPolicy() };
    const result = this.toolPolicy.apply(this.session, target, this.generation);
    if (result.Outcome === 'unknown') await this.hold();
    return result;
  }
  async configure(requested) {
    const current = this.settings;
    const result = (Outcome) => ({ Requested: requested, Effective: this.settings, Outcome });
    if (this.held || this.pending || this.session.isStreaming || this.hostBridge?.pending.size) return result('rejected');
    if (requested.Revision === current.Revision) return result(isDeepStrictEqual(requested, current) ? 'applied' : 'rejected');
    if (!['off', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'].includes(requested.Thinking)) return result('rejected');
    if (requested.Revision !== current.Revision + 1 || ((requested.Provider !== current.Provider) !== (requested.AccountRef !== current.AccountRef))) return result('rejected');
    try { this.checkAccount(current); this.checkAccount(requested); } catch { return result('rejected'); }
    const model = this.runtime.getModel(requested.Provider, requested.Model);
    if (!model) return result('rejected');
    try {
      await this.session.setModel(model, { persist: false });
      this.session.setThinkingLevel(requested.Thinking, { persist: false });
      await this.storageSettings.flush();
      this.settings = { ...requested, Thinking: this.session.thinkingLevel };
      return result('applied');
    } catch {
      await this.hold();
      return result('unknown');
    }
  }
  async dispose() {
    if (this.held || this.pending || this.session.isStreaming || this.hostBridge?.pending.size) throw new Error('worker has unresolved or active work');
    this.session.dispose();
  }
}

export async function createSession(sdk, config, options = {}) {
  assertAccountBinding(config.accountBindings, config.settings);
  const modelsPath = join(config.agentDir, 'models.json');
  if (!options.modelRuntime) checkModelsFile(modelsPath);
  let manager;
  if (config.historyFile) {
    if (!existsSync(config.historyFile) || !statSync(config.historyFile).isFile()) throw new Error('mapped native history is missing');
    manager = sdk.SessionManager.open(config.historyFile);
    if (manager.getSessionId() !== config.nativeId || manager.getCwd() !== config.cwd) throw new Error('native mapping mismatch');
  } else {
    if (!config.emptyProven) throw new Error('empty session lacks no-dispatch/drain proof');
    manager = sdk.SessionManager.create(config.cwd, config.historyDir, { id: config.nativeId });
  }
  const modelRuntime = options.modelRuntime ?? await sdk.ModelRuntime.create({
    authPath: join(config.authDirectory, 'auth.json'), modelsPath, allowModelNetwork: false,
  });
  if (!options.modelRuntime) assertStoredAccount(modelRuntime, config.settings.Provider);
  const settings = sdk.SettingsManager.create(config.agentDir, config.agentDir);
  settings.setRetryEnabled(false);
  let worker;
  const toolPolicy = config.toolPolicy ? createToolPolicy(config.toolPolicy, {changed: () => worker?.notifyWorkflow()}) : null;
  if (toolPolicy && (typeof config.generation !== 'string' || !/^[0-9a-f]{64}$/.test(config.generation))) throw new Error('tool policy generation unavailable');
  const resourceLoader = new sdk.DefaultResourceLoader({ cwd: config.cwd, agentDir: config.agentDir,
    settingsManager: settings, extensionFactories: [toolPolicy ? toolPolicy.extensionFactory : async (pi) => installLinearGuards(pi)], noExtensions: true, noSkills: true, noPromptTemplates: true, noThemes: true,
    ...(options.resourceOptions ?? {}) });
  await resourceLoader.reload();
  const model = modelRuntime.getModel(config.settings.Provider, config.settings.Model);
  if (!model) throw new Error('configured model unavailable');
  const { session } = await sdk.createAgentSession({ cwd: config.cwd, agentDir: config.agentDir,
    modelRuntime, model, thinkingLevel: config.settings.Thinking,
    sessionManager: manager, settingsManager: settings, resourceLoader,
    ...(options.sessionOptions ?? {}), ...(toolPolicy ? { tools: [...config.toolPolicy.RegisteredCeiling] } : {}) });
  await session.bindExtensions({});
  await session.setModel(model, { persist: false });
  session.setThinkingLevel(config.settings.Thinking, { persist: false });
  await settings.flush();
  worker = new WorkerSession(sdk, session, manager, { historyDirectory: config.historyDir, syncHistory: options.syncHistory });
  worker.runtime = modelRuntime; worker.storageSettings = settings;
  worker.checkAccount = (requested) => {
    assertAccountBinding(config.accountBindings, requested);
    if (!options.modelRuntime) { checkModelsFile(modelsPath); assertStoredAccount(modelRuntime, requested.Provider); }
  };
  worker.settings = { ...config.settings, Thinking: session.thinkingLevel };
  worker.toolPolicy = toolPolicy; worker.generation = config.generation;
  worker.hostTools = [...(config.hostTools ?? [])];
  if (toolPolicy) {
    const applied = toolPolicy.apply(session, config.toolPolicy, config.generation);
    if (applied.Outcome !== 'applied' || !applied.Proof.Applied) throw new Error('initial tool policy unresolved');
  }
  return worker;
}
