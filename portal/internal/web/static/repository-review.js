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
const short = sha => String(sha || "").slice(0, 10);
const normalClick = event => event.button === 0 && !event.metaKey && !event.ctrlKey && !event.shiftKey && !event.altKey;
const request = async (url, options = {}) => {
  const response = await fetch(url, {credentials: "same-origin", ...options});
  const payload = await response.json();
  if (!response.ok) throw new Error(payload.error || "Review request failed (" + response.status + ")");
  return payload;
};
const readMode = () => {
  try { return localStorage.getItem("repository-review-mode") === "unified" ? "unified" : "split"; }
  catch (_) { return "split"; }
};
const routeKeys = ["repository", "review", "commit", "file", "view", "layout", "version"];
export function reviewRoute(href, fallbackLayout = "split") {
  const url = new URL(href);
  const line = /^#(old|new)-L([1-9][0-9]*)$/.exec(url.hash);
  return {
    repository: url.searchParams.get("repository") || "",
    review: url.searchParams.get("review") || "",
    commit: url.searchParams.get("commit") || "",
    file: url.searchParams.get("file") || "",
    view: url.searchParams.get("view") === "file" ? "file" : "diff",
    layout: ["split", "unified"].includes(url.searchParams.get("layout")) ? url.searchParams.get("layout") : fallbackLayout,
    version: ["old", "new"].includes(url.searchParams.get("version")) ? url.searchParams.get("version") : "",
    line: line && Number.isSafeInteger(Number(line[2])) ? {side: line[1], number: Number(line[2])} : null,
  };
}
export function reviewURL(href, route = {}) {
  const url = new URL(href);
  for (const key of routeKeys) url.searchParams.delete(key);
  url.searchParams.set("tab", "repositories");
  for (const key of routeKeys) if (route[key]) url.searchParams.set(key, route[key]);
  url.hash = route.line ? route.line.side + "-L" + route.line.number : "";
  return url.href;
}
export function fileStatus(status) {
  return ({A: ["added", "Added"], M: ["modified", "Modified"], D: ["deleted", "Deleted"],
    R: ["renamed", "Renamed"], C: ["copied", "Copied"], T: ["type", "Type changed"]})[String(status)[0]] || ["other", "Changed"];
}
export function changeCounts(stats, total = false) {
  if (!stats) return "";
  if (!total && (stats.additions === null || stats.deletions === null)) return "Binary";
  const parts = [];
  if (total) parts.push(stats.files + " changed " + (stats.files === 1 ? "file" : "files"));
  parts.push("+" + Number(stats.additions || 0).toLocaleString(), "−" + Number(stats.deletions || 0).toLocaleString());
  if (total && stats.binaryFiles) parts.push(stats.binaryFiles + " binary " + (stats.binaryFiles === 1 ? "file" : "files"));
  return parts.join(" · ");
}
export function fullFileVersion(file, requested) {
  if (file.newMode === "000000") return "old";
  if (file.oldMode === "000000") return "new";
  return requested === "old" ? "old" : "new";
}

export function mount({slug, nonce, element, createCopyButton}) {
  if (!document.querySelector('link[data-repository-review-styles]')) {
    const sheet = node("link"); sheet.rel = "stylesheet"; sheet.href = "/static/repository-review.css";
    sheet.dataset.repositoryReviewStyles = "true"; document.head.append(sheet);
  }
  const preload = () => {
    editorModule ||= import(editorAsset).catch(error => { editorModule = null; throw error; });
    return editorModule;
  };
  const overview = node("div", "repository-overview"); overview.append(...element.childNodes);
  const review = node("section", "repository-comparison"); review.hidden = true;
  element.append(overview, review);
  const states = new Map();
  let active = null, sequence = 0, checking = false, destroyed = false;
  let opening = null;
  let route = reviewRoute(location.href, readMode());
  const historyQueue = new Set();
  let historyScheduled = false;
  const url = (operation, id, values = {}) => {
    const query = new URLSearchParams();
    if (id) query.set("repository", id);
    for (const [key, value] of Object.entries(values)) for (const item of Array.isArray(value) ? value : [value]) {
      if (item !== undefined && item !== null && item !== "") query.append(key, String(item));
    }
    return "/api/sessions/" + encodeURIComponent(slug) + "/repository-" + operation + "?" + query;
  };
  const showFailure = (target, error) => target.replaceChildren(node("p", "notice warning", error.message));
  const copy = (text, label) => createCopyButton({text, label});
  const link = (text, href, action, className = "") => {
    const result = node("a", className, text); result.href = href;
    result.addEventListener("click", event => {
      if (!normalClick(event)) return;
      event.preventDefault(); action();
    });
    return result;
  };
  const setRoute = (next, replace = false) => {
    route = next;
    const href = reviewURL(location.href, next);
    if (href !== location.href) history[replace ? "replaceState" : "pushState"](null, "", href);
    refreshLinks();
  };
  const comparisonRoute = (state, commit = null) => ({
    repository: state.id, review: state.review, commit: commit?.sha || "", view: "diff", layout: readMode(),
  });
  const destroyEditors = () => {
    opening?.abort(); opening = null;
    if (!active) return;
    active.observer?.disconnect(); active.abort.abort();
    for (const record of active.sections.values()) { ++record.generation; record.editor?.destroy(); }
  };
  const markChanged = (state, head) => {
    state.latestHead = head;
    state.card.querySelector(".repository-head-change").hidden = !state.pair || !head || state.pair.head === head;
    if (active?.state === state) active.changed.hidden = !head || active.historyHead === head;
  };
  const renderHistory = (state, payload) => {
    state.snapshot = payload.snapshot; state.review = payload.review;
    state.pair = payload.pair; state.page = payload.history.page;
    const branch = state.card.querySelector("[data-review-branch]");
    const compare = link("Compare", reviewURL(location.href, comparisonRoute(state)),
      () => navigate(comparisonRoute(state)));
    compare.dataset.reviewBranch = "";
    branch.replaceWith(compare);
    const target = state.card.querySelector("[data-repository-commits]");
    const list = node("ol", "repository-commit-list");
    for (const commit of payload.history.commits) {
      const item = node("li", "repository-commit");
      const row = node("div", "repository-commit-row");
      const subjectGroup = node("div", "repository-commit-subject-group");
      const next = comparisonRoute(state, commit);
      const subject = link(commit.subject || "(No commit subject)", reviewURL(location.href, next),
        () => navigate(next, commit), "repository-commit-subject");
      subjectGroup.append(subject);
      if (commit.body) {
        const body = node("pre", "repository-commit-message", commit.body); body.hidden = true;
        body.id = "commit-body-" + state.id + "-" + commit.id;
        const expand = button("…", () => {
          body.hidden = !body.hidden; expand.setAttribute("aria-expanded", String(!body.hidden));
        }, "repository-commit-expand quiet");
        expand.setAttribute("aria-label", "Show commit message");
        expand.setAttribute("aria-controls", body.id); expand.setAttribute("aria-expanded", "false");
        subjectGroup.append(expand); item.append(body);
      }
      const identity = node("div", "repository-commit-identity");
      identity.append(node("code", "repository-commit-sha", short(commit.sha)), copy(commit.sha, "Copy commit hash"));
      if (commit.url) {
        const external = node("a", "repository-commit-link", "↗");
        external.href = commit.url; external.target = "_blank"; external.rel = "noreferrer";
        external.title = "View commit on GitHub"; external.setAttribute("aria-label", "View commit " + short(commit.sha) + " on GitHub");
        identity.append(external);
      }
      row.append(subjectGroup, identity); item.prepend(row); list.append(item);
    }
    target.replaceChildren(node("p", "muted repository-history-base", short(payload.pair.base) + " → " +
      short(payload.pair.head) + " · " + payload.pair.baseLabel));
    if (payload.pair.warning) target.append(node("p", "notice warning", payload.pair.warning));
    target.append(list);
    if (!payload.history.commits.length) target.append(node("p", "muted", "No commits in this comparison."));
    const pagination = node("nav", "repository-pagination"); pagination.setAttribute("aria-label", "Commit history pages");
    const previous = button("Previous", () => loadHistory(state, state.page - 1)); previous.disabled = state.page === 0;
    const next = button("Next", () => loadHistory(state, state.page + 1)); next.disabled = !payload.history.hasMore;
    pagination.append(previous, node("span", "muted", "Page " + (state.page + 1)), next); target.append(pagination);
    markChanged(state, state.latestHead || payload.pair.head);
  };
  const loadingHistory = (state, value) => {
    state.loading = value; state.card.querySelector("[data-review-refresh]").disabled = value;
  };
  const loadHistory = async (state, page = 0, refresh = false) => {
    if (state.loading || destroyed) return false;
    loadingHistory(state, true);
    try {
      const payload = await request(url("history", state.id, {page, snapshot: refresh ? "" : state.snapshot}));
      if (!states.has(state.id) || destroyed) return false;
      renderHistory(state, payload);
      if (refresh) markChanged(state, payload.pair.head);
      return true;
    } catch (error) { showFailure(state.card.querySelector("[data-repository-commits]"), error); return false; }
    finally { loadingHistory(state, false); }
  };
  const flushHistories = async () => {
    historyScheduled = false;
    const items = [...historyQueue]; historyQueue.clear();
    for (let offset = 0; offset < items.length && !destroyed; offset += 8) {
      const batch = items.slice(offset, offset + 8).filter(state => states.has(state.id) && !state.loading);
      if (!batch.length) continue;
      batch.forEach(state => loadingHistory(state, true));
      try {
        const payload = await request(url("histories", null, {repository: batch.map(state => state.id)}));
        const results = new Map(payload.repositories.map(result => [result.repository, result]));
        for (const state of batch) {
          if (!states.has(state.id) || destroyed) continue;
          const result = results.get(state.id);
          if (!result || result.error) showFailure(state.card.querySelector("[data-repository-commits]"), new Error(result?.error || "Repository history is unavailable."));
          else renderHistory(state, result);
        }
      } catch (error) {
        for (const state of batch) showFailure(state.card.querySelector("[data-repository-commits]"), error);
      } finally { batch.forEach(state => loadingHistory(state, false)); }
    }
  };
  const queueHistory = state => {
    historyQueue.add(state);
    if (!historyScheduled) { historyScheduled = true; queueMicrotask(flushHistories); }
  };
  const metadata = (file, content) => {
    const lines = [];
    if (file.oldPath) lines.push("Renamed from " + file.oldPath);
    if (file.oldMode !== file.newMode) lines.push("Mode: " + file.oldMode + " → " + file.newMode);
    for (const [label, blob] of [["Before", content.before], ["After", content.after]]) {
      if (blob.kind === "absent") lines.push(label + ": file absent");
      if (blob.kind === "symlink") lines.push(label + ": symbolic link target");
      if (blob.kind === "submodule") lines.push(label + ": submodule commit " + blob.text.trim());
      if (blob.binary) lines.push(label + ": binary or non-UTF-8 data (" + blob.bytes.toLocaleString() + " bytes)");
      if (blob.limited) lines.push(label + ": preview omitted (limit: 512 KiB or 12,000 lines; " + blob.bytes.toLocaleString() + " bytes)");
      if (blob.missingNewline) lines.push(label + ": no newline at end of file");
    }
    return lines;
  };
  const trimEditors = (selected, current = null) => {
    const mounted = [...selected.sections.values()].filter(record => record.editor || record.content).sort((a, b) => a.used - b.used);
    let count = mounted.length;
    for (const record of mounted) {
      if (count <= 8) break;
      if (record.file.id === route.file || record === current) continue;
      record.host.style.minHeight = Math.max(160, record.host.getBoundingClientRect().height) + "px";
      ++record.generation; record.editor?.destroy(); record.editor = null; record.content = null; count--;
      record.host.replaceChildren(node("p", "muted", "Scroll here to load this comparison."));
    }
  };
  const fileRoute = (file, changes = {}) => {
    const next = {...route, file: file.id, line: null, ...changes};
    if (next.view === "file") next.version = fullFileVersion(file, next.version);
    return next;
  };
  const parentRoute = sha => ({repository: route.repository, review: route.review, commit: sha, layout: route.layout});
  const refreshLinks = () => {
    if (!active) return;
    for (const parent of review.querySelectorAll(".repository-parent-link")) parent.href = reviewURL(location.href, parentRoute(parent.dataset.commit));
    for (const record of active.sections.values()) {
      record.nav.href = reviewURL(location.href, fileRoute(record.file));
      record.nav.setAttribute("aria-current", record.file.id === route.file ? "true" : "false");
      record.fileLink.href = reviewURL(location.href, fileRoute(record.file, {view: "file", version: fullFileVersion(record.file, route.version)}));
      record.diffLink.href = reviewURL(location.href, fileRoute(record.file, {view: "diff", version: ""}));
    }
  };
  const reveal = async (selected, record) => {
    if (active !== selected || route.file !== record.file.id) return;
    selected.scroll.scrollTop += record.section.getBoundingClientRect().top - selected.scroll.getBoundingClientRect().top;
    if (route.line && record.editor) {
      const target = route.line;
      const found = await record.editor.revealLine(target.side, target.number);
      if (active !== selected || route.line !== target) return;
      selected.lineNotice.hidden = found;
      if (!found) selected.lineNotice.textContent = "This line is not available in the selected file version.";
    }
  };
  const renderFile = async (selected, record) => {
    if (!record.content || active !== selected) return;
    trimEditors(selected, record);
    const fileView = route.view === "file" && record.file.id === route.file;
    const version = fullFileVersion(record.file, route.version);
    const editorMode = fileView ? "file" : route.layout;
    const key = editorMode + ":" + version;
    if (record.editor && record.editorKey === key) return;
    const content = record.content;
    const generation = ++record.generation;
    record.editor?.destroy(); record.editor = null; record.editorKey = key;
    record.host.replaceChildren();
    const blobs = fileView ? [version === "old" ? content.before : content.after] : [content.before, content.after];
    if (blobs.some(blob => blob.binary || blob.limited)) {
      record.host.style.minHeight = "0px";
      record.host.append(node("p", "muted", "Text preview is unavailable for this file. Review the metadata above or inspect it locally."));
      return;
    }
    record.host.append(node("p", "muted", "Loading file view…"));
    try {
      const {createReviewEditor} = await preload();
      if (active !== selected || generation !== record.generation) return;
      record.host.replaceChildren(); record.host.style.minHeight = "0px";
      record.editor = createReviewEditor({
        parent: record.host, before: content.before.text, after: content.after.text,
        oldPath: record.file.oldPath || record.file.path, newPath: record.file.path,
        mode: editorMode, version, nonce,
        lineURL: (side, number) => reviewURL(location.href, fileRoute(record.file, {line: {side, number}})),
        onLineSelect: (side, number) => navigate(fileRoute(record.file, {line: {side, number}})),
      });
      record.used = performance.now();
      void record.editor.ready.then(() => {
        if (active !== selected || generation !== record.generation) return;
        trimEditors(selected);
        const destination = selected.sections.get(route.file);
        if (selected.pendingNavigation && destination?.editor) {
          requestAnimationFrame(() => { if (active === selected && selected.pendingNavigation) void reveal(selected, destination); });
        }
      });
    } catch (error) {
      if (active === selected && generation === record.generation) showFailure(record.host, error);
    }
  };
  const assignContent = (record, content) => {
    record.content = content; record.metadata.replaceChildren();
    for (const line of metadata(record.file, content)) record.metadata.append(node("p", "", line));
  };
  const pumpFiles = selected => {
    if (active !== selected) return;
    while (selected.jobs < 2 && selected.fileQueue.size) {
      const batch = [...selected.fileQueue].slice(0, 4);
      batch.forEach(record => selected.fileQueue.delete(record));
      selected.jobs++;
      void (async () => {
        try {
          const payload = await request(url("files", selected.state.id, {snapshot: selected.snapshot, file: batch.map(record => record.file.id)}), {signal: selected.abort.signal});
          if (active !== selected) return;
          const results = new Map(payload.files.map(result => [result.file, result]));
          await Promise.all(batch.map(async record => {
            const result = results.get(record.file.id);
            if (!result || result.error) throw Object.assign(new Error(result?.error || "File content is unavailable."), {record});
            assignContent(record, result.content);
            if (!record.section.hidden) await renderFile(selected, record);
          }).map(promise => promise.catch(error => {
            if (error.record) showFailure(error.record.host, error);
            else throw error;
          })));
        } catch (error) {
          if (active === selected && error.name !== "AbortError") for (const record of batch) if (!record.content) showFailure(record.host, error);
        } finally {
          selected.jobs--;
          for (const record of batch) { record.loading = false; record.resolveLoad?.(); record.resolveLoad = null; }
          if (active === selected) { trimEditors(selected); pumpFiles(selected); }
        }
      })();
    }
  };
  const loadFile = (selected, record, priority = false) => {
    record.used = performance.now();
    if (record.content) return renderFile(selected, record);
    if (record.loading) {
      if (priority && selected.fileQueue.has(record)) selected.fileQueue = new Set([record, ...selected.fileQueue]);
      return record.loadPromise;
    }
    record.loading = true;
    record.host.replaceChildren(node("p", "muted", "Loading file…"));
    record.loadPromise = new Promise(resolve => { record.resolveLoad = resolve; });
    selected.fileQueue = new Set(priority ? [record, ...selected.fileQueue] : [...selected.fileQueue, record]);
    queueMicrotask(() => pumpFiles(selected));
    return record.loadPromise;
  };
  const applyView = async () => {
    if (!active) return;
    const selected = active;
    selected.pendingNavigation = true;
    selected.lineNotice.hidden = true;
    for (const item of selected.controls.querySelectorAll("[data-layout]")) item.setAttribute("aria-pressed", String(item.dataset.layout === route.layout));
    selected.controls.hidden = route.view === "file";
    const record = selected.sections.get(route.file);
    for (const item of selected.sections.values()) {
      item.section.hidden = route.view === "file" && item !== record;
      item.fileLink.hidden = route.view === "file";
      item.diffLink.hidden = route.view !== "file";
      item.versions.hidden = route.view !== "file";
      const version = fullFileVersion(item.file, route.version);
      for (const option of item.versions.querySelectorAll("[data-version]")) option.setAttribute("aria-pressed", String(option.dataset.version === version));
    }
    refreshLinks();
    if (!record) {
      if (route.file) { selected.lineNotice.hidden = false; selected.lineNotice.textContent = "This file is not part of the comparison."; }
      return;
    }
    const others = [...selected.sections.values()].filter(item => item !== record && !item.section.hidden && item.content);
    void Promise.all(others.map(item => renderFile(selected, item)));
    await loadFile(selected, record, true);
    if (active === selected) await reveal(selected, record);
  };
  const closeReview = (update = true) => {
    ++sequence; destroyEditors(); active = null; review.hidden = true; overview.hidden = false;
    element.classList.remove("repository-review-open");
    if (update) setRoute({layout: readMode()});
  };
  const commitHeading = (title, state, commit, pair) => {
    title.append(node("h2", "", state.name + (commit ? " · " + short(commit.sha) : " · Branch comparison")));
    if (commit) {
      const identity = node("div", "repository-commit-detail-identity");
      identity.append(node("code", "", commit.sha), copy(commit.sha, "Copy commit hash"));
      if (commit.author) identity.append(node("span", "muted", commit.author + " · " + new Date(commit.date).toLocaleString()));
      title.append(identity, node("pre", "repository-commit-full-message", commit.message || [commit.subject, commit.body].filter(Boolean).join("\n\n")));
      if (Array.isArray(commit.parents)) {
        const parents = node("div", "muted repository-commit-parents");
        parents.append(commit.parents.length ? (commit.parents.length === 1 ? "Parent " : "Parents ") : "No parent");
        for (const sha of commit.parents) {
          const parent = link(short(sha), reviewURL(location.href, parentRoute(sha)), () => navigate(parentRoute(sha)), "repository-parent-link");
          parent.dataset.commit = sha;
          parent.title = "View parent commit " + sha;
          parent.setAttribute("aria-label", "View parent commit " + short(sha));
          parents.append(parent, copy(sha, "Copy parent hash"));
        }
        if (commit.parents.length > 1) parents.append(node("span", "", "Diff against first parent"));
        title.append(parents);
      }
    }
    if (pair && !commit) {
      const identity = node("div", "muted repository-pair");
      identity.append(node("code", "", short(pair.base)), copy(pair.base, "Copy base hash"), document.createTextNode(" → "),
        node("code", "", short(pair.head)), copy(pair.head, "Copy head hash"), document.createTextNode(" · " + pair.baseLabel));
      title.append(identity);
    }
  };
  const openComparison = async (state, hint = null) => {
    const ticket = ++sequence;
    destroyEditors(); active = null;
    const requested = {...route};
    const abort = new AbortController(); opening = abort;
    overview.hidden = true; review.hidden = false; element.classList.add("repository-review-open");
    const initialTitle = node("div", "repository-review-title");
    if (hint) commitHeading(initialTitle, state, hint);
    review.replaceChildren(button("← Repositories", () => closeReview()), initialTitle, node("p", "muted", "Loading comparison…"));
    try {
      const payload = await request(url("comparison", state.id, {review: requested.review, commit: requested.commit, file: requested.file}), {signal: abort.signal});
      if (ticket !== sequence || destroyed) return;
      const heading = node("div", "repository-review-heading");
      const title = node("div", "repository-review-title");
      commitHeading(title, state, payload.commit || hint, payload.pair);
      title.append(node("p", "repository-comparison-stats", changeCounts(payload.stats, true)));
      const controls = node("div", "repository-mode-controls"); controls.setAttribute("role", "group"); controls.setAttribute("aria-label", "Comparison layout");
      for (const value of ["split", "unified"]) {
        const option = button(value === "split" ? "Split" : "Unified", () => {
          try { localStorage.setItem("repository-review-mode", value); } catch (_) {}
          navigate({...route, layout: value});
        });
        option.dataset.layout = value; controls.append(option);
      }
      heading.append(button("← Repositories", () => closeReview()), title, copy(() => location.href, "Copy comparison link"), controls);
      const changed = node("div", "notice warning repository-comparison-changed"); changed.hidden = true;
      changed.append(node("span", "", "The branch has changed. This view keeps the revisions shown above."),
        button("Refresh comparison", async () => { if (await loadHistory(state, 0, true)) navigate(comparisonRoute(state)); }));
      const lineNotice = node("p", "notice warning"); lineNotice.hidden = true;
      const body = node("div", "repository-review-body");
      const fileList = node("nav", "repository-file-list"); fileList.setAttribute("aria-label", "Changed files");
      fileList.append(node("p", "muted", changeCounts(payload.stats, true)));
      const scroll = node("section", "repository-file-scroll"); scroll.setAttribute("aria-label", "File comparisons");
      const sections = new Map();
      for (const file of payload.files) {
        const [kind, label] = fileStatus(file.status);
        const nav = link("", reviewURL(location.href, fileRoute(file)), () => navigate(fileRoute(file)), "repository-file");
        nav.dataset.fileId = file.id;
        const name = node("span", "repository-file-name");
        name.append(node("span", "repository-file-path", file.path), node("span", "repository-file-counts", changeCounts(file)));
        nav.append(node("span", "repository-file-status status-" + kind, label), name);
        if (file.oldPath) nav.title = file.oldPath + " → " + file.path;
        fileList.append(nav);
        const section = node("section", "repository-file-section"); section.dataset.fileId = file.id;
        const fileTitle = node("div", "repository-file-title");
        fileTitle.append(node("h3", "", file.path), node("span", "repository-file-status status-" + kind, label),
          node("span", "repository-file-counts", changeCounts(file)));
        const fileLink = link("View file", "", () => navigate(fileRoute(file, {view: "file", version: fullFileVersion(file, route.version)})));
        const diffLink = link("View diff", "", () => navigate(fileRoute(file, {view: "diff", version: ""})));
        const versions = node("div", "repository-mode-controls"); versions.setAttribute("role", "group"); versions.setAttribute("aria-label", "File version");
        for (const [value, label] of [["old", "Before"], ["new", "After"]]) {
          const option = button(label, () => navigate(fileRoute(file, {view: "file", version: value})));
          option.dataset.version = value; option.disabled = (value === "old" ? file.oldMode : file.newMode) === "000000";
          versions.append(option);
        }
        fileTitle.append(fileLink, diffLink, versions);
        const fileMetadata = node("div", "repository-file-metadata");
        const host = node("div", "repository-editor repository-editor-placeholder");
        host.append(node("p", "muted", "Scroll here to load this comparison."));
        section.append(fileTitle, fileMetadata, host); scroll.append(section);
        sections.set(file.id, {file, nav, fileLink, diffLink, versions, section, metadata: fileMetadata, host, editor: null, content: null, generation: 0, loading: false, used: 0});
      }
      body.append(fileList, scroll);
      review.replaceChildren(heading, changed, lineNotice);
      if (payload.pair.warning) review.append(node("p", "notice warning", payload.pair.warning));
      review.append(body);
      active = {state, pair: payload.pair, snapshot: payload.snapshot, review: payload.review, commit: requested.commit,
        historyHead: payload.historyHead || state.pair?.head || payload.pair.head, changed, controls, lineNotice,
        fileList, scroll, sections, abort, fileQueue: new Set(), jobs: 0};
      opening = null;
      const selected = active;
      if (payload.preview && sections.has(payload.preview.file)) assignContent(sections.get(payload.preview.file), payload.preview.content);
      if (!route.file && payload.files.length) {
        setRoute({...route, file: payload.files[0].id, version: route.view === "file" ? fullFileVersion(payload.files[0], route.version) : ""}, true);
      }
      selected.observer = new IntersectionObserver(entries => {
        for (const entry of entries) {
          const record = sections.get(entry.target.dataset.fileId);
          if (entry.isIntersecting && !record.section.hidden && active === selected) void loadFile(selected, record);
        }
      }, {root: scroll, rootMargin: "600px 0px"});
      for (const record of sections.values()) selected.observer.observe(record.section);
      for (const event of ["wheel", "touchstart", "pointerdown", "keydown"]) {
        scroll.addEventListener(event, () => { selected.pendingNavigation = false; }, {passive: true});
      }
      scroll.addEventListener("scroll", () => { if (active === selected) trimEditors(selected); }, {passive: true});
      markChanged(state, state.latestHead);
      await applyView();
      if (!payload.files.length) scroll.append(node("p", "empty", "No changed files between these revisions."));
    } catch (error) {
      if (ticket === sequence && error.name !== "AbortError") review.replaceChildren(button("← Repositories", () => closeReview()), node("p", "notice warning", error.message));
    }
  };
  const restore = (hint = null) => {
    if (!route.repository || !route.review) { closeReview(false); return; }
    const state = states.get(route.repository);
    if (!state) {
      ++sequence; destroyEditors(); active = null; overview.hidden = true; review.hidden = false;
      element.classList.add("repository-review-open");
      review.replaceChildren(button("← Repositories", () => closeReview()), node("p", "notice warning", "This repository is not available in this session."));
      return;
    }
    if (active && active.state === state && active.review === route.review && active.commit === route.commit) void applyView();
    else void openComparison(state, hint);
  };
  function navigate(next, hint = null) { setRoute(next); restore(hint); }
  const onURL = () => {
    route = reviewRoute(location.href, readMode());
    restore();
  };
  const hydrate = card => {
    const id = card.dataset.repositoryId;
    if (!id || states.has(id)) return;
    const state = {id, name: card.dataset.repositoryName, card, snapshot: null, review: null, pair: null,
      latestHead: card.dataset.repositoryHead || "", loading: false};
    states.set(id, state);
    card.querySelector("[data-review-branch]").addEventListener("click", event => {
      if (!state.review || !normalClick(event)) return;
      event.preventDefault(); navigate(comparisonRoute(state));
    });
    card.querySelector("[data-review-refresh]").addEventListener("click", () => loadHistory(state, 0, true));
    queueHistory(state);
  };
  for (const card of overview.querySelectorAll("[data-repository-id]")) hydrate(card);
  const checkHeads = async () => {
    if (checking || destroyed || document.hidden || !element.classList.contains("active")) return;
    checking = true;
    try {
      const items = [...states.values()];
      for (let offset = 0; offset < items.length; offset += 32) {
        const payload = await request(url("states", null, {repository: items.slice(offset, offset + 32).map(state => state.id)}));
        for (const result of payload.repositories) if (result.head && states.has(result.repository)) markChanged(states.get(result.repository), result.head);
      }
    } catch (_) { /* Explicit refresh reports unavailable repositories. */ }
    finally { checking = false; }
  };
  const interval = setInterval(checkHeads, 15000);
  const onSection = event => { if (event.detail === "repositories" || event.detail?.section === "repositories") void checkHeads(); };
  document.addEventListener("session-section-change", onSection);
  addEventListener("popstate", onURL); addEventListener("hashchange", onURL);
  restore();
  return {
    updateHTML(html) {
      const candidate = node("div"); candidate.innerHTML = html;
      const incoming = new Map([...candidate.querySelectorAll("[data-repository-id]")].map(card => [card.dataset.repositoryId, card]));
      for (const [id, state] of states) {
        const fresh = incoming.get(id);
        if (!fresh) { state.card.remove(); states.delete(id); if (active?.state === state) restore(); continue; }
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
    },
    destroy() {
      destroyed = true; ++sequence; clearInterval(interval);
      document.removeEventListener("session-section-change", onSection);
      removeEventListener("popstate", onURL); removeEventListener("hashchange", onURL); destroyEditors();
    },
  };
}
