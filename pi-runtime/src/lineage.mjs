import { createHash } from 'node:crypto';
import { existsSync, statSync } from 'node:fs';
import { isDeepStrictEqual } from 'node:util';
import { jsonBytes } from './framing.mjs';

export function lp32(strings) {
  if (!Array.isArray(strings) || strings.length > 64) throw new Error('invalid lineage tuple');
  const chunks = []; const count = Buffer.alloc(4); count.writeUInt32BE(strings.length); chunks.push(count);
  for (const value of strings) {
    if (typeof value !== 'string' || !value.isWellFormed()) throw new Error('invalid lineage UTF-8');
    const bytes = Buffer.from(value, 'utf8');
    if (bytes.length > 65536) throw new Error('lineage string exceeds bound');
    const length = Buffer.alloc(4); length.writeUInt32BE(bytes.length); chunks.push(length, bytes);
  }
  return Buffer.concat(chunks);
}

export function selectionToken(subject, root) {
  let identity; let tuple;
  if (subject.Kind === 'conversation' && subject.Conversation && !subject.Utility) {
    identity = subject.Conversation.Conversation;
    tuple = ['conversation', identity.Session.Workspace, identity.Session.Slug, identity.Session.Incarnation,
      identity.Member, identity.Incarnation, identity.NativeID, root];
  } else if (subject.Kind === 'utility' && subject.Utility && !subject.Conversation) {
    identity = subject.Utility;
    tuple = ['utility', identity.Session.Workspace, identity.Session.Slug, identity.Session.Incarnation,
      identity.MonitorOperationID, identity.Incarnation, identity.NativeID, root];
  } else throw new Error('invalid callback subject');
  if (!root) throw new Error('empty native root');
  return `linear-v1:${createHash('sha256').update(lp32(tuple)).digest('hex')}`;
}

export function linearEntries(manager) {
  const entries = manager.getEntries();
  if (!entries.length) throw new Error('empty native selection');
  const ids = new Set(); let parent = null;
  for (const entry of entries) {
    if (!entry.id || ids.has(entry.id) || (entry.parentId ?? null) !== parent
      || ['branch_summary', 'context_edit'].includes(entry.type)) throw new Error('native selection diverged');
    ids.add(entry.id); parent = entry.id;
  }
  if (manager.getLeafId() !== parent || !isDeepStrictEqual(manager.getBranch(), entries)) throw new Error('native branch changed');
  return entries;
}

function persistedEntries(sdk, manager, expectedID) {
  if (manager.getSessionId() !== expectedID) throw new Error('native origin identity mismatch');
  const entries = linearEntries(manager);
  const file = manager.getSessionFile();
  if (!existsSync(file) || !statSync(file).isFile()) throw new Error('native origin history missing');
  const disk = sdk.SessionManager.open(file);
  if (disk.getSessionId() !== expectedID || disk.getCwd() !== manager.getCwd()) throw new Error('native origin mapping mismatch');
  const persisted = linearEntries(disk);
  // The public SDK may retain optional undefined properties in memory. Its
  // native JSON writer omits those; compare every serialized entry exactly.
  if (JSON.stringify(persisted) !== JSON.stringify(entries)) throw new Error('native origin is not persisted');
  return entries;
}

function toolOrigin(entries, toolCallID, tool) {
  const matches = [];
  for (let index = 0; index < entries.length; index++) {
    const entry = entries[index];
    if (entry.type !== 'message' || entry.message.role !== 'assistant') continue;
    for (const block of entry.message.content ?? []) {
      if (block.type === 'toolCall' && block.id === toolCallID) matches.push({ entry, block, index });
    }
  }
  if (matches.length !== 1 || matches[0].block.name !== tool) throw new Error('native tool origin mismatch');
  const args = matches[0].block.arguments;
  if (!args || typeof args !== 'object' || Array.isArray(args)) throw new Error('invalid native tool arguments');
  return matches[0];
}

export function inspectOrigin(sdk, manager, subject, input, toolCallID, tool, args) {
  const expectedID = subject.Conversation?.Conversation.NativeID ?? subject.Utility?.NativeID;
  if (!input?.witness || input.witness.sessionId !== expectedID) throw new Error('callback input scope mismatch');
  const entries = persistedEntries(sdk, manager, expectedID);
  const inputIndex = entries.findIndex((entry) => entry.id === input.witness.entryId);
  const user = entries[inputIndex];
  if (inputIndex < 0 || user.type !== 'message' || user.message.role !== 'user'
    || (user.parentId ?? '') !== (input.witness.parentId ?? '')
    || !isDeepStrictEqual(user.message.content, [{ type: 'text', text: input.witness.effectiveMessage }])) throw new Error('native input missing');
  const origin = toolOrigin(entries, toolCallID, tool);
  if (origin.index <= inputIndex || !isDeepStrictEqual(origin.block.arguments, args)) throw new Error('native tool origin mismatch');
  return { anchorEntryID: origin.entry.id, selectionToken: selectionToken(subject, entries[0].id) };
}

// Historical retained reads deliberately have no active-input dependency.
export function inspectHistoricalOrigin(sdk, manager, subject, ref) {
  if (subject?.Kind !== 'conversation' || !subject.Conversation || subject.Utility) throw new Error('retained origin required');
  const entries = persistedEntries(sdk, manager, subject.Conversation.Conversation.NativeID);
  const origin = toolOrigin(entries, ref.NativeToolCallID, ref.Tool);
  if (origin.entry.id !== ref.AnchorEntryID || selectionToken(subject, entries[0].id) !== ref.SelectionToken) throw new Error('stale historical origin');
  return { RootEntryID: entries[0].id, Arguments: JSON.parse(jsonBytes(origin.block.arguments)) };
}

export function installLinearGuards(pi) {
  for (const event of ['session_before_tree', 'session_before_fork', 'session_before_switch']) {
    pi.on(event, () => ({ cancel: true }));
  }
}
