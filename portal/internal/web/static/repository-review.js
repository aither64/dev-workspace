const editorAsset = "/static/review-editor.js";
let editorModule;
const node = (tag, className, text) => {
  const element = document.createElement(tag);
  if (className) element.className = className;
  if (text !== undefined) element.textContent = text;
  return element;
};
const button = (label, action, className = "quiet") => {
  const result = node("button", className, label);
  result.type = "button";
  result.addEventListener("click", action);
  return result;
};
const short = (sha) => String(sha || "").slice(0, 10);
const request = async (url, options = {}) => {
  const response = await fetch(url, {credentials: "same-origin", ...options});
  const payload = await response.json();
  if (!response.ok) throw new Error(payload.error || `Review request failed (${response.status})`);
  return payload;
};
const readMode = () => {
  try { return localStorage.getItem("repository-review-mode") === "unified" ? "unified" : "split"; }
  catch (_) { return "split"; }
};

export function mount({slug, nonce, element}) {
  if (!document.querySelector('link[data-repository-review-styles]')) {
    const sheet = node("link"); sheet.rel = "stylesheet"; sheet.href = "/static/repository-review.css";
    sheet.dataset.repositoryReviewStyles = "true"; document.head.append(sheet);
  }
  const overview = node("div", "repository-overview");
  overview.append(...element.childNodes);
  const review = node("section", "repository-comparison"); review.hidden = true;
  element.append(overview, review);
  const states = new Map();
  let active = null;
  let mode = readMode();
  let sequence = 0;
  let checking = false;
  const url = (operation, id, values = {}) => {
    const query = new URLSearchParams({repository: id, ...values});
    return `/api/sessions/${encodeURIComponent(slug)}/repository-${operation}?${query}`;
  };
  const destroyEditors = () => {
    if (!active) return;
    active.observer?.disconnect();
    active.abort.abort();
    for (const record of active.sections.values()) { ++record.generation; record.editor?.destroy(); }
  };
  const showFailure = (target, error) => { target.replaceChildren(node("p", "notice warning", error.message)); };
  const markChanged = (state, head) => {
    state.latestHead = head;
    const changed = Boolean(state.pair && head && state.pair.head !== head);
    state.card.querySelector(".repository-head-change").hidden = !changed;
    if (active?.state === state && active.branch) active.changed.hidden = !head || active.pair.head === head;
  };
  const loadHistory = async (state, page = 0, refresh = false) => {
    if (state.loading) return;
    state.loading = true;
    const target = state.card.querySelector("[data-repository-commits]");
    const branch = state.card.querySelector("[data-review-branch]");
    const refreshButton = state.card.querySelector("[data-review-refresh]");
    refreshButton.disabled = true;
    if (!state.snapshot) target.replaceChildren(node("p", "muted", "Loading local commits…"));
    try {
      const params = {page: String(page)};
      if (state.snapshot && !refresh) params.snapshot = state.snapshot;
      const payload = await request(url("history", state.id, params));
      state.snapshot = payload.snapshot; state.pair = payload.pair; state.page = page;
      branch.disabled = false;
      const list = node("ol", "repository-commit-list");
      for (const commit of payload.history.commits) {
        const item = node("li", "repository-commit");
        const row = node("div", "repository-commit-row");
        const subject = button(commit.subject || "(No commit subject)", () => openComparison(state, commit), "repository-commit-subject quiet");
        const identity = node("code", "repository-commit-sha", short(commit.sha));
        subject.prepend(identity, document.createTextNode(" "));
        row.append(subject);
        if (commit.url) {
          const link = node("a", "repository-commit-link", "GitHub ↗");
          link.href = commit.url; link.target = "_blank"; link.rel = "noreferrer";
          link.setAttribute("aria-label", `View ${short(commit.sha)} on GitHub`); row.append(link);
        }
        item.append(row);
        if (commit.body) {
          const details = node("details", "repository-commit-message");
          details.append(node("summary", "", "Commit message"), node("pre", "", commit.body)); item.append(details);
        }
        list.append(item);
      }
      target.replaceChildren(node("p", "muted repository-history-base", `${short(payload.pair.base)} → ${short(payload.pair.head)} · ${payload.pair.baseLabel}`));
      if (payload.pair.warning) target.append(node("p", "notice warning", payload.pair.warning));
      target.append(list);
      if (!payload.history.commits.length) target.append(node("p", "muted", "No commits in this comparison."));
      const pagination = node("nav", "repository-pagination"); pagination.setAttribute("aria-label", "Commit history pages");
      const previous = button("Previous", () => loadHistory(state, page - 1)); previous.disabled = page === 0;
      const next = button("Next", () => loadHistory(state, page + 1)); next.disabled = !payload.history.hasMore;
      pagination.append(previous, node("span", "muted", `Page ${page + 1} · up to 50 commits`), next);
      target.append(pagination);
      markChanged(state, refresh ? payload.pair.head : state.latestHead || payload.pair.head);
      return true;
    } catch (error) { showFailure(target, error); return false; }
    finally { state.loading = false; refreshButton.disabled = false; }
  };
  const metadata = (file, content) => {
    const lines = [];
    if (file.oldPath) lines.push(`Renamed from ${file.oldPath}`);
    if (file.oldMode !== file.newMode) lines.push(`Mode: ${file.oldMode} → ${file.newMode}`);
    for (const [label, blob] of [["Before", content.before], ["After", content.after]]) {
      if (blob.kind === "absent") lines.push(`${label}: file absent`);
      if (blob.kind === "symlink") lines.push(`${label}: symbolic link target`);
      if (blob.kind === "submodule") lines.push(`${label}: submodule commit ${blob.text.trim()}`);
      if (blob.binary) lines.push(`${label}: binary or non-UTF-8 data (${blob.bytes.toLocaleString()} bytes)`);
      if (blob.limited) lines.push(`${label}: preview omitted (limit: 512 KiB or 12,000 lines; ${blob.bytes.toLocaleString()} bytes)`);
      if (blob.missingNewline) lines.push(`${label}: no newline at end of file`);
    }
    return lines;
  };
  const trimEditors = selected => {
    const viewport = selected.scroll.getBoundingClientRect();
    const mounted = [...selected.sections.values()].filter(record => record.editor || record.content);
    if (mounted.length <= 8) return;
    mounted.sort((a, b) => a.used - b.used);
    for (const record of mounted) {
      if ([...selected.sections.values()].filter(item => item.editor || item.content).length <= 8) break;
      const bounds = record.section.getBoundingClientRect();
      if (bounds.bottom >= viewport.top - 700 && bounds.top <= viewport.bottom + 700) continue;
      // Preserve the measured space so disposing a distant editor never jumps
      // the current scroll position. Blob text is released with the editor.
      record.host.style.minHeight = `${Math.max(160, record.host.getBoundingClientRect().height)}px`;
      ++record.generation; record.editor?.destroy(); record.editor = null; record.content = null;
      record.host.replaceChildren(node("p", "muted", "Scroll here to load this comparison."));
    }
  };
  const renderFile = async (selected, record) => {
    if (!record.content || active !== selected) return;
    const content = record.content;
    const generation = ++record.generation;
    record.editor?.destroy(); record.editor = null;
    record.host.replaceChildren();
    if (content.before.binary || content.after.binary || content.before.limited || content.after.limited) {
      record.host.style.minHeight = "0px";
      record.host.append(node("p", "muted", "Text comparison is unavailable for this file. Review the metadata above or inspect it locally.")); return;
    }
    record.host.append(node("p", "muted", "Loading comparison…"));
    try {
      editorModule ||= import(editorAsset);
      const {createReviewEditor} = await editorModule;
      if (active !== selected || generation !== record.generation) return;
      record.host.replaceChildren(); record.host.style.minHeight = "0px";
      record.editor = createReviewEditor({parent: record.host, before: content.before.text, after: content.after.text, mode, nonce});
      record.used = performance.now();
      requestAnimationFrame(() => {
        if (active !== selected) return;
        trimEditors(selected);
        const destination = selected.sections.get(selected.pendingNavigation);
        if (destination) {
          selected.scroll.scrollTop += destination.section.getBoundingClientRect().top - selected.scroll.getBoundingClientRect().top;
          requestAnimationFrame(() => {
            if (active !== selected || selected.pendingNavigation !== destination.file.id) return;
            selected.scroll.scrollTop += destination.section.getBoundingClientRect().top - selected.scroll.getBoundingClientRect().top;
          });
        }
      });
    } catch (error) { editorModule = null; if (active === selected && generation === record.generation) showFailure(record.host, error); }
  };
  const loadFile = async (selected, record) => {
    if (active !== selected || record.loading) return;
    record.used = performance.now();
    if (record.content) { if (!record.editor) await renderFile(selected, record); return; }
    record.loading = true;
    record.host.replaceChildren(node("p", "muted", "Loading file…"));
    try {
      const content = await request(url("file", selected.state.id, {snapshot: selected.snapshot, file: record.file.id}), {signal: selected.abort.signal});
      if (active !== selected) return;
      record.content = content; record.metadata.replaceChildren();
      for (const line of metadata(record.file, content)) record.metadata.append(node("p", "", line));
      await renderFile(selected, record);
    } catch (error) { if (active === selected && error.name !== "AbortError") showFailure(record.host, error); }
    finally { record.loading = false; }
  };
  const selectFile = file => {
    if (!active) return;
    const selected = active;
    const record = selected.sections.get(file.id);
    selected.file = file; selected.pendingNavigation = file.id;
    for (const item of selected.fileList.querySelectorAll("button")) item.setAttribute("aria-current", item.dataset.fileId === file.id ? "true" : "false");
    const bounds = record.section.getBoundingClientRect();
    const viewport = selected.scroll.getBoundingClientRect();
    selected.scroll.scrollTop += bounds.top - viewport.top;
    loadFile(selected, record);
  };
  const closeReview = () => {
    ++sequence; destroyEditors(); active = null; review.hidden = true; overview.hidden = false;
    element.classList.remove("repository-review-open");
  };
  const openComparison = async (state, commit = null) => {
    if (!state.snapshot) return;
    const ticket = ++sequence;
    const previousFile = active?.file?.path;
    overview.hidden = true; review.hidden = false; element.classList.add("repository-review-open");
    destroyEditors(); active = null;
    review.replaceChildren(button("← Repositories", closeReview), node("p", "muted", "Loading comparison…"));
    try {
      const payload = await request(url("comparison", state.id), {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({snapshot: state.snapshot, ...(commit ? {commit: commit.id} : {})})});
      if (ticket !== sequence) return;
      const heading = node("div", "repository-review-heading");
      const title = node("div", "repository-review-title");
      title.append(node("h2", "", commit ? `${state.name} · ${short(commit.sha)}` : `${state.name} · Branch comparison`));
      if (commit) title.append(node("p", "", commit.subject));
      title.append(node("p", "muted repository-pair", `${short(payload.pair.base)} → ${short(payload.pair.head)} · ${payload.pair.baseLabel}`));
      const controls = node("div", "repository-mode-controls"); controls.setAttribute("role", "group"); controls.setAttribute("aria-label", "Comparison layout");
      for (const value of ["split", "unified"]) {
        const option = button(value === "split" ? "Split" : "Unified", () => {
          mode = value; try { localStorage.setItem("repository-review-mode", mode); } catch (_) {}
          for (const item of controls.querySelectorAll("button")) item.setAttribute("aria-pressed", item === option ? "true" : "false");
          if (active) for (const record of active.sections.values()) if (record.content) renderFile(active, record);
        }); option.setAttribute("aria-pressed", mode === value ? "true" : "false"); controls.append(option);
      }
      heading.append(button("← Repositories", closeReview), title, controls);
      const changed = node("div", "notice warning repository-comparison-changed"); changed.hidden = true;
      changed.append(node("span", "", "The branch has changed. This view keeps the revisions shown above."), button("Refresh comparison", async () => { if (await loadHistory(state, 0, true)) openComparison(state); }));
      const body = node("div", "repository-review-body");
      const fileList = node("nav", "repository-file-list"); fileList.setAttribute("aria-label", "Changed files");
      fileList.append(node("p", "muted", `${payload.files.length} changed ${payload.files.length === 1 ? "file" : "files"}`));
      for (const file of payload.files) {
        const item = button("", () => selectFile(file), "repository-file quiet"); item.dataset.fileId = file.id;
        item.append(node("span", "repository-file-status", file.status), node("span", "repository-file-path", file.path));
        if (file.oldPath) item.title = `${file.oldPath} → ${file.path}`; fileList.append(item);
      }
      const scroll = node("section", "repository-file-scroll"); scroll.setAttribute("aria-label", "File comparisons");
      const sections = new Map();
      for (const file of payload.files) {
        const section = node("section", "repository-file-section"); section.dataset.fileId = file.id;
        const title = node("h3", "repository-file-title", file.path);
        const fileMetadata = node("div", "repository-file-metadata");
        const host = node("div", "repository-editor repository-editor-placeholder");
        host.append(node("p", "muted", "Scroll here to load this comparison."));
        section.append(title, fileMetadata, host); scroll.append(section);
        sections.set(file.id, {file, section, metadata: fileMetadata, host, editor: null, content: null, generation: 0, loading: false, used: 0});
      }
      body.append(fileList, scroll);
      review.replaceChildren(heading, changed);
      if (payload.pair.warning) review.append(node("p", "notice warning", payload.pair.warning));
      review.append(body);
      active = {state, pair: payload.pair, snapshot: payload.snapshot, branch: !commit, changed, fileList, scroll, sections, abort: new AbortController()};
      const selected = active;
      selected.observer = new IntersectionObserver(entries => {
        for (const entry of entries) if (entry.isIntersecting) loadFile(selected, sections.get(entry.target.dataset.fileId));
      }, {root: scroll, rootMargin: "600px 0px"});
      for (const record of sections.values()) selected.observer.observe(record.section);
      for (const event of ["wheel", "touchstart", "pointerdown", "keydown"]) scroll.addEventListener(event, () => { selected.pendingNavigation = null; }, {passive: true});
      scroll.addEventListener("scroll", () => { if (active === selected) trimEditors(selected); }, {passive: true});
      markChanged(state, state.latestHead);
      if (payload.files.length) selectFile(payload.files.find(file => file.path === previousFile) || payload.files[0]);
      else scroll.append(node("p", "empty", "No changed files between these revisions."));
    } catch (error) { if (ticket === sequence) review.replaceChildren(button("← Repositories", closeReview), node("p", "notice warning", error.message)); }
  };
  const hydrate = (card) => {
    const id = card.dataset.repositoryId;
    if (!id || states.has(id)) return;
    const state = {id, name: card.dataset.repositoryName, card, snapshot: null, pair: null, latestHead: card.dataset.repositoryHead || "", loading: false};
    states.set(id, state);
    card.querySelector("[data-review-branch]").addEventListener("click", () => openComparison(state));
    card.querySelector("[data-review-refresh]").addEventListener("click", () => loadHistory(state, 0, true));
    loadHistory(state);
  };
  for (const card of overview.querySelectorAll("[data-repository-id]")) hydrate(card);
  const checkHeads = async () => {
    if (checking || document.hidden || !element.classList.contains("active")) return;
    checking = true;
    try {
      await Promise.allSettled([...states.values()].map(async state => {
        try { const payload = await request(url("state", state.id)); markChanged(state, payload.head); }
        catch (_) { /* The next explicit refresh reports unavailable repositories. */ }
      }));
    } finally { checking = false; }
  };
  const interval = setInterval(checkHeads, 15000);
  const onSection = event => { if (event.detail === "repositories" || event.detail?.section === "repositories") checkHeads(); };
  document.addEventListener("session-section-change", onSection);
  return {
    updateHTML(html) {
      const candidate = node("div"); candidate.innerHTML = html;
      const incoming = new Map([...candidate.querySelectorAll("[data-repository-id]")].map(card => [card.dataset.repositoryId, card]));
      for (const [id, state] of states) {
        const fresh = incoming.get(id);
        if (!fresh) { state.card.remove(); states.delete(id); continue; }
        const oldStatus = state.card.querySelector("[data-repository-status]");
        const newStatus = fresh.querySelector("[data-repository-status]");
        if (oldStatus && newStatus && oldStatus.innerHTML !== newStatus.innerHTML) oldStatus.replaceChildren(...newStatus.childNodes);
        if (fresh.dataset.repositoryHead) markChanged(state, fresh.dataset.repositoryHead);
        incoming.delete(id);
      }
      let grid = overview.querySelector(".repo-grid");
      if (!grid) { overview.replaceChildren(...candidate.childNodes); grid = overview.querySelector(".repo-grid"); }
      for (const card of incoming.values()) { grid?.append(card); hydrate(card); }
      const heading = overview.querySelector(".section-heading span"); if (heading) heading.textContent = String(states.size);
      if (states.size) grid?.querySelector(":scope > .empty")?.remove();
      checkHeads();
    },
    destroy() { clearInterval(interval); document.removeEventListener("session-section-change", onSection); destroyEditors(); },
  };
}
