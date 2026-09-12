import {Text} from "@codemirror/state";
import {Chunk} from "@codemirror/merge";

export const DIFF_CONFIG = {scanLimit: 500, timeout: 100};

export function normalizeSource(text) {
  return String(text ?? "").replace(/\r\n?/g, "\n");
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

// Project the native CodeMirror diff into ordinary read-only editor lines.
// Source offsets remain separate from display offsets, including for renames,
// removals, missing final newlines and empty files.
export function unifiedProjection(before, after) {
  const oldLines = sourceLines(before), newLines = sourceLines(after);
  const chunks = Chunk.build(Text.of(before.split("\n")), Text.of(after.split("\n")), DIFF_CONFIG);
  const rows = [], positions = {old: new Map(), new: new Map()};
  let a = 0, b = 0, offset = 0;
  function push(oldLine, newLine, kind, chunk) {
    const source = newLine ?? oldLine;
    const row = {text: source.text, from: offset, oldLine: oldLine?.number, newLine: newLine?.number,
      oldFrom: oldLine?.from, newFrom: newLine?.from, kind, changes: []};
    if (chunk && kind !== "context") {
      const old = kind === "deletion", start = old ? chunk.fromA : chunk.fromB;
      for (const change of chunk.changes) {
        const from = Math.max(source.from, start + (old ? change.fromA : change.fromB));
        const to = Math.min(source.from + source.text.length, start + (old ? change.toA : change.toB));
        if (to > from) row.changes.push([offset + from - source.from, offset + to - source.from]);
      }
    }
    if (oldLine) positions.old.set(oldLine.number, offset);
    if (newLine) positions.new.set(newLine.number, offset);
    rows.push(row);
    offset += source.text.length + 1;
  }
  for (const chunk of chunks) {
    while (a < oldLines.length && b < newLines.length && oldLines[a].from < chunk.fromA && newLines[b].from < chunk.fromB) {
      push(oldLines[a++], newLines[b++], "context");
    }
    while (a < oldLines.length && oldLines[a].from < chunk.toA) push(oldLines[a++], null, "deletion", chunk);
    while (b < newLines.length && newLines[b].from < chunk.toB) push(null, newLines[b++], "addition", chunk);
  }
  while (a < oldLines.length && b < newLines.length) push(oldLines[a++], newLines[b++], "context");
  while (a < oldLines.length) push(oldLines[a++], null, "deletion");
  while (b < newLines.length) push(null, newLines[b++], "addition");
  return {text: rows.map(row => row.text).join("\n"), rows, positions};
}

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
      if (to > from) regions.push({from, to, lines: last - first});
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
