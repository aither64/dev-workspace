import { TextDecoder } from 'node:util';
import { parseRecord } from './framing.mjs';
import { HostBridge } from './host-bridge.mjs';
import { inspectHistoricalOrigin } from './lineage.mjs';
import { isDeepStrictEqual } from 'node:util';
import { loadSDK } from './package.mjs';
import { createSession } from './session.mjs';

const limit = 1024 * 1024;
export async function runWorker(options = {}) {
  let worker; let identity; let bridge; let callbackSubject; let initializing = false; let unloading = false;
  const pending = new Set();
  const emit = (record) => {
    const bytes = JSON.stringify(record);
    if (Buffer.byteLength(bytes) > limit) throw new Error('worker response exceeds bound');
    process.stdout.write(`${bytes}\n`);
  };

  async function handle(record) {
    if (record.schema !== 1 || typeof record.id !== 'string' || !record.identity || !record.body) throw new Error('unsupported worker envelope');
    if (record.op === 'version' && !worker && !initializing) return { workerProtocol: 1, hostCallbacks: 1, linearLineage: 1 };
    if (record.op === 'init') {
      if (worker || initializing) throw new Error('worker already initialized');
      initializing = true;
      const config = record.body;
      callbackSubject = config.callbackSubject;
      const utility = record.identity.OwnerKind === 'utility' && record.identity.Utility;
      const retained = !record.identity.OwnerKind && !record.identity.Utility && record.identity.Conversation;
      if ((!utility && !retained) || (utility && record.identity.Conversation)
        || config.nativeId !== (utility ? utility.NativeID : retained.NativeID)) throw new Error('native worker scope mismatch');
      const sdk = options.sdk ?? await loadSDK(config.piPackage);
      const extra = options.sessionOptions ? await options.sessionOptions(sdk, config) : {};
      identity = record.identity;
      const names = config.hostTools ?? [];
      if (!Array.isArray(names) || names.length > 6 || names.some((name, i) => typeof name !== 'string' || (i > 0 && names[i - 1] >= name))) throw new Error('invalid host tool list');
      if (names.length) {
        bridge = new HostBridge({ sdk, subject: config.callbackSubject, emit: (frame) => emit({ schema: 1, identity, ...frame }),
          getInput: () => worker?.activeInput, held: () => !worker || worker.held });
        const definitions = bridge.tools(names);
        extra.sessionOptions = { ...(extra.sessionOptions ?? {}), customTools: [...(extra.sessionOptions?.customTools ?? []), ...definitions],
          ...(extra.sessionOptions?.tools ? { tools: [...extra.sessionOptions.tools, ...names] } : {}) };
      }
      worker = await createSession(sdk, config, extra);
      if (bridge) { bridge.manager = worker.manager; worker.hostBridge = bridge; }
      identity = record.identity;
      worker.listeners.add((event) => {
        emit({ schema: 1, event: 'invalidate', identity,
          nativeEvent: { type: event.type, ...(event.message?.role === 'assistant' ? { provider: event.message.provider, model: event.message.model } : {}) } });
      });
      return worker.snapshot();
    }
    if (!worker || !isDeepStrictEqual(record.identity, identity)) throw new Error('stale worker identity');
    switch (record.op) {
      case 'snapshot': return worker.snapshot(record.body);
      case 'verify_history': return worker.verifyHistory(record.body);
      case 'send': return worker.send(record.body.message, record.body.inputOperationID);
      case 'verify_origin': return bridge.verify(record.body);
      case 'inspect_origin': return inspectHistoricalOrigin(worker.sdk, worker.manager, callbackSubject, record.body);
      case 'tool_policy_inspect': return worker.inspectToolPolicy();
      case 'tool_policy_apply': return worker.applyToolPolicy(record.body);
      case 'abort': return worker.abort();
      case 'hold': return worker.hold();
      case 'configure': return worker.configure(record.body);
      case 'unload': await worker.dispose(); return { exited: true };
      default: throw new Error('unsupported worker operation');
    }
  }
  let buffered = Buffer.alloc(0);
  const decoder = new TextDecoder('utf-8', { fatal: true });
  try {
    for await (const chunk of process.stdin) {
      buffered = Buffer.concat([buffered, chunk]);
      while (buffered.includes(10)) {
        const end = buffered.indexOf(10); const line = buffered.subarray(0, end); buffered = buffered.subarray(end + 1);
        const request = parseRecord(decoder.decode(line), ['schema', 'id', 'identity', 'op', 'body']);
        if (request.op === 'host_result' || request.op === 'host_cancel') {
          if (request.schema !== 1 || !isDeepStrictEqual(request.identity, identity)) throw new Error('stale callback result');
          if (request.op === 'host_result') bridge?.result(request.id, request.body);
          else bridge?.result(request.id, { Outcome: 'unknown' });
          continue;
        }
        const operation = handle(request).then((body) => {
          emit({ schema: 1, id: request.id, identity: request.identity, body });
          if (request.op === 'unload') { unloading = true; process.stdin.destroy(); }
        }).catch((error) => { options.onError?.(error); emit({ schema: 1, id: request.id, identity: identity ?? request.identity, error: 'worker operation unresolved' }); })
          .finally(() => pending.delete(operation));
        pending.add(operation);
      }
      if (buffered.length > limit) throw new Error('worker frame exceeds bound');
    }
    if (buffered.length) throw new Error('unfinished worker frame');
  } catch (error) { if (!unloading) throw error; } finally {
    bridge?.cancelAll();
    if (worker) await worker.abort();
    await Promise.allSettled(pending);
    if (worker) { worker.session.dispose(); await worker.runtime.settleCredentialWrites(); }
  }
}
