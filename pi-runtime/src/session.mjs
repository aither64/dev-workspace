import { existsSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { setImmediate as immediate } from 'node:timers/promises';
import { isDeepStrictEqual } from 'node:util';

export class WorkerSession {
  constructor(sdk, session, manager) {
    this.sdk = sdk; this.session = session; this.manager = manager;
    this.pending = null; this.held = false; this.listeners = new Set();
    session.subscribe((event) => {
      if (event.type === 'message_end' && event.message.role === 'user' && this.pending) {
        this.pending.observed.push(event.message);
        // Pi emits before append. Leave the event callback before reading.
        void this.observePersisted(this.pending);
      }
      for (const listener of this.listeners) listener(event);
    });
  }
  snapshot() {
    return { nativeId: this.manager.getSessionId(), historyFile: this.manager.getSessionFile(),
      persisted: existsSync(this.manager.getSessionFile()), leafId: this.manager.getLeafId(),
      entries: this.manager.getEntries(), busy: this.session.isStreaming,
      model: this.session.model?.id, provider: this.session.model?.provider,
      thinking: this.session.thinkingLevel, held: this.held };
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
    if (!existsSync(file)) return;
    let persisted;
    try { persisted = this.sdk.SessionManager.open(file); } catch { return; }
    if (persisted.getSessionId() !== this.manager.getSessionId()) return;
    const disk = persisted.getEntry(entry.id);
    if (!disk || JSON.stringify(disk) !== JSON.stringify(entry)) return;
    pending.finished = true;
    pending.resolve({ outcome: 'Accepted', witness: { sessionId: persisted.getSessionId(),
      entryId: entry.id, parentId: entry.parentId, previousLeafId: pending.leaf,
      effectiveMessage: pending.message } });
  }
  async send(message) {
    if (typeof message !== 'string' || !message || Buffer.byteLength(message) > 65536) {
      return { outcome: 'NotSubmitted', refusal: 'invalid text input' };
    }
    if (this.held) return { outcome: 'Unknown' };
    if (this.pending || this.session.isStreaming) return { outcome: 'NotSubmitted', refusal: 'worker busy' };
    const pending = { before: new Set(this.manager.getEntries().map((entry) => entry.id)),
      leaf: this.manager.getLeafId(), observed: [], message, finished: false };
    const result = new Promise((resolve) => { pending.resolve = resolve; });
    this.pending = pending;
    this.run = this.session.prompt(message).catch(() => {}).finally(async () => {
      await this.observePersisted(pending);
      if (!pending.finished) { this.held = true; pending.resolve({ outcome: 'Unknown' }); }
      if (this.pending === pending) this.pending = null;
    });
    return result;
  }
  async abort() { await this.session.abort(); return { idle: !this.session.isStreaming, held: this.held }; }
  async hold() {
    this.held = true;
    if (this.pending && !this.pending.finished) {
      this.pending.finished = true;
      this.pending.resolve({ outcome: 'Unknown' });
    }
    return this.abort();
  }
  async configure(requested) {
    const current = this.settings;
    const result = (Outcome) => ({ Requested: requested, Effective: this.settings, Outcome });
    if (this.held || this.pending || this.session.isStreaming) return result('rejected');
    if (requested.Revision === current.Revision) return result(isDeepStrictEqual(requested, current) ? 'applied' : 'rejected');
    if (requested.Revision !== current.Revision + 1 || requested.AccountRef !== current.AccountRef) return result('rejected');
    const model = this.runtime.getModel(requested.Provider, requested.Model);
    if (!model) return result('rejected');
    try {
      await this.session.setModel(model, { persist: true });
      this.session.setThinkingLevel(requested.Thinking, { persist: true });
      await this.storageSettings.flush();
      this.settings = { ...requested, Thinking: this.session.thinkingLevel };
      return result('applied');
    } catch {
      await this.hold();
      return result('unknown');
    }
  }
  async dispose() {
    if (this.held || this.pending || this.session.isStreaming) throw new Error('worker has unresolved or active work');
    this.session.dispose();
  }
}

export async function createSession(sdk, config, options = {}) {
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
    authPath: join(config.authDirectory, 'auth.json'), modelsPath: join(config.agentDir, 'models.json'), allowModelNetwork: false,
  });
  const settings = sdk.SettingsManager.create(config.agentDir, config.agentDir);
  settings.setRetryEnabled(false);
  const resourceLoader = new sdk.DefaultResourceLoader({ cwd: config.cwd, agentDir: config.agentDir,
    settingsManager: settings, noExtensions: true, noSkills: true, noPromptTemplates: true, noThemes: true,
    ...(options.resourceOptions ?? {}) });
  await resourceLoader.reload();
  const model = modelRuntime.getModel(config.settings.Provider, config.settings.Model);
  if (!model) throw new Error('configured model unavailable');
  const { session } = await sdk.createAgentSession({ cwd: config.cwd, agentDir: config.agentDir,
    modelRuntime, model, thinkingLevel: config.settings.Thinking,
    sessionManager: manager, settingsManager: settings, resourceLoader,
    ...(options.sessionOptions ?? {}) });
  const worker = new WorkerSession(sdk, session, manager);
  worker.runtime = modelRuntime; worker.storageSettings = settings;
  worker.settings = { ...config.settings, Thinking: session.thinkingLevel };
  return worker;
}
