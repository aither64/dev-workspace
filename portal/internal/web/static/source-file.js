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
    case "archived-artifact": return "Archived session artifact";
    case "artifact": return "Current session artifact";
    default: throw new Error("The file source is unknown. Reload the page to try again.");
  }
}

export async function mountSourceFile(root) {
  const notice = document.getElementById("source-file-notice");
  const content = document.getElementById("source-file-content");
  const title = document.getElementById("source-file-title");
  const version = document.getElementById("source-file-version");
  const copy = document.getElementById("source-file-copy");
  const announce = text => { notice.textContent = text; notice.hidden = !text; };
  copy.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(location.href);
      copy.textContent = "Copied";
    } catch (_) {
      announce("Unable to copy the link. Copy the address from your browser.");
    }
  });
  try {
    const response = await fetch(`/api/sessions/${encodeURIComponent(root.dataset.slug)}/file${location.search}`, {credentials: "same-origin"});
    const file = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(file.error || "This file is unavailable. Check that the session and file still exist.");
    title.textContent = file.repository ? `${file.repository}/${file.path}` : file.path;
    document.title = `${title.textContent} · File`;
    version.textContent = sourceFileVersion(file);
    if (file.content.limited) {
      announce(`Preview omitted: the file exceeds 512 KiB or 12,000 lines (${file.content.bytes.toLocaleString()} bytes).`);
      return;
    }
    if (file.content.binary) {
      announce("This file cannot be displayed as UTF-8 text.");
      return;
    }
    const {createReviewEditor} = await import("/static/review-editor.js");
    const editor = createReviewEditor({
      parent: content, after: file.content.text, newPath: file.path, mode: "file", fileLabel: "Read-only file",
      nonce: document.querySelector('meta[name="style-nonce"]').content,
      lineURL: (_side, line) => sourceFileURL(location.href, line),
      onLineSelect: (_side, line) => { location.hash = `L${line}`; },
    });
    const reveal = async () => {
      const href = location.href;
      const line = sourceFileLine(href);
      // A retained page can load the previous editor bundle during rollback.
      editor.clearLine?.();
      if (line !== null) {
        const found = await editor.revealLine("new", line);
        if (location.href !== href) return;
        announce(found ? "" : `Line ${line} is not present in this version of the file.`);
      } else {
        announce(location.hash ? "This line reference is invalid." : file.content.text === "" ? "This file is empty." : "");
      }
      copy.textContent = "Copy link";
    };
    addEventListener("hashchange", reveal);
    await reveal();
  } catch (error) {
    announce(error.message || "Unable to display this file.");
  }
}

if (typeof document !== "undefined") {
  const root = document.getElementById("source-file");
  if (root) void mountSourceFile(root);
}
