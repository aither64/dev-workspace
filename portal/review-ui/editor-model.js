import {presentableDiff as characterDiff} from "@codemirror/merge";

export const DIFF_CONFIG = {scanLimit: 500, timeout: 100};

export function normalizeSource(text) {
  return String(text ?? "").replace(/\r\n/g, "\n");
}

export function sourceLines(text) {
  const lines = [];
  let from = 0;
  for (const value of text.split("\n")) {
    if (from >= text.length) break;
    lines.push({text: value, from, number: lines.length + 1});
    from += value.length + 1;
  }
  return lines;
}

// Keep this allowlist aligned with the grammars compiled into the worker.
export function languageForPath(path, text = "") {
  const name = String(path ?? "").split("/").at(-1).toLowerCase();
  if (/^(gemfile|rakefile|vagrantfile|berksfile|podfile)(\.|$)/.test(name)) return "ruby";
  if (/^(dockerfile|containerfile)(\.|$)/.test(name)) return "dockerfile";
  if (/^(makefile|gnumakefile)$/.test(name)) return "makefile";
  if (/^(\.bashrc|\.bash_profile|\.profile|\.zshrc|pkgbuild)$/.test(name)) return "shellscript";
  const extension = name.split(".").at(-1);
  const extensions = {
    nix: "nix", rb: "ruby", rake: "ruby", gemspec: "ruby", go: "go",
    js: "javascript", mjs: "javascript", cjs: "javascript", jsx: "jsx",
    ts: "typescript", mts: "typescript", cts: "typescript", tsx: "tsx",
    sh: "shellscript", bash: "shellscript", zsh: "shellscript", ksh: "shellscript",
    yaml: "yaml", yml: "yaml", json: "json", jsonc: "jsonc", hcl: "hcl", tf: "hcl", tfvars: "hcl",
    py: "python", php: "php", html: "html", htm: "html", xml: "xml", css: "css", scss: "scss",
    md: "markdown", markdown: "markdown", toml: "toml", ini: "ini", sql: "sql", rs: "rust",
    c: "c", h: "c", cc: "cpp", cpp: "cpp", hpp: "cpp", mk: "makefile", diff: "diff", patch: "diff",
  };
  if (extensions[extension]) return extensions[extension];
  const interpreter = text.split("\n", 1)[0];
  if (/^#!.*\b(?:ba|z|k)?sh\b/.test(interpreter)) return "shellscript";
  if (/^#!.*\bpython[\d.]*\b/.test(interpreter)) return "python";
  if (/^#!.*\bruby\b/.test(interpreter)) return "ruby";
  return null;
}

// Git alone classifies changed lines. The bounded character diff is cosmetic
// and sees only one Git change block at a time, never unchanged source lines.
// Keep presentation cleanup: raw diffs leave incidental matching letters and
// spaces unmarked inside otherwise replaced words and unrelated statements.
export function reviewProjection(before, after, diff, split = false) {
  const oldLines = sourceLines(before), newLines = sourceLines(after);
  if (!Array.isArray(diff?.changes)) throw new Error("Exact Git diff is unavailable. Reload this page to try again.");
  let oldEnd = 0, newEnd = 0;
  for (const change of diff.changes) {
    const {oldStart, oldLines: oldCount, newStart, newLines: newCount} = change;
    if (![oldStart, oldCount, newStart, newCount].every(n => Number.isSafeInteger(n) && n >= 0) ||
        oldStart < oldEnd || newStart < newEnd || oldStart - oldEnd !== newStart - newEnd ||
        oldStart + oldCount > oldLines.length || newStart + newCount > newLines.length || !oldCount && !newCount) {
      throw new Error("Git diff ranges do not match the file contents.");
    }
    oldEnd = oldStart + oldCount; newEnd = newStart + newCount;
  }
  if (oldLines.length - oldEnd !== newLines.length - newEnd) throw new Error("Git diff ranges do not match the file contents.");
  function projection() { return {text: "", rows: [], positions: {old: new Map(), new: new Map()}, offset: 0}; }
  const unified = projection(), old = projection(), next = projection();
  function push(target, oldLine, newLine, kind, changes = []) {
    const source = newLine ?? oldLine;
    const row = {text: source?.text ?? "", from: target.offset, oldLine: oldLine?.number, newLine: newLine?.number,
      oldFrom: oldLine?.from, newFrom: newLine?.from, kind, changes: []};
    if (source) for (const [from, to] of changes) {
      const lo = Math.max(from, source.from), hi = Math.min(to, source.from + source.text.length);
      if (hi > lo) row.changes.push([row.from + lo - source.from, row.from + hi - source.from]);
    }
    if (oldLine) target.positions.old.set(oldLine.number, row.from);
    if (newLine) target.positions.new.set(newLine.number, row.from);
    target.rows.push(row);
    target.offset += row.text.length + 1;
  }
  function context(a, b) {
    if (a.text !== b.text) throw new Error("Git diff context does not match the file contents.");
    if (split) { push(old, a, null, "context"); push(next, null, b, "context"); }
    else push(unified, a, b, "context");
  }
  let a = 0, b = 0;
  for (const change of diff.changes) {
    while (a < change.oldStart) context(oldLines[a++], newLines[b++]);
    const removed = oldLines.slice(a, a + change.oldLines), added = newLines.slice(b, b + change.newLines);
    const oldMarks = [], newMarks = [];
    if (removed.length && added.length) {
      const oldText = removed.map(line => line.text).join("\n"), newText = added.map(line => line.text).join("\n");
      for (const mark of characterDiff(oldText, newText, DIFF_CONFIG)) {
        oldMarks.push([removed[0].from + mark.fromA, removed[0].from + mark.toA]);
        newMarks.push([added[0].from + mark.fromB, added[0].from + mark.toB]);
      }
    }
    if (split) {
      for (let i = 0; i < Math.max(removed.length, added.length); i++) {
        push(old, removed[i], null, removed[i] ? "deletion" : "empty", oldMarks);
        push(next, null, added[i], added[i] ? "addition" : "empty", newMarks);
      }
    } else {
      for (const line of removed) push(unified, line, null, "deletion", oldMarks);
      for (const line of added) push(unified, null, line, "addition", newMarks);
    }
    a += change.oldLines; b += change.newLines;
  }
  while (a < oldLines.length) context(oldLines[a++], newLines[b++]);
  for (const target of [unified, old, next]) target.text = target.rows.map(row => row.text).join("\n");
  return split ? {old, new: next} : unified;
}
export const unifiedProjection = (before, after, diff) => reviewProjection(before, after, diff);
export const splitProjection = (before, after, diff) => reviewProjection(before, after, diff, true);

export function contextRegions(rows, margin = 3, minimum = 8) {
  const regions = [];
  for (let start = 0; start < rows.length;) {
    if (rows[start].kind !== "context") { start++; continue; }
    let end = start + 1;
    while (end < rows.length && rows[end].kind === "context") end++;
    const first = start + (start ? margin : 0), last = end - (end < rows.length ? margin : 0);
    if (last - first >= minimum) {
      const from = rows[first].from;
      const to = last < rows.length ? rows[last].from : rows[last - 1].from + rows[last - 1].text.length;
      if (to > from) regions.push({from, to, lines: last - first, first, last});
    }
    start = end;
  }
  return regions;
}

// Worker tokens are sorted UTF-16 ranges. This projection deliberately reads
// complete-file tokens so a deleted fragment retains its original syntax state.
export function projectTokens(projection, oldTokens, newTokens) {
  const output = [], cursor = {old: 0, new: 0};
  for (const row of projection.rows) {
    if (row.oldLine == null && row.newLine == null) continue;
    const side = row.newLine == null ? "old" : "new";
    const tokens = side === "old" ? oldTokens : newTokens;
    const sourceFrom = side === "old" ? row.oldFrom : row.newFrom;
    const sourceTo = sourceFrom + row.text.length;
    let i = cursor[side];
    while (i < tokens.length && tokens[i][1] <= sourceFrom) i++;
    for (; i < tokens.length && tokens[i][0] < sourceTo; i++) {
      const [from, to, style] = tokens[i];
      const lo = Math.max(from, sourceFrom), hi = Math.min(to, sourceTo);
      if (hi > lo) output.push([row.from + lo - sourceFrom, row.from + hi - sourceFrom, style]);
      if (to > sourceTo) break;
    }
    cursor[side] = i;
  }
  return output;
}
