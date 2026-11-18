import { createHash } from 'node:crypto';
import { readFileSync, readdirSync, lstatSync } from 'node:fs';
import { dirname, isAbsolute, join } from 'node:path';
import { TextDecoder } from 'node:util';
import { linearEntries } from './lineage.mjs';
import { parseRecord } from './framing.mjs';
import { syncHistoryPrefix } from './persistence.mjs';

const digest = entries => createHash('sha256').update(JSON.stringify(entries)).digest('hex');
function history(sdk, file, id, cwd) {
  const stat = lstatSync(file);
  if (!stat.isFile() || stat.uid !== process.getuid()) throw new Error('fork history unavailable');
  const text = new TextDecoder('utf-8', { fatal: true }).decode(readFileSync(file));
  if (!text.endsWith('\n')) throw new Error('partial fork history');
  const records = text.slice(0, -1).split('\n').map(line => {
    const value = JSON.parse(line);
    return parseRecord(line, Object.keys(value), Buffer.byteLength(line));
  });
  const [header, ...entries] = records;
  if (header?.type !== 'session' || header.id !== id || header.cwd !== cwd || header.version !== 3
    || entries.some(entry => entry.type === 'session')) throw new Error('fork header mismatch');
  const manager = sdk.SessionManager.open(file);
  if (manager.getSessionId() !== id || manager.getCwd() !== cwd
    || JSON.stringify(manager.getHeader()) !== JSON.stringify(header)
    || JSON.stringify(linearEntries(manager)) !== JSON.stringify(entries)) throw new Error('fork history is not complete linear history');
  return { manager, header, entries, stat };
}

export function captureFork(sdk, manager) {
  const sourceFile = manager.getSessionFile(), sourceNativeId = manager.getSessionId(), sourceCwd = manager.getCwd();
  const disk = history(sdk, sourceFile, sourceNativeId, sourceCwd);
  if (JSON.stringify(disk.entries) !== JSON.stringify(linearEntries(manager))) throw new Error('unpersisted fork source');
  return { sourceFile, sourceNativeId, sourceCwd, entriesDigest: digest(disk.entries),
    entryCount: disk.entries.length, leafId: disk.entries.at(-1).id };
}

export function verifyFork(sdk, manager, origin, sync = syncHistoryPrefix, exact = false) {
  const file = manager.getSessionFile();
  const disk = history(sdk, file, manager.getSessionId(), manager.getCwd());
  const prefix = disk.entries.slice(0, origin.entryCount);
  if (disk.header.parentSession !== origin.sourceFile || prefix.length !== origin.entryCount
    || prefix.at(-1)?.id !== origin.leafId || digest(prefix) !== origin.entriesDigest
    || exact && disk.entries.length !== origin.entryCount) throw new Error('fork prefix mismatch');
  sync(file, disk.stat);
  return { ...origin, destinationFile: file, destinationNativeId: manager.getSessionId(), synced: true };
}

// Native filenames contain timestamps. The reserved ID and complete prefix are
// the retry key; a partial or ambiguous file is held without replacement.
export function initializeFork(sdk, config, sync = syncHistoryPrefix) {
  const origin = config.fork;
  if (!origin || config.emptyProven || config.historyFile || !isAbsolute(origin.sourceFile)
    || !Number.isSafeInteger(origin.entryCount) || origin.entryCount < 1
    || !/^[0-9a-f]{64}$/.test(origin.entriesDigest) || !origin.leafId
    || config.nativeId === origin.sourceNativeId) throw new Error('invalid fork initialization');
  const files = readdirSync(config.historyDir);
  if (files.length) {
    if (files.length !== 1 || !files[0].endsWith(`_${config.nativeId}.jsonl`)) throw new Error('ambiguous fork history');
    const { manager } = history(sdk, join(config.historyDir, files[0]), config.nativeId, config.cwd);
    if (manager.getSessionId() !== config.nativeId || manager.getCwd() !== config.cwd) throw new Error('fork recovery identity mismatch');
    verifyFork(sdk, manager, origin, sync, true);
    return manager;
  }
  const source = history(sdk, origin.sourceFile, origin.sourceNativeId, origin.sourceCwd);
  if (JSON.stringify(captureFork(sdk, source.manager)) !== JSON.stringify(origin)) throw new Error('fork source drift');
  const manager = sdk.SessionManager.forkFrom(origin.sourceFile, config.cwd, config.historyDir, { id: config.nativeId });
  if (dirname(manager.getSessionFile()) !== config.historyDir) throw new Error('fork history escaped destination');
  verifyFork(sdk, manager, origin, sync, true);
  // Detect a concurrent writer before exposing this copy. Source admission and
  // the supervisor's native exclusion span this entire synchronous operation.
  if (JSON.stringify(captureFork(sdk, source.manager)) !== JSON.stringify(origin)) throw new Error('fork source drift');
  return manager;
}
