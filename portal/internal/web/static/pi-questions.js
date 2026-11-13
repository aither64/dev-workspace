// Server snapshots own question/reply status. Storage contains only drafts and
// the exact pre-network reply, never inferred native acceptance.
export function mountPiQuestions({root, api, storage, namespace, renderCard, onChange = () => {},
  randomUUID = () => globalThis.crypto.randomUUID()}) {
  let revision = -1, destroyed = false;
  const records = new Map(), cards = new Map();
  const key = id => `pi-questions:${namespace}:${id}`;
  const readDraft = id => {
    try { const value = JSON.parse(storage?.getItem(key(id)) || 'null');
      if (value?.schema === 1 && value.items && typeof value.items === 'object') return value;
    } catch (_) {}
    return {schema: 1, items: {}};
  };
  const save = record => {
    if (!storage) throw Error('Browser storage is unavailable. Your answer has not been sent.');
    const data = JSON.stringify(record.draft); storage.setItem(key(record.view.id), data);
    if (storage.getItem(key(record.view.id)) !== data) throw Error('Browser storage did not retain the answer.');
  };
  const emit = () => { if (destroyed) return; for (const card of cards.values()) card.update(); onChange(); };
  const answered = r => r.view.answers;
  const draftFor = (r, item) => { const value = r.draft.items[item.id], answer = answered(r)[item.id];
    return value && typeof value.choice === 'string' && typeof value.note === 'string' ? value :
      {choice: answer?.choiceId || '', note: answer?.text || ''}; };
  const statusText = r => r.error || r.view.latestResponse?.status || r.view.status;
  const check = async r => {
    const id = r.draft.pending?.responseId || r.view.latestResponse?.responseId;
    if (destroyed || !id || r.busy) return;
    r.busy = true; emit();
    try {
      const known = await api.responseStatus(id);
      if (known?.responseId === r.draft.pending?.responseId && known.questionId === r.view.id &&
          known.questionRevision === r.draft.pending.questionRevision &&
          JSON.stringify(known.answers) === JSON.stringify(r.draft.pending.answers)) {
        delete r.draft.pending; save(r);
      }
      r.error = ''; await refresh();
    }
    catch (_) { r.error = 'Could not check the answer status.'; }
    finally { r.busy = false; emit(); }
  };
  const submit = async (r, item) => {
    if (destroyed || r.busy || !r.view.canRespond || r.draft.pending || (answered(r)[item.id] && !r.correcting.has(item.id))) return;
    const draft = draftFor(r, item), option = item.options.find(o => o.id === draft.choice);
    if (!option && !draft.note.trim()) return;
    const payload = {responseId: randomUUID(), questionId: r.view.id, questionRevision: r.view.revision,
      supersedesResponseId: r.view.latestResponse?.responseId || '',
      answers: [{questionId: item.id, choiceId: option?.id || '', text: draft.note}]};
    r.draft.pending = payload;
    try { save(r); } catch (error) { delete r.draft.pending; r.error = error.message; emit(); return; }
    r.busy = true; r.error = ''; emit();
    try {
      // Lost replies are checked observationally. No submit/retry loop exists.
      await api.respond(payload);
      delete r.draft.pending; r.correcting.clear(); save(r);
      await refresh();
    } catch (_) { r.error = 'Your answer is saved in this browser. Check its delivery status before continuing.'; }
    finally { r.busy = false; emit(); }
  };
  let nextMount = 0; const mounts = new WeakMap();
  const cardFor = (r, item, mount) => {
    const id = `${r.view.id}:${item.id}:${mount}`;
    if (!cards.has(id)) cards.set(id, renderCard({id, question: {title: item.title, options: item.options.map(o => o.label)},
      choiceValues: item.options.map(o => o.id),
      draft: () => draftFor(r, item),
      state: () => ({enabled: r.view.canRespond, busy: r.busy,
        locked: Boolean(r.draft.pending) || Boolean(answered(r)[item.id] && !r.correcting.has(item.id)),
        canSubmit: Boolean(draftFor(r, item).choice || draftFor(r, item).note.trim()),
        check: Boolean(r.draft.pending || r.view.latestResponse), statusText: statusText(r)}),
      checkLabel: 'Check answer',
      onDraft: value => { r.draft.items[item.id] = value; try { save(r); r.error = ''; } catch (e) { r.error = e.message; } emit(); },
      onSubmit: () => { void submit(r, item); }, onCheck: () => { void check(r); },
    }));
    return cards.get(id).root;
  };
  const appendCards = (container, selected) => {
    if (!mounts.has(container)) mounts.set(container, ++nextMount);
    const mount = mounts.get(container);
    const nodes = selected.flatMap(r => r.view.questions.map(item => cardFor(r, item, mount)));
    for (const node of [...(container.children || [])]) if (!nodes.includes(node)) node.remove();
    for (const node of nodes) if (node.parentElement !== container) container.append(node);
  };
  function update(snapshot) {
    if (destroyed || !snapshot || !Number.isSafeInteger(snapshot.revision) || snapshot.revision < revision) return;
    revision = snapshot.revision;
    for (const view of snapshot.questions || []) {
      let r = records.get(view.id);
      if (!r) { r = {view, draft: readDraft(view.id), busy: false, error: '', correcting: new Set()}; records.set(view.id, r); }
      r.view = view;
      // A retained pending reply is cleared only by authoritative matching data.
      if (r.draft.pending?.responseId === view.latestResponse?.responseId) {
        delete r.draft.pending; try { save(r); } catch (e) { r.error = e.message; }
      }
    }
    if (root) appendCards(root, [...records.values()]);
    emit();
  }
  async function refresh() { const snapshot = await api.read(); update(snapshot); return snapshot; }
  return {update, refresh,
    inline(entry) {
      const ids = new Set((entry.questions || []).map(q => q.id));
      const group = globalThis.document.createElement('div'); group.className = 'codex-inline-questions';
      appendCards(group, [...records.values()].filter(r => ids.has(r.view.id))); return group;
    },
    renderPinned(container) { appendCards(container, [...records.values()].filter(r => !['closed', 'stale'].includes(r.view.status))); },
    correct(id, itemId) {
      const r = records.get(id);
      if (destroyed || !r || r.busy || r.draft.pending || !r.view.canRespond || !r.view.latestResponse) return false;
      const ids = itemId === undefined ? Object.keys(answered(r)) : [itemId];
      if (!ids.length || ids.some(id => !answered(r)[id])) return false;
      r.correcting = new Set(ids); r.error = ''; emit(); return true;
    },
    destroy() { destroyed = true; cards.clear(); records.clear(); },
  };
}
