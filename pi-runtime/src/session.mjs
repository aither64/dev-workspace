import { existsSync, statSync, readFileSync, lstatSync } from 'node:fs';
import { join, dirname, isAbsolute, normalize } from 'node:path';
import { syncHistoryPrefix } from './persistence.mjs';
import { installLinearGuards, linearEntries } from './lineage.mjs';
import { createToolPolicy } from './tool-policy.mjs';
import { setImmediate as immediate } from 'node:timers/promises';
import { isDeepStrictEqual } from 'node:util';
import { createHash } from 'node:crypto';
import { assertAccountBinding, checkModelsFile, assertStoredAccount } from './accounts.mjs';
import { parseRecord } from './framing.mjs';
import { captureFork, initializeFork, verifyFork } from './fork.mjs';
import { WorkerActivity } from './activity.mjs';

// Display snapshots omit image payloads; native history and input proofs keep
// their original objects and bytes. Only copy messages that contain images.
function displayMessage(message) {
  if (!Array.isArray(message?.content) || !message.content.some(block => block.type === 'image')) return message;
  return { ...message, content: message.content.map(block => block.type === 'image'
    ? { type: 'text', text: '[Image omitted from transcript display]' } : block) };
}
function displayEntry(entry) {
  if (entry.type !== 'message') return entry;
  const message = displayMessage(entry.message);
  return message === entry.message ? entry : { ...entry, message };
}

// Compaction appends one SDK entry without replacing the captured prefix or
// changing the linear parent chain. All other non-message ancestors stay held.
function allowedInputAncestor(entry, witnessedUsers) {
  return entry?.type === 'compaction' || entry?.type === 'message' &&
    (['assistant', 'toolResult', 'system'].includes(entry.message.role) ||
      entry.message.role === 'user' && witnessedUsers.has(entry.id));
}

// Trusted init supplies only the fixed admitted package file. Validate it before
// creating native state, including on subsequent account checks; never fall back.
function checkInstalledModels(path) {
  try {
    if (typeof path !== 'string' || !isAbsolute(path) || normalize(path) !== path) throw new Error();
    const info = lstatSync(path);
    if (!info.isFile() || info.size > 65536) throw new Error();
    const bytes = readFileSync(path);
    if (bytes.length > 65536) throw new Error();
    const config = parseRecord(new TextDecoder('utf-8', {fatal: true}).decode(bytes), ['providers'], 65536);
    const keys = (value, expected) => value && !Array.isArray(value) && typeof value === 'object'
      && Object.keys(value).sort().join(',') === expected;
    if (!keys(config, 'providers') || !keys(config.providers, 'openai')
      || !(keys(config.providers.openai, 'api,baseUrl')
        || keys(config.providers.openai, 'api,baseUrl,modelOverrides')
        || keys(config.providers.openai, 'api,baseUrl,models')
        || keys(config.providers.openai, 'api,baseUrl,modelOverrides,models'))) throw new Error();
    if (Object.hasOwn(config.providers.openai, 'modelOverrides')) {
      const overrides = config.providers.openai.modelOverrides;
      if (!overrides || typeof overrides !== 'object' || Array.isArray(overrides)
        || Object.keys(overrides).length < 1 || Object.keys(overrides).length > 16) throw new Error();
      for (const [name, override] of Object.entries(overrides)) {
        if (!/^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$/.test(name)
          || !keys(override, 'samplingParams') || !keys(override.samplingParams, 'parallel_tool_calls')
          || typeof override.samplingParams.parallel_tool_calls !== 'boolean') throw new Error();
      }
    }
    if (Object.hasOwn(config.providers.openai, 'models')) {
      const models = config.providers.openai.models, ids = new Set();
      if (!Array.isArray(models) || models.length < 1 || models.length > 16) throw new Error();
      for (const model of models) {
        if (!keys(model, 'contextWindow,id,input,name,reasoning,samplingParams,thinkingLevelMap')
          || typeof model.id !== 'string' || !/^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$/.test(model.id)
          || ids.has(model.id) || Object.hasOwn(config.providers.openai.modelOverrides ?? {}, model.id)
          || typeof model.name !== 'string' || !model.name.trim()
          || !Number.isSafeInteger(model.contextWindow) || model.contextWindow <= 0 || model.contextWindow > 2000000
          || model.reasoning !== true || !Array.isArray(model.input) || model.input.length !== 2
          || model.input[0] !== 'text' || model.input[1] !== 'image'
          || !keys(model.samplingParams, 'parallel_tool_calls') || model.samplingParams.parallel_tool_calls !== false
          || !keys(model.thinkingLevelMap, 'high,low,max,medium,minimal,off,xhigh')
          || model.thinkingLevelMap.off !== null || model.thinkingLevelMap.minimal !== null
          || ['low','medium','high','xhigh','max'].some(level => model.thinkingLevelMap[level] !== level)) throw new Error();
        ids.add(model.id);
      }
    }
    const {baseUrl, api} = config.providers.openai;
    if (typeof baseUrl !== 'string' || /[\p{Cc}\s?#$`{}\\]/u.test(baseUrl) || api !== 'openai-responses') throw new Error();
    const url = new URL(baseUrl);
    if (!/^https?:\/\//.test(baseUrl) || !url.hostname || url.username || url.password
      || /[\p{Cc}]/u.test(decodeURIComponent(url.pathname))) throw new Error();
  } catch { throw new Error('installed native model configuration unavailable'); }
}

export class WorkerSession {
  #verifyInstructions;
  constructor(sdk, session, manager, options = {}) {
    this.#verifyInstructions = options.verifyInstructions;
    this.historyDirectory = options.historyDirectory; this.syncHistory = options.syncHistory ?? syncHistoryPrefix;
    this.sdk = sdk; this.session = session; this.manager = manager;
    this.pending = null; this.held = false; this.listeners = new Set();
    this.inputs = new Map(); this.acceptedInputs = new Map(); this.witnessedUsers = new Set();
    this.activity = new WorkerActivity(manager, session.isStreaming, options.activityClock);
    this.unsubscribe = session.subscribe((event) => {
      this.activity.event(event, this.held);
      if (event.type === 'message_start' || event.type === 'message_update') this.liveMessage = event.message.role === 'assistant' ? event.message : null;
      if (event.type === 'message_end') this.liveMessage = null;
      if (event.type === 'message_end' || event.type === 'agent_settled') {
        // Public message_end precedes appendMessage. Invalidate again after
        // persistence so a connected watcher cannot miss the final entry.
        void immediate().then(() => { this.activity.history(); this.notifyWorkflow(); })
          .catch(() => { this.held = true; this.notifyWorkflow(); });
      }
      if (event.type === 'message_end' && event.message.role === 'user') {
        let input = [...this.inputs.values()].find(value => value.object === event.message);
        // Idle prompt constructs its own object. There is exactly one such
        // admission, and no foreign lower-level queue is allowed.
        if (!input && this.pending && !this.pending.object && !this.pending.finished) {
          input = this.pending; input.object = event.message;
        }
        if (!input || input.observed.length) { void this.hold(); return; }
        input.observed.push(event.message);
        this.activeInput = input;
        // Pi emits before append. Leave the event callback before reading.
        void this.observePersisted(input);
      }
      for (const listener of this.listeners) listener(event);
    });
  }
  notifyWorkflow() {
    if (this.held) this.activity.transition('unclassified');
    for (const listener of this.listeners) listener({type: 'workflow_changed'});
  }
  readActivity() { this.checkInstructions(); return this.activity.read(this.held); }
  checkInstructions() {
    try { return this.#verifyInstructions?.(); }
    catch {
      this.held = true; this.notifyWorkflow();
      throw new Error('role instructions unavailable');
    }
  }
  workflowObservation() {
    if (!this.toolPolicy) throw new Error('workflow observation unavailable');
    const policy = this.toolPolicy.workflow(this.session, this.generation);
    return {nativeId: this.manager.getSessionId(), model: this.session.model?.id,
      provider: this.session.model?.provider, thinking: this.session.thinkingLevel,
      proof: policy.proof, running: policy.running, activeCalls: policy.activeCalls,
      pending: !!this.pending || !!this.inputs.size, streaming: this.session.isStreaming,
      hostCalls: this.hostBridge?.pending.size ?? 0, held: this.held || policy.held,
      hostTools: [...this.hostTools]};
  }
  assertForkQuiescent() {
    this.checkInstructions();
    if (this.held || this.pending || this.inputs.size || this.session.isStreaming || this.session.agent.hasQueuedMessages() || this.hostBridge?.pending.size) throw new Error('fork source busy');
    if (this.toolPolicy) {
      const state = this.toolPolicy.workflow(this.session, this.generation);
      if (state.held || state.running || state.activeCalls || !state.proof.Applied) throw new Error('fork source effects unresolved');
    }
  }
  forkSnapshot() {
    this.assertForkQuiescent();
    return captureFork(this.sdk, this.manager);
  }
  verifyFork(origin) { this.checkInstructions(); return verifyFork(this.sdk, this.manager, origin, this.syncHistory); }
  snapshot({ before } = {}) {
    const instructionsDigest = this.checkInstructions();
    const branch = this.manager.getBranch();
    const end = before ?? branch.length;
    if (!Number.isSafeInteger(end) || end < 0 || end > branch.length) throw new Error('invalid native history position');
    let start = end; let size = 0; const entries = [];
    while (start > 0 && end - start < 50) {
      const entry = displayEntry(branch[start - 1]);
      const next = Buffer.byteLength(JSON.stringify(entry));
      if (next > 512 * 1024) throw new Error('native entry exceeds display bound');
      if (size + next > 512 * 1024) break;
      size += next; start--; entries.unshift(entry);
    }
    return { ...(instructionsDigest ? { instructionsDigest } : {}), historyStart: start, historyTotal: branch.length,
      historyDigest: createHash('sha256').update(JSON.stringify(branch)).digest('hex'), nativeId: this.manager.getSessionId(), historyFile: this.manager.getSessionFile(),
      persisted: existsSync(this.manager.getSessionFile()), leafId: this.manager.getLeafId(),
      entries, liveMessage: displayMessage(this.liveMessage), busy: !!this.pending || !!this.inputs.size || this.session.isStreaming || !!this.hostBridge?.pending.size,
      model: this.session.model?.id, provider: this.session.model?.provider,
      thinking: this.session.thinkingLevel, held: this.held };
  }
  unknownInput(pending) {
    if (pending.finished) return;
    pending.finished = true; pending.resolve({ outcome: 'Unknown' });
    this.held = true; this.notifyWorkflow();
    void this.hold().catch(() => {});
  }
  async observePersisted(pending) {
    await immediate();
    if (this.inputs.get(pending.operationID) !== pending || pending.finished || !pending.observed.length) return;
    let entries;
    try { entries = linearEntries(this.manager); } catch { this.unknownInput(pending); return; }
    if (JSON.stringify(entries.slice(0, pending.prefixLength)) !== pending.prefixJSON) {
      this.unknownInput(pending); return;
    }
    const matches = entries.filter(entry => entry.type === 'message' && entry.message === pending.object);
    if (pending.observed.length !== 1 || matches.length !== 1) { this.unknownInput(pending); return; }
    const entry = matches[0];
    const text = entry.message.content;
    if (!isDeepStrictEqual(text, [{type: 'text', text: pending.message}])) { this.unknownInput(pending); return; }
    let parent = entry.parentId;
    while ((parent ?? '') !== (pending.leaf ?? '')) {
      const ancestor = this.manager.getEntry(parent);
      if (!allowedInputAncestor(ancestor, this.witnessedUsers)) {
        this.unknownInput(pending); return;
      }
      parent = ancestor.parentId;
    }
    const file = this.manager.getSessionFile();
    let observedFile, persisted;
    try {
      observedFile = statSync(file);
      if (!observedFile.isFile() || this.historyDirectory && dirname(file) !== this.historyDirectory) throw new Error();
      persisted = this.sdk.SessionManager.open(file);
      if (persisted.getSessionId() !== this.manager.getSessionId() || persisted.getCwd() !== this.manager.getCwd()
        || JSON.stringify(linearEntries(persisted)) !== JSON.stringify(entries)) throw new Error();
      this.syncHistory(file, observedFile);
    } catch { this.unknownInput(pending); return; }
    pending.finished = true;
    pending.witness = { sessionId: persisted.getSessionId(), entryId: entry.id, parentId: entry.parentId,
      previousLeafId: pending.leaf, effectiveMessage: pending.message };
    this.witnessedUsers.add(entry.id);
    pending.resolve({ outcome: 'Accepted', witness: pending.witness });
  }
  inputCheck(message) {
    if (typeof message !== 'string' || !message || Buffer.byteLength(message) > 65536) return {outcome: 'NotSubmitted', refusal: 'invalid text input'};
    try { this.checkInstructions(); } catch { return {outcome: 'Unknown'}; }
    if (this.held || this.toolPolicy && !this.toolPolicy.inspect(this.session, this.generation).Applied) return {outcome: 'Unknown'};
    if (this.inputs.size >= 20) return {outcome: 'NotSubmitted', refusal: 'native input capacity reached'};
    // peek returns the next selected batch, not every priority queue. Check
    // before and after each insertion; exclusive ownership is preserved.
    if (this.session.agent.peekQueuedMessages().some(object => ![...this.inputs.values()].some(input => input.object === object))) return {outcome: 'Unknown'};
  }
  makeInput(message, operationID, delivery) {
    const prefix=linearEntries(this.manager);
    const input = {operationID, message, delivery, prefixLength:prefix.length, prefixJSON:JSON.stringify(prefix),
      leaf: this.manager.getLeafId(), observed: [], finished: false, committed: false};
    input.result = new Promise(resolve => { input.resolve = resolve; });
    this.inputs.set(operationID, input);
    return input;
  }
  startPrompt(input) {
    this.pending = input; this.activeInput = input; this.notifyWorkflow();
    const run = this.session.prompt(input.message, {expandPromptTemplates: false}).catch(() => {}).finally(async () => {
      await this.observePersisted(input);
      if (!input.finished) this.unknownInput(input);
      if (this.pending === input) this.pending = null;
      // Native queue consumption can install a newer active input while the
      // original prompt's lifetime is still unwinding.
      if (!this.session.isStreaming && !this.session.agent.hasQueuedMessages()) {
        for (const value of this.inputs.values()) if (!value.finished) this.unknownInput(value);
        if (this.activeInput === input) this.activeInput = null;
      }
      this.notifyWorkflow();
    });
    this.run = run;
  }
  async send(message, operationID) {
    const refusal = this.inputCheck(message);
    if (refusal) return refusal;
    if (this.pending || this.session.isStreaming || this.hostBridge?.pending.size) return {outcome: 'NotSubmitted', refusal: 'worker busy'};
    const input = this.makeInput(message, operationID);
    this.startPrompt(input);
    const result = await input.result;
    // Legacy callbacks already wait for the Go receipt before effects.
    input.committed = true;
    this.inputs.delete(operationID);
    return result;
  }
  enqueueInput({message, inputOperationID: operationID, delivery}) {
    if (!['steer', 'followUp'].includes(delivery) || typeof operationID !== 'string' || this.inputs.has(operationID)) throw new Error('invalid native input admission');
    const refusal = this.inputCheck(message);
    if (refusal) return refusal;
    const input = this.makeInput(message, operationID, delivery);
    // No await separates the streaming decision from native insertion. Idle
    // uses literal prompt so an input cannot be stranded in an idle queue.
    if (this.session.isStreaming) {
      input.object = {role: 'user', content: [{type: 'text', text: message}], timestamp: Date.now()};
      this.session.agent[delivery](input.object);
    } else {
      if (this.pending || this.hostBridge?.pending.size) { this.inputs.delete(operationID); return {outcome: 'NotSubmitted', refusal: 'worker settling'}; }
      this.startPrompt(input);
    }
    this.notifyWorkflow();
    return {outcome: 'Pending'};
  }
  waitInput({inputOperationID}) {
    const input = this.inputs.get(inputOperationID);
    if (!input?.delivery) throw new Error('unknown native input');
    return input.result;
  }
  commitInput({inputOperationID}) {
    const input = this.inputs.get(inputOperationID);
    if (!input?.delivery || !input.witness || !input.finished) throw new Error('unwitnessed native input');
    input.committed = true;
    // Keep active provenance independently from pending admission capacity.
    this.inputs.delete(inputOperationID);
    this.acceptedInputs.set(inputOperationID, input);
    while (this.acceptedInputs.size > 20) this.acceptedInputs.delete(this.acceptedInputs.keys().next().value);
    this.notifyWorkflow();
    return {committed: true};
  }
  verifyHistory({ Witness: witness, Message: message }) {
    const file = this.manager.getSessionFile();
    if (!existsSync(file) || !statSync(file).isFile()) return { Verified: false };
    const disk = this.sdk.SessionManager.open(file);
    try { linearEntries(disk); } catch { return {Verified: false}; }
    if (disk.getSessionId() !== witness.SessionID || disk.getCwd() !== this.manager.getCwd()) return { Verified: false };
    const entry = disk.getEntry(witness.EntryID);
    if (!entry || entry.parentId !== witness.ParentID || entry.type !== 'message' || entry.message.role !== 'user') return { Verified: false };
    const text = entry.message.content;
    if (!Array.isArray(text) || text.length !== 1 || text[0].type !== 'text' || text[0].text !== message) return { Verified: false };
    let parent = entry.parentId;
    while ((parent ?? '') !== witness.PreviousLeafID) {
      const ancestor = disk.getEntry(parent);
      if (!allowedInputAncestor(ancestor, this.witnessedUsers)) return { Verified: false };
      parent = ancestor.parentId;
    }
    return { Verified: true };
  }
  async abort() {
    // Exclusive public queue clearing prevents unconsumed inputs from silently
    // running after cancellation. Labels are not proof of non-submission.
    this.session.clearQueue();
    for (const input of this.inputs.values()) if (!input.finished) {
      input.finished = true; input.resolve({outcome: 'Unknown'}); this.held = true;
    }
    if (this.held) this.notifyWorkflow();
    await this.session.abort();
    return {idle: !this.session.isStreaming, held: this.held};
  }
  holdInputs() {
    this.held = true; this.notifyWorkflow(); this.hostBridge?.cancelAll();
    // Acknowledge the hold without waiting for tools that may need the web
    // admission lock to unwind. EOF/shutdown still joins the public abort.
    void this.abort().catch(() => {});
    return {held: true};
  }
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
    if (this.held || this.pending || this.inputs.size || this.session.isStreaming || this.hostBridge?.pending.size) return { Outcome: 'rejected', Proof: this.inspectToolPolicy() };
    const result = this.toolPolicy.apply(this.session, target, this.generation);
    if (result.Outcome === 'unknown') await this.hold();
    return result;
  }
  async configure(requested) {
    const current = this.settings;
    const result = (Outcome) => ({ Requested: requested, Effective: this.settings, Outcome });
    try { this.checkInstructions(); } catch { return result('unknown'); }
    if (this.held || this.pending || this.inputs.size || this.session.isStreaming || this.hostBridge?.pending.size) return result('rejected');
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
    if (this.held || this.pending || this.inputs.size || this.session.isStreaming || this.hostBridge?.pending.size) throw new Error('worker has unresolved or active work');
    this.unsubscribe?.(); this.listeners.clear();
    this.session.dispose();
  }
}

export async function createSession(sdk, config, options = {}, trustedBinding = '') {
  const literal = Object.hasOwn(config, 'instructions') ? config.instructions : '';
  if (typeof literal !== 'string' || literal.includes('\0') || Buffer.byteLength(literal) > 4096
    || Buffer.from(literal, 'utf8').toString('utf8') !== literal
    || Object.hasOwn(config, 'instructions') && !literal) throw new Error('invalid role instructions');
  const boundPrompt = !!(literal || trustedBinding);
  if (typeof trustedBinding !== 'string' || boundPrompt && (literal && !config.toolPolicy || config.callbackSubject?.Kind !== 'conversation'
    || Object.keys(options.resourceOptions ?? {}).length
    || Object.keys(options.sessionOptions ?? {}).some(key => key !== 'tools' && key !== 'customTools'))) {
    throw new Error('role instruction construction unavailable');
  }
  assertAccountBinding(config.accountBindings, config.settings);
  const installedModels = Object.hasOwn(config, 'modelsPath');
  const modelsPath = installedModels ? config.modelsPath : join(config.agentDir, 'models.json');
  const checkModels = () => installedModels ? checkInstalledModels(modelsPath) : checkModelsFile(modelsPath);
  if (installedModels || !options.modelRuntime) checkModels();
  const createRuntime = () => sdk.ModelRuntime.create({
    authPath: join(config.authDirectory, 'auth.json'), modelsPath,
    modelsStorePath: join(config.agentDir, 'models-store.json'), allowModelNetwork: false,
  });
  const modelRuntime = options.modelRuntime ?? await (options.acquireModelRuntime ? options.acquireModelRuntime(createRuntime) : createRuntime());
  if (!options.modelRuntime) assertStoredAccount(modelRuntime, config.settings.Provider);
  const settings = sdk.SettingsManager.create(config.agentDir, config.agentDir);
  settings.setRetryEnabled(false);
  let worker;
  const toolPolicy = config.toolPolicy ? createToolPolicy(config.toolPolicy, {changed: () => worker?.notifyWorkflow()}) : null;
  if (toolPolicy && (typeof config.generation !== 'string' || !/^[0-9a-f]{64}$/.test(config.generation))) throw new Error('tool policy generation unavailable');
  let expectedAppend, promptContext;
  const observePrompt = api => { api.on('session_start', (_event, context) => { promptContext = context; }); };
  const resourceLoader = new sdk.DefaultResourceLoader({ cwd: config.cwd, agentDir: config.agentDir,
    settingsManager: settings, extensionFactories: [toolPolicy ? toolPolicy.extensionFactory : async (pi) => installLinearGuards(pi), ...(boundPrompt ? [observePrompt] : [])], noExtensions: true, noSkills: true, noPromptTemplates: true, noThemes: true,
    ...(options.resourceOptions ?? {}),
    ...(boundPrompt ? { appendSystemPromptOverride: base => {
      if (!Array.isArray(base) || base.some(value => typeof value !== 'string')) throw new Error('role instruction base unavailable');
      const captured = Object.freeze([...base]);
      expectedAppend = Object.freeze([...captured, ...(trustedBinding ? [trustedBinding] : []), ...(literal ? [literal] : [])]);
      return [...expectedAppend];
    } } : {}) });
  await resourceLoader.reload();
  const model = modelRuntime.getModel(config.settings.Provider, config.settings.Model);
  if (!model) throw new Error('configured model unavailable');
  if (config.fork && !await modelRuntime.checkAuth(model.provider)) throw new Error('fork target auth unavailable');
  let manager;
  if (config.fork) {
    manager = initializeFork(sdk, config, options.syncHistory);
  } else if (config.historyFile) {
    if (!existsSync(config.historyFile) || !statSync(config.historyFile).isFile()) throw new Error('mapped native history is missing');
    manager = sdk.SessionManager.open(config.historyFile);
    if (manager.getSessionId() !== config.nativeId || manager.getCwd() !== config.cwd) throw new Error('native mapping mismatch');
  } else {
    if (!config.emptyProven) throw new Error('empty session lacks no-dispatch/drain proof');
    manager = sdk.SessionManager.create(config.cwd, config.historyDir, { id: config.nativeId });
  }
  const { session } = await sdk.createAgentSession({ cwd: config.cwd, agentDir: config.agentDir,
    modelRuntime, model, thinkingLevel: config.settings.Thinking,
    sessionManager: manager, settingsManager: settings, resourceLoader,
    ...(options.sessionOptions ?? {}), ...(toolPolicy ? { tools: [...config.toolPolicy.RegisteredCeiling] } : {}) });
  await session.bindExtensions({});
  if (config.historyFile || config.fork) {
    try {
      // setModel(persist:false) still appends native model-change history.
      // Keep its auth precondition, then observe the explicit construction
      // instead of retuning or rewriting an already retained conversation.
      if (!await modelRuntime.checkAuth(model.provider)) throw new Error('retained native auth unavailable');
      if (session.model?.provider !== model.provider || session.model?.id !== model.id
        || session.thinkingLevel !== config.settings.Thinking) throw new Error('retained native settings diverged');
    } catch (error) { session.dispose(); throw error; }
  } else {
    await session.setModel(model, { persist: false });
    session.setThinkingLevel(config.settings.Thinking, { persist: false });
  }
  await settings.flush();
  // These public SDK observations bind the actual complete addendum. The closed
  // pinned builder renders it before AGENTS; no substring or second builder is
  // used, and only the observed final literal's digest crosses the worker pipe.
  const boundRunner = boundPrompt ? session.extensionRunner : undefined;
  const verifyInstructions = boundPrompt ? () => {
    if (!boundRunner || session.extensionRunner !== boundRunner || !promptContext) throw new Error('role instructions unavailable');
    // This public factory only constructs lazy closures. Read the options;
    // never dispatch a command or call its session-control methods.
    const construction = structuredClone(boundRunner.createCommandContext().getSystemPromptOptions());
    const observed = [...resourceLoader.getAppendSystemPrompt()];
    if (!(session instanceof sdk.AgentSession) || session.resourceLoader !== resourceLoader
      || Object.hasOwn(session, 'systemPrompt') || !expectedAppend || !isDeepStrictEqual(observed, expectedAppend)
      || !construction || construction.appendSystemPrompt !== observed.join('\n\n')
      || construction.customPrompt !== resourceLoader.getSystemPrompt()
      || !isDeepStrictEqual(construction.contextFiles, resourceLoader.getAgentsFiles().agentsFiles)
      || typeof session.systemPrompt !== 'string' || !session.systemPrompt
      || promptContext.getSystemPrompt() !== session.systemPrompt) throw new Error('role instructions unavailable');
    return literal ? createHash('sha256').update(observed.at(-1), 'utf8').digest('hex') : undefined;
  } : undefined;
  worker = new WorkerSession(sdk, session, manager, { historyDirectory: config.historyDir, syncHistory: options.syncHistory, verifyInstructions, activityClock: options.activityClock });
  worker.runtime = modelRuntime; worker.storageSettings = settings;
  worker.checkAccount = (requested) => {
    assertAccountBinding(config.accountBindings, requested);
    if (!options.modelRuntime) { checkModels(); assertStoredAccount(modelRuntime, requested.Provider); }
  };
  worker.settings = { ...config.settings, Thinking: session.thinkingLevel };
  worker.toolPolicy = toolPolicy; worker.generation = config.generation;
  worker.hostTools = [...(config.hostTools ?? [])];
  if (toolPolicy) {
    const applied = toolPolicy.apply(session, config.toolPolicy, config.generation);
    if (applied.Outcome !== 'applied' || !applied.Proof.Applied) throw new Error('initial tool policy unresolved');
  }
  worker.checkInstructions();
  return worker;
}
