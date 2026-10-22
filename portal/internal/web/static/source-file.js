export function sourceFileLine(href) {
  const match = /^#L([1-9][0-9]*)$/.exec(new URL(href).hash);
  const line = match ? Number(match[1]) : null;
  return Number.isSafeInteger(line) ? line : null;
}

export function sourceFileURL(href, line) {
  const url = new URL(href);
  url.hash = Number.isSafeInteger(line) && line > 0 ? `L${line}` : "";
  return url.href;
}

export function sourceFileVersion(file) {
  switch (file.source) {
    case "worktree": return "Current worktree · includes uncommitted edits";
    case "archive": return `Archived final commit · ${file.revision}`;
    case "archived-artifact": return "Archived session file";
    case "artifact": return "Current session file";
    case "workspace": return "Current workspace · includes uncommitted edits";
    default: throw new Error("The file source is unknown. Reload the page to try again.");
  }
}

// Both file surfaces consume the same optional, server-sanitized preview.
export async function renderSourceFileContent({parent,file,mode,nonce,isCurrent,lineURL,onLineSelect}) {
  let editor = null, destroyed = false;
  const current = () => !destroyed && isCurrent();
  const body = document.createElement("div"); body.className = "source-file-body";
  const notice = text => {
    const message = document.createElement("p"); message.className = "notice";
    message.textContent = text; body.append(message);
  };
  const destroy = () => { destroyed = true; editor?.destroy?.(); body.remove(); };
  if (!current()) return {destroy};
  if (mode === "preview" && file.preview?.kind === "markdown" && typeof file.preview.html === "string") {
    body.classList.add("source-file-markdown");
    body.innerHTML = file.preview.html;
  } else if (mode === "preview" && file.preview?.kind === "image") {
    const url = new URL(file.preview.url, location.href);
    if (!file.preview.url.startsWith("/artifact-previews/") || url.origin !== location.origin || !url.pathname.startsWith("/artifact-previews/")) throw new Error("This image preview is unavailable.");
    const image = document.createElement("img"); image.alt = file.path;
    image.addEventListener("error", () => { if (current()) { image.remove(); notice("This image preview is unavailable. Download the file instead."); } });
    image.src = url.href; body.classList.add("source-file-image"); body.append(image);
  } else if (file.content.limited) {
    notice(`Preview omitted: the file exceeds 512 KiB or 12,000 lines (${file.content.bytes.toLocaleString()} bytes).`);
  } else if (file.content.binary || typeof file.content.text !== "string") {
    notice("This file cannot be displayed as UTF-8 text. Download the file instead.");
  } else {
    const {createReviewEditor} = await import("/static/review-editor.js");
    if (!current()) return {destroy};
    editor = createReviewEditor({parent: body, after: file.content.text, newPath: file.path,
      mode: "file", fileLabel: "Read-only file", nonce, lineURL, onLineSelect});
    if (lineURL && onLineSelect) {
      const href = location.href, line = sourceFileLine(href);
      editor.clearLine?.();
      if (line !== null) {
        const found = await editor.revealLine("new", line);
        if (!current() || location.href !== href) { destroy(); return {destroy}; }
        if (!found) notice(`Line ${line} is not present in this version of the file.`);
      } else if (location.hash) notice("This line reference is invalid.");
    }
    if (file.content.text === "") notice("This file is empty.");
  }
  if (current()) parent.append(body); else destroy();
  return {destroy};
}

export async function mountSourceFile(root) {
  const notice = document.getElementById("source-file-notice");
  const content = document.getElementById("source-file-content");
  const title = document.getElementById("source-file-title");
  const version = document.getElementById("source-file-version");
  const copy = document.getElementById("source-file-copy");
  const copyContent = document.getElementById("source-file-copy-content");
  const download = document.getElementById("source-file-download");
  const toggle = document.getElementById("source-file-mode");
  const api = root.dataset.fileApi || `/api/sessions/${encodeURIComponent(root.dataset.slug)}/file`;
  let file = null, mode = "raw", renderer = null, generation = 0, read = null, leaving = false, needsRender = false;
  const active = () => !document.hidden && !leaving;
  const announce = text => { notice.textContent = text; notice.hidden = !text; };
  const completeText = () => file && !file.content.binary && !file.content.limited && typeof file.content.text === "string" ? file.content.text : null;
  const previewable = () => ["markdown", "image"].includes(file?.preview?.kind);
  const controls = () => {
    copyContent.disabled = completeText() === null;
    if (toggle) {
      toggle.hidden = !previewable(); toggle.disabled = !file;
      toggle.title = toggle.ariaLabel = mode === "preview" ? "Show raw" : "Show preview";
      toggle.textContent = mode === "preview" ? "‹›" : "◉";
    }
  };
  const render = async () => {
    if (!file || !active()) { needsRender = true; return; }
    const ticket = ++generation; renderer?.destroy(); renderer = null; content.replaceChildren();
    needsRender = true; controls();
    try {
      const next = await renderSourceFileContent({parent: content, file, mode,
        nonce: document.querySelector('meta[name="style-nonce"]')?.content || "",
        isCurrent: () => ticket === generation && active(),
        lineURL: (_side, line) => sourceFileURL(location.href, line),
        onLineSelect: (_side, line) => { location.hash = `L${line}`; }});
      if (ticket !== generation || !active()) { next.destroy(); return; }
      renderer = next; needsRender = false;
    } catch (error) { if (ticket === generation && active()) { needsRender = false; announce(error.message || "Unable to display this file."); } }
  };
  copyContent?.addEventListener("click", async () => {
    const text = completeText(), ticket = generation;
    if (text === null) return;
    try { await navigator.clipboard.writeText(text); if (ticket === generation && active()) announce("Content copied."); }
    catch (_) { if (ticket === generation && active()) announce("Unable to copy the content. Download the file instead."); }
  });
  copy?.addEventListener("click", async () => {
    const ticket = generation;
    try { await navigator.clipboard.writeText(location.href); if (ticket === generation && active()) announce("Link copied."); }
    catch (_) { if (ticket === generation && active()) announce("Unable to copy the link. Copy the address from your browser."); }
  });
  toggle?.addEventListener("click", () => {
    mode = mode === "preview" ? "raw" : "preview";
    if (mode === "preview" && location.hash) history.replaceState(null, "", sourceFileURL(location.href, null));
    announce(""); void render().catch(error => announce(error.message));
  });
  const load = async () => {
    if (!active() || read || file) return;
    const abort = new AbortController(), ticket = ++generation;
    read = abort; const timeout = setTimeout(() => abort.abort(), 10000);
    announce("Loading file…"); controls();
    try {
      const response = await fetch(`${api}${location.search}`, {credentials: "same-origin", signal: abort.signal});
      const payload = await response.json().catch(() => ({}));
      if (ticket !== generation || !active()) return;
      if (abort.signal.aborted) throw new Error("The request timed out. Try again.");
      if (!response.ok) throw new Error(payload.error || "This file is unavailable. Check that the session and file still exist.");
      if (!payload.content || typeof payload.path !== "string") throw new Error("The file response is invalid.");
      file = payload;
      title.textContent = file.repository ? `${file.repository}/${file.path}` : file.path;
      document.title = `${title.textContent} · File`; version.textContent = sourceFileVersion(file);
      download.href = `${api}/download${location.search}`; download.hidden = false;
      mode = previewable() && !location.hash ? "preview" : "raw";
      announce(""); await render();
    } catch (error) {
      if (ticket === generation && active()) announce(error.message || "Unable to display this file.");
    } finally { clearTimeout(timeout); if (read === abort) read = null; }
  };
  addEventListener("hashchange", () => { mode = "raw"; announce(""); void render().catch(error => announce(error.message)); });
  const pause = () => { ++generation; read?.abort(); read = null; };
  document.addEventListener("visibilitychange", () => {
    if (!active()) pause(); else if (!file) void load(); else if (needsRender) void render();
  });
  addEventListener("pagehide", () => { leaving = true; pause(); });
  addEventListener("pageshow", () => { leaving = false; if (!file) void load(); else if (needsRender) void render(); });
  await load();
}

if (typeof document !== "undefined") {
  const root = document.getElementById("source-file");
  if (root) void mountSourceFile(root);
}
