import { TextDecoder } from 'node:util';
import {existsSync, readFileSync, realpathSync} from 'node:fs';
import {join} from 'node:path';
import {createHash} from 'node:crypto';
import { parseRecord } from './framing.mjs';
import { HostBridge } from './host-bridge.mjs';
import { inspectHistoricalOrigin } from './lineage.mjs';
import { isDeepStrictEqual } from 'node:util';
import { loadSDK } from './package.mjs';
import { createSession } from './session.mjs';

const canonicalIdentity = value => Array.isArray(value) ? value.map(canonicalIdentity) :
  value && typeof value === 'object' ? Object.fromEntries(Object.keys(value).sort().map(key => [key, canonicalIdentity(value[key])])) : value;
// The key identifies immutable model definitions and credential namespace,
// never a selected member model or a credential value.
export function modelRuntimeKey(config) {
  const path = Object.hasOwn(config, 'modelsPath') ? config.modelsPath : join(config.agentDir, 'models.json');
  const models = existsSync(path) ? createHash('sha256').update(readFileSync(path)).digest('hex') : '';
  return JSON.stringify([config.piPackage, realpathSync(config.authDirectory), models, config.accountBindings]);
}

const limit = 1024 * 1024;
export async function runWorker(options = {}) {
  const slots = new Map(); const runtimes = new Map();
  const keyOf = identity => JSON.stringify(canonicalIdentity(identity));
  const acquireRuntime = (slot, config) => async create => {
    const key = modelRuntimeKey(config);
    let entry = runtimes.get(key);
    if (!entry) { entry = {value: create(), refs: 0}; runtimes.set(key, entry); }
    entry.refs++; slot.runtimeKey = key;
    return entry.value;
  };
  const releaseRuntime = async slot => {
    const key = slot.runtimeKey, entry = runtimes.get(key); if (!entry) return;
    slot.runtimeKey = null;
    if (--entry.refs === 0) { runtimes.delete(key); await (await entry.value).settleCredentialWrites(); }
  };
  const pending = new Set();
  const emit = (record) => {
    const bytes = JSON.stringify(record);
    if (Buffer.byteLength(bytes) > limit) throw new Error('worker response exceeds bound');
    process.stdout.write(`${bytes}\n`);
  };

  async function handle(record) {
    if (record.schema !== 1 || typeof record.id !== 'string' || !record.identity || !record.body) throw new Error('unsupported worker envelope');
    if (record.op === 'version' && slots.size === 0) return { workerProtocol: 2, hostCallbacks: 1, linearLineage: 1 };
    const key = keyOf(record.identity);
    if (record.op === 'init') {
      if (slots.has(key)) throw new Error('worker already initialized');
      const slot = {identity: record.identity}; slots.set(key, slot);
      let worker, bridge, callbackSubject;

      const config = record.body;
      callbackSubject = config.callbackSubject;
      const utility = record.identity.OwnerKind === 'utility' && record.identity.Utility;
      const retained = !record.identity.OwnerKind && !record.identity.Utility && record.identity.Conversation;
      if ((!utility && !retained) || (utility && record.identity.Conversation)
        || config.nativeId !== (utility ? utility.NativeID : retained.NativeID)) throw new Error('native worker scope mismatch');
      const sdk = options.sdk ?? await loadSDK(config.piPackage);
      const extra = options.sessionOptions ? await options.sessionOptions(sdk, config) : {};
      const identity = record.identity;
      const names = config.hostTools ?? [];
      if (!Array.isArray(names) || names.length > 6 || names.some((name, i) => typeof name !== 'string' || (i > 0 && names[i - 1] >= name))) throw new Error('invalid host tool list');
      if (names.length) {
        bridge = new HostBridge({ sdk, subject: config.callbackSubject, emit: (frame) => emit({ schema: 1, identity, ...frame }),
          getInput: (operationID) => operationID ? worker?.inputs.get(operationID) ?? worker?.acceptedInputs.get(operationID) ?? (worker?.activeInput?.operationID === operationID ? worker.activeInput : null) : worker?.activeInput, held: () => !worker || worker.held });
        const definitions = bridge.tools(names);
        extra.sessionOptions = { ...(extra.sessionOptions ?? {}), customTools: [...(extra.sessionOptions?.customTools ?? []), ...definitions],
          ...(extra.sessionOptions?.tools ? { tools: [...extra.sessionOptions.tools, ...names] } : {}) };
      }
      extra.acquireModelRuntime = acquireRuntime(slot, config);
      worker = await createSession(sdk, config, extra); slot.worker = worker; slot.bridge = bridge; slot.callbackSubject = callbackSubject;
      if (bridge) { bridge.manager = worker.manager; worker.hostBridge = bridge; }
      worker.listeners.add((event) => {
        emit({ schema: 1, event: 'invalidate', identity,
          nativeEvent: { type: event.type, ...(event.message?.role === 'assistant' ? { provider: event.message.provider, model: event.message.model } : {}) } });
      });
      return worker.snapshot();
    }
    const slot = slots.get(key);
    if (!slot?.worker || !isDeepStrictEqual(record.identity, slot.identity)) throw new Error('stale worker identity');
    const {worker, bridge, callbackSubject} = slot;
    switch (record.op) {
      case 'snapshot': return worker.snapshot(record.body);
      case 'fork_quiescent_v1': worker.assertForkQuiescent(); return {idle: true};
      case 'fork_snapshot_v1': return worker.forkSnapshot();
      case 'fork_verify_v1': return worker.verifyFork(record.body);
      case 'activity_v1': return worker.readActivity();
      case 'workflow_observe': return worker.workflowObservation();
      case 'verify_history': return worker.verifyHistory(record.body);
      case 'input_hold_v1': return worker.holdInputs();
      case 'input_capabilities_v1': return {version: 1};
      case 'input_enqueue_v1': return worker.enqueueInput(record.body);
      case 'input_wait_v1': return worker.waitInput(record.body);
      case 'input_commit_v1': return worker.commitInput(record.body);
      case 'send': return worker.send(record.body.message, record.body.inputOperationID);
      case 'verify_origin': return bridge.verify(record.body);
      case 'inspect_origin': return inspectHistoricalOrigin(worker.sdk, worker.manager, callbackSubject, record.body);
      case 'tool_policy_inspect': return worker.inspectToolPolicy();
      case 'tool_policy_apply': return worker.applyToolPolicy(record.body);
      case 'abort': return worker.abort();
      case 'hold': return worker.hold();
      case 'configure': return worker.configure(record.body);
      case 'unload': await worker.dispose(); bridge?.cancelAll(); slots.delete(key); await releaseRuntime(slot); return { detached: true };
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
          const slot = slots.get(keyOf(request.identity));
          if (request.schema !== 1 || !slot?.worker || !isDeepStrictEqual(request.identity, slot.identity)) throw new Error('stale callback result');
          if (request.op === 'host_result') slot.bridge?.result(request.id, request.body);
          else slot.bridge?.result(request.id, { Outcome: 'unknown' });
          continue;
        }
        const operation = handle(request).then((body) => {
          emit({ schema: 1, id: request.id, identity: request.identity, body });

        }).catch((error) => { options.onError?.(error); emit({ schema: 1, id: request.id, identity: request.identity, error: 'worker operation unresolved' }); })
          .finally(() => pending.delete(operation));
        pending.add(operation);
      }
      if (buffered.length > limit) throw new Error('worker frame exceeds bound');
    }
    if (buffered.length) throw new Error('unfinished worker frame');
  } finally {
    const active = [...slots.values()];
    for (const slot of active) slot.bridge?.cancelAll();
    await Promise.allSettled(active.map(slot => slot.worker?.abort()));
    await Promise.allSettled(pending);
    for (const slot of active) {
      slot.worker?.session.dispose();
      await releaseRuntime(slot);
    }
  }
}
