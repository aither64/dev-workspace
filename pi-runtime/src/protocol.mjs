import { createInterface } from 'node:readline';
import { isDeepStrictEqual } from 'node:util';
import { loadSDK } from './package.mjs';
import { createSession } from './session.mjs';

const limit = 1024 * 1024;
export async function runWorker(options = {}) {
  let worker; let identity; let initializing = false;
  const pending = new Set();
  const emit = (record) => {
    const bytes = JSON.stringify(record);
    if (Buffer.byteLength(bytes) > limit) throw new Error('worker response exceeds bound');
    process.stdout.write(`${bytes}\n`);
  };
  const input = createInterface({ input: process.stdin });
  async function handle(record) {
    if (record.schema !== 1 || typeof record.id !== 'string' || !record.identity || !record.body) throw new Error('unsupported worker envelope');
    if (record.op === 'version' && !worker && !initializing) return { workerProtocol: 1 };
    if (record.op === 'init') {
      if (worker || initializing) throw new Error('worker already initialized');
      initializing = true;
      const config = record.body;
      if (config.nativeId !== record.identity.Conversation.NativeID) throw new Error('native worker scope mismatch');
      const sdk = options.sdk ?? await loadSDK(config.piPackage);
      const extra = options.sessionOptions ? await options.sessionOptions(sdk, config) : {};
      worker = await createSession(sdk, config, extra);
      identity = record.identity;
      worker.listeners.add((event) => {
        emit({ schema: 1, event: 'invalidate', identity,
          nativeEvent: { type: event.type, ...(event.message?.role === 'assistant' ? { provider: event.message.provider, model: event.message.model } : {}) } });
      });
      return worker.snapshot();
    }
    if (!worker || !isDeepStrictEqual(record.identity, identity)) throw new Error('stale worker identity');
    switch (record.op) {
      case 'snapshot': return worker.snapshot();
      case 'send': return worker.send(record.body.message);
      case 'abort': return worker.abort();
      case 'hold': return worker.hold();
      case 'configure': return worker.configure(record.body);
      case 'unload': await worker.dispose(); return { exited: true };
      default: throw new Error('unsupported worker operation');
    }
  }
  input.on('line', (line) => {
    if (Buffer.byteLength(line) > limit) { input.close(); return; }
    let request;
    try { request = JSON.parse(line); } catch { input.close(); return; }
    const operation = handle(request).then((body) => {
      emit({ schema: 1, id: request.id, identity: request.identity, body });
      if (request.op === 'unload') input.close();
    }).catch(() => emit({ schema: 1, id: request.id, identity: identity ?? request.identity, error: 'worker operation unresolved' }))
      .finally(() => pending.delete(operation));
    pending.add(operation);
  });
  await new Promise((resolve) => input.once('close', resolve));
  if (worker) await worker.abort();
  await Promise.allSettled(pending);
  if (worker) { worker.session.dispose(); await worker.runtime.settleCredentialWrites(); }
}
