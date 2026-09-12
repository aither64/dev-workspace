import {EditorState} from "@codemirror/state";
import {EditorView, lineNumbers, highlightSpecialChars} from "@codemirror/view";
import {MergeView, unifiedMergeView} from "@codemirror/merge";

// Editors are mounted near the viewport. Git metadata stays outside them so
// binary files, modes, object kinds and final newlines cannot disappear in a diff.
export function createReviewEditor({parent, before, after, mode, nonce}) {
  const extensions = [
    EditorState.readOnly.of(true), EditorView.editable.of(false),
    EditorView.cspNonce.of(nonce), lineNumbers(), highlightSpecialChars(),
    EditorView.contentAttributes.of({"aria-label": "Read-only file comparison", tabindex: "0"}),
    EditorView.theme({"&": {fontSize: "13px"}, ".cm-content": {fontFamily: "ui-monospace, SFMono-Regular, Consolas, monospace"}}, {dark: true}),
  ];
  const options = {highlightChanges: true, gutter: true, collapseUnchanged: {margin: 3, minSize: 8}, diffConfig: {scanLimit: 500, timeout: 100}};
  let editor;
  if (mode === "unified") {
    editor = new EditorView({parent, doc: after, extensions: [...extensions, unifiedMergeView({...options, original: before, mergeControls: false, syntaxHighlightDeletions: false})]});
  } else {
    editor = new MergeView({...options, parent, a: {doc: before, extensions}, b: {doc: after, extensions}});
  }
  return {destroy: () => editor.destroy()};
}
