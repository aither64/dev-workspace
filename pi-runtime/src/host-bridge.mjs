import { randomUUID, createHash } from 'node:crypto';
import { setImmediate as immediate, setTimeout as delay } from 'node:timers/promises';
import { inspectOrigin } from './lineage.mjs';
import { jsonBytes } from './framing.mjs';
import { createReportingTools, createMonitorTools } from './host-tools/reporting.mjs';
import { createQuestionTools } from './host-tools/questions.mjs';
import { createPlanTools } from './host-tools/plan.mjs';

export class HostBridge {
  constructor({ sdk, manager, subject, emit, getInput, held }) {
    Object.assign(this, { sdk, manager, subject, emit, getInput, held });
    this.pending = new Map();
  }
  async call(tool, args, { nativeToolCallID, signal } = {}) {
    if (typeof nativeToolCallID !== 'string' || !nativeToolCallID || this.pending.size >= 16) throw new Error('native host call unavailable');
    signal?.throwIfAborted();
    const deadline = Date.now() + 30000;
    await immediate();
    if (!this.getInput()?.operationID) throw new Error('missing original callback input');
    while (!this.getInput()?.witness) {
      if (this.held() || Date.now() >= deadline) throw new Error('native input is not accepted');
      await delay(1, undefined, { signal });
    }
    const input = this.getInput();
    const bytes = jsonBytes(args);
    const origin = inspectOrigin(this.sdk, this.manager, this.subject, input, nativeToolCallID, tool, args);
    const id = randomUUID();
    return new Promise((resolve, reject) => {
      const finish = () => { clearTimeout(timer); signal?.removeEventListener('abort', cancel); this.pending.delete(id); };
      const cancel = () => { finish(); this.emit({ event: 'host_cancel', id }); reject(new Error('host call canceled; effects may exist')); };
      const timer = setTimeout(cancel, Math.max(1, deadline - Date.now()));
      signal?.addEventListener('abort', cancel, { once: true });
      this.pending.set(id, { finish, resolve, reject });
      this.emit({ event: 'host_call', id, body: { inputOperationID: input.operationID,
        anchorEntryID: origin.anchorEntryID, nativeToolCallID, selectionToken: origin.selectionToken,
        tool, arguments: args, argumentsSHA256: createHash('sha256').update(bytes).digest('hex') } });
    });
  }
  verify(body) {
    const input = this.getInput();
    if (!input || body.inputOperationID !== input.operationID) throw new Error('stale callback input');
    const origin = inspectOrigin(this.sdk, this.manager, this.subject, input, body.nativeToolCallID, body.tool, body.arguments);
    if (origin.anchorEntryID !== body.anchorEntryID || origin.selectionToken !== body.selectionToken) throw new Error('stale callback lineage');
    return { Verified: true, RootEntryID: this.manager.getEntries()[0].id, AnchorEntryID: origin.anchorEntryID, SelectionToken: origin.selectionToken };
  }
  result(id, result) {
    const pending = this.pending.get(id); if (!pending) return;
    pending.finish();
    if (result?.Outcome === 'result' && result.Body !== undefined) pending.resolve(result.Body);
    else pending.reject(new Error('host result refused or unknown'));
  }
  cancelAll() {
    for (const [id, pending] of this.pending) { pending.finish(); this.emit({ event: 'host_cancel', id }); pending.reject(new Error('host connection closed; effects may exist')); }
  }
  tools(names) {
    const callHost = this.call.bind(this);
    const host = { publishQuestion: (id, args, signal) => callHost('request_user_input_async', args, { nativeToolCallID: id, signal }),
      publishPlan: (id, args, signal) => callHost('publish_plan', args, { nativeToolCallID: id, signal }) };
    const all = [...createReportingTools(callHost), ...createMonitorTools(callHost), ...createQuestionTools({ host, boundCaller: this.subject }), ...createPlanTools({ host, boundCaller: this.subject })];
    const chosen = all.filter((tool) => names.includes(tool.name));
    if (chosen.length !== names.length) throw new Error('host tool factory unavailable');
    return chosen.map((tool) => ({ ...tool, exposure: 'direct' }));
  }
}
