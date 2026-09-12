import {Compartment, EditorState, StateEffect, StateField} from "@codemirror/state";
import {Decoration, EditorView, GutterMarker, WidgetType, gutter, highlightSpecialChars} from "@codemirror/view";
import {MergeView, getChunks, uncollapseUnchanged} from "@codemirror/merge";
import {normalizeSource, sourceLines, languageForPath, unifiedProjection, contextRegions, projectTokens, DIFF_CONFIG} from "./editor-model.js";
import {highlightSource, preloadReviewEditor} from "./highlight-client.js";

export {preloadReviewEditor};

const expandContext = StateEffect.define();
const syntaxThemes = new Map();
const baseTheme = EditorView.theme({
  "&": {fontSize: "13px", backgroundColor: "#0d1117", color: "#e6edf3"},
  ".cm-content": {fontFamily: "ui-monospace, SFMono-Regular, Consolas, monospace", lineHeight: "1.5"},
  ".cm-gutters": {backgroundColor: "#161b22", color: "#8b949e", borderRight: "1px solid #30363d"},
  ".review-line-numbers .cm-gutterElement": {minWidth: "3ch", padding: "0 6px", textAlign: "right"},
  ".review-line-numbers a": {color: "inherit", textDecoration: "none", display: "block"},
  ".review-line-numbers a:hover, .review-line-numbers a:focus-visible": {color: "#e6edf3", textDecoration: "underline"},
  ".review-line-numbers a[aria-current=line]": {color: "#f2cc60", fontWeight: "bold"},
  ".review-added-line": {backgroundColor: "#143323"},
  ".review-deleted-line": {backgroundColor: "#3b2028"},
  ".review-added-text": {backgroundColor: "#1f5c36"},
  ".review-deleted-text": {backgroundColor: "#76323c"},
  ".review-linked-line": {outline: "1px solid #d29922", outlineOffset: "-1px", backgroundColor: "#604b1633"},
  ".review-context-button": {display: "block", width: "100%", textAlign: "left", cursor: "pointer", border: "0", padding: "5px 12px", color: "#79c0ff", backgroundColor: "#182c40", font: "inherit"},
}, {dark: true});

class LineLink extends GutterMarker {
  constructor(side, line, url, onSelect, selected = false) {
    super();
    Object.assign(this, {side, line, url, onSelect, selected});
  }
  eq(other) { return this.side === other.side && this.line === other.line && this.url === other.url && this.selected === other.selected; }
  toDOM() {
    const link = document.createElement("a");
    link.textContent = String(this.line);
    link.href = this.url;
    link.dataset.side = this.side;
    link.dataset.line = String(this.line);
    link.setAttribute("aria-label", `${this.side === "old" ? "Before" : "After"}, line ${this.line}`);
    if (this.selected) link.setAttribute("aria-current", "line");
    link.addEventListener("click", event => {
      if (!this.onSelect || event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return;
      event.preventDefault();
      this.onSelect(this.side, this.line);
    });
    return link;
  }
}
class ContextButton extends WidgetType {
  constructor(region) { super(); this.region = region; }
  eq(other) { return this.region.from === other.region.from && this.region.to === other.region.to; }
  toDOM(view) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "review-context-button";
    button.textContent = `${this.region.lines} unchanged lines`;
    button.addEventListener("click", () => view.dispatch({effects: expandContext.of(this.region.from)}));
    return button;
  }
  ignoreEvent() { return true; }
  get estimatedHeight() { return 30; }
}
function collapsedContext(regions) {
  return StateField.define({
    create: () => regions,
    update: (value, transaction) => {
      for (const effect of transaction.effects) if (effect.is(expandContext)) {
        value = value.filter(region => effect.value < region.from || effect.value >= region.to);
      }
      return value;
    },
    provide: field => EditorView.decorations.from(field, value => Decoration.set(value.map(region =>
      Decoration.replace({block: true, widget: new ContextButton(region)}).range(region.from, region.to)))),
  });
}
function syntaxTheme(styles) {
  const key = JSON.stringify(styles);
  if (syntaxThemes.has(key)) return syntaxThemes.get(key);
  const rules = {};
  for (const [index, style] of styles.entries()) {
    // Tokens carry palette indices, never source-derived CSS or markup.
    if (!/^#[0-9a-f]{6}(?:[0-9a-f]{2})?$/i.test(style.color)) continue;
    rules[`.review-token-${index}`] = {color: style.color,
      ...(style.flags & 1 ? {fontStyle: "italic"} : {}),
      ...(style.flags & 2 ? {fontWeight: "bold"} : {}),
      ...(style.flags & 4 ? {textDecoration: "underline"} : {})};
  }
  const extension = EditorView.theme(rules, {dark: true});
  // The worker serves one fixed theme. Bound defensive memoization as well.
  if (syntaxThemes.size >= 4) syntaxThemes.delete(syntaxThemes.keys().next().value);
  syntaxThemes.set(key, extension);
  return extension;
}
function syntaxDecorations(tokens, length) {
  return EditorView.decorations.of(Decoration.set(tokens.filter(([from, to]) => from >= 0 && to > from && to <= length)
    .map(([from, to, style]) => Decoration.mark({class: `review-token-${style}`}).range(from, to)), true));
}
function changeDecorations(rows) {
  const marks = [];
  for (const row of rows) if (row.kind !== "context") {
    const added = row.kind === "addition";
    marks.push(Decoration.line({class: added ? "review-added-line" : "review-deleted-line"}).range(row.from));
    for (const [from, to] of row.changes) marks.push(Decoration.mark({class: added ? "review-added-text" : "review-deleted-text"}).range(from, to));
  }
  return EditorView.decorations.of(Decoration.set(marks, true));
}
function uncollapseAt(view, position) {
  const effects = [];
  for (const provider of view.state.facet(EditorView.decorations)) {
    const decorations = typeof provider === "function" ? provider(view) : provider;
    decorations.between(0, position + 1, (from, to, decoration) => {
      if (from <= position && to > position && decoration.spec.block && decoration.spec.widget) effects.push(uncollapseUnchanged.of(from));
    });
  }
  if (effects.length) view.dispatch({effects});
}
function siblingPosition(position, chunks, old) {
  let from = 0, otherFrom = 0;
  for (const chunk of chunks) {
    const start = old ? chunk.fromA : chunk.fromB, end = old ? chunk.toA : chunk.toB;
    if (position < start) break;
    if (position < end) return old ? chunk.fromB : chunk.fromA;
    from = end;
    otherFrom = old ? chunk.toB : chunk.toA;
  }
  return otherFrom + position - from;
}
const nextFrame = () => new Promise(resolve => requestAnimationFrame(resolve));

// A synchronous mount keeps navigation responsive while a same-origin worker
// highlights complete immutable sources. No editor mutates source content.
export function createReviewEditor({parent, before, after, oldPath = "", newPath = "", mode = "split", version = "new", nonce = "", lineURL, onLineSelect}) {
  before = normalizeSource(before);
  after = normalizeSource(after);
  const abort = new AbortController();
  const host = document.createElement("div");
  host.className = "review-code-view";
  const status = document.createElement("div");
  status.className = "review-syntax-status";
  status.setAttribute("role", "status");
  status.textContent = "Highlighting…";
  parent.append(host, status);
  const oldLines = sourceLines(before), newLines = sourceLines(after);
  const syntax = {old: new Compartment(), new: new Compartment(), unified: new Compartment()};
  const selection = {old: new Compartment(), new: new Compartment(), unified: new Compartment()};
  let destroyed = false, editor, projection;
  let selected;
  function lineGutter(side, resolveLine) {
    return gutter({class: `review-line-numbers review-${side}-numbers`, renderEmptyElements: true,
      lineMarker(view, line) {
        const number = resolveLine(view, line.from);
        if (!number) return null;
        const url = lineURL?.(side, number) ?? `#${side}-L${number}`;
        return new LineLink(side, number, url, onLineSelect, selected?.side === side && selected.line === number);
      },
      lineMarkerChange: update => update.transactions.some(transaction => transaction.effects.length > 0),
    });
  }
  function extensions(key, label) {
    return [EditorState.readOnly.of(true), EditorView.editable.of(false), EditorView.cspNonce.of(nonce),
      highlightSpecialChars(), baseTheme, syntax[key].of([]), selection[key].of([]),
      EditorView.contentAttributes.of({"aria-label": label, tabindex: "0"})];
  }
  function sourceGutter(side, lines) {
    return lineGutter(side, (view, from) => {
      const number = view.state.doc.lineAt(from).number;
      return number <= lines.length ? number : null;
    });
  }
  if (mode === "file") {
    const old = version === "old", side = old ? "old" : "new";
    editor = new EditorView({parent: host, doc: old ? before : after,
      extensions: [...extensions(side, `Read-only file, ${old ? "before" : "after"} changes`), sourceGutter(side, old ? oldLines : newLines)]});
  } else if (mode === "unified") {
    projection = unifiedProjection(before, after);
    editor = new EditorView({parent: host, doc: projection.text,
      extensions: [...extensions("unified", "Read-only unified file comparison"),
        ...["old", "new"].map(side => lineGutter(side, (view, from) => projection.rows[view.state.doc.lineAt(from).number - 1]?.[side === "old" ? "oldLine" : "newLine"])),
        changeDecorations(projection.rows), collapsedContext(contextRegions(projection.rows))]});
  } else {
    editor = new MergeView({parent: host, highlightChanges: true, gutter: true,
      collapseUnchanged: {margin: 3, minSize: 8}, diffConfig: DIFF_CONFIG,
      a: {doc: before, extensions: [...extensions("old", "Read-only file before changes"), sourceGutter("old", oldLines)]},
      b: {doc: after, extensions: [...extensions("new", "Read-only file after changes"), sourceGutter("new", newLines)]}});
  }
  function apply(view, key, result, tokens = result.tokens) {
    view.dispatch({effects: syntax[key].reconfigure([syntaxTheme(result.styles), syntaxDecorations(tokens, view.state.doc.length)])});
  }
  const oldLanguage = languageForPath(oldPath || newPath, before);
  const newLanguage = languageForPath(newPath || oldPath, after);
  const needed = mode === "file" ? [version] : ["old", "new"];
  const ready = Promise.all(needed.map(side => highlightSource(side === "old" ? before : after, side === "old" ? oldLanguage : newLanguage, abort.signal)
    .then(result => [side, result]))).then(entries => {
    if (destroyed) return;
    const result = Object.fromEntries(entries);
    if (mode === "file") apply(editor, version, result[version]);
    else if (mode === "unified") {
      // Both results use the worker's fixed, stable palette, even across languages.
      apply(editor, "unified", result.new.styles.length ? result.new : result.old,
        projectTokens(projection, result.old.tokens, result.new.tokens));
    } else {
      apply(editor.a, "old", result.old);
      apply(editor.b, "new", result.new);
    }
    status.remove();
  }).catch(() => {
    if (!destroyed) status.textContent = "Syntax highlighting is unavailable.";
  });

  async function revealLine(side, line) {
    if (destroyed || !["old", "new"].includes(side) || !Number.isSafeInteger(line) || line < 1) return false;
    const source = side === "old" ? oldLines : newLines;
    if (line > source.length || (mode === "file" && version !== side)) return false;
    let view, key, position;
    if (mode === "unified") {
      view = editor; key = "unified"; position = projection.positions[side].get(line);
      if (position == null) return false;
      view.dispatch({effects: expandContext.of(position)});
    } else {
      key = side; view = mode === "file" ? editor : side === "old" ? editor.a : editor.b;
      position = source[line - 1].from;
      if (mode !== "file") {
        uncollapseAt(view, position);
        const sibling = side === "old" ? editor.b : editor.a;
        const otherPosition = siblingPosition(position, getChunks(view.state)?.chunks ?? [], side === "old");
        uncollapseAt(sibling, Math.min(otherPosition, sibling.state.doc.length));
      }
    }
    selected = {side, line};
    if (mode !== "file" && mode !== "unified") {
      const otherSide = side === "old" ? "new" : "old";
      const sibling = side === "old" ? editor.b : editor.a;
      sibling.dispatch({effects: selection[otherSide].reconfigure([])});
    }
    view.dispatch({effects: [selection[key].reconfigure(EditorView.decorations.of(Decoration.set([
      Decoration.line({class: "review-linked-line"}).range(position)]))), EditorView.scrollIntoView(position, {y: "center"})]});
    await nextFrame();
    if (destroyed) return false;
    // A second measure handles a newly mounted or expanded offscreen editor.
    view.dispatch({effects: EditorView.scrollIntoView(position, {y: "center"})});
    return true;
  }
  return {ready, revealLine, destroy() {
    if (destroyed) return;
    destroyed = true;
    abort.abort();
    editor.destroy();
    host.remove();
    status.remove();
  }};
}
