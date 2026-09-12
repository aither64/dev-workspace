import {createHighlighterCore} from "shiki/core";
import {createJavaScriptRegexEngine} from "shiki/engine/javascript";
import theme from "shiki/dist/themes/github-dark-default.mjs";
import nix from "shiki/dist/langs/nix.mjs";
import ruby from "shiki/dist/langs/ruby.mjs";
import go from "shiki/dist/langs/go.mjs";
import javascript from "shiki/dist/langs/javascript.mjs";
import jsx from "shiki/dist/langs/jsx.mjs";
import typescript from "shiki/dist/langs/typescript.mjs";
import tsx from "shiki/dist/langs/tsx.mjs";
import shellscript from "shiki/dist/langs/shellscript.mjs";
import yaml from "shiki/dist/langs/yaml.mjs";
import json from "shiki/dist/langs/json.mjs";
import jsonc from "shiki/dist/langs/jsonc.mjs";
import hcl from "shiki/dist/langs/hcl.mjs";
import python from "shiki/dist/langs/python.mjs";
import php from "shiki/dist/langs/php.mjs";
import html from "shiki/dist/langs/html.mjs";
import xml from "shiki/dist/langs/xml.mjs";
import css from "shiki/dist/langs/css.mjs";
import scss from "shiki/dist/langs/scss.mjs";
import markdown from "shiki/dist/langs/markdown.mjs";
import toml from "shiki/dist/langs/toml.mjs";
import ini from "shiki/dist/langs/ini.mjs";
import sql from "shiki/dist/langs/sql.mjs";
import rust from "shiki/dist/langs/rust.mjs";
import c from "shiki/dist/langs/c.mjs";
import cpp from "shiki/dist/langs/cpp.mjs";
import makefile from "shiki/dist/langs/makefile.mjs";
import dockerfile from "shiki/dist/langs/dockerfile.mjs";
import diff from "shiki/dist/langs/diff.mjs";

const grammars = [nix, ruby, go, javascript, jsx, typescript, tsx, shellscript, yaml, json, jsonc, hcl,
  python, php, html, xml, css, scss, markdown, toml, ini, sql, rust, c, cpp, makefile, dockerfile, diff];
let highlighter;

export function initializeHighlighter() {
  // Explicit imports keep the worker self-contained: no grammar requests,
  // Oniguruma/WASM download, dynamic evaluation or remote asset loading.
  highlighter ??= createHighlighterCore({themes: [theme], langs: grammars, engine: createJavaScriptRegexEngine()});
  return highlighter;
}

export async function tokenizeSource(text, language) {
  const lineCount = text.split("\n").length - (text.endsWith("\n") ? 1 : 0);
  if (new TextEncoder().encode(text).length > 512 * 1024 || lineCount > 12000) {
    throw new Error("Source exceeds the syntax highlighting limit");
  }
  const engine = await initializeHighlighter();
  if (!language || !engine.getLoadedLanguages().includes(language)) return {tokens: [], styles: []};
  const resolvedTheme = engine.getTheme(theme.name);
  const colors = [...new Set([resolvedTheme.fg, ...resolvedTheme.settings.map(rule => rule.settings.foreground)]
    .filter(color => typeof color === "string" && /^#[0-9a-f]{6}(?:[0-9a-f]{2})?$/i.test(color))
    .map(color => color.toLowerCase()))].sort();
  const styles = [], indices = new Map();
  for (const color of colors) for (let flags = 0; flags < 8; flags++) {
    indices.set(`${color}:${flags}`, styles.length);
    styles.push({color, flags});
  }
  const lines = engine.codeToTokensBase(text, {
    // Compilation counts against Shiki's soft timeout and can silently color
    // a whole line as its first token. The client terminates this worker after
    // eight seconds instead of accepting a partially tokenized document.
    lang: language, theme: theme.name, tokenizeMaxLineLength: 10000, tokenizeTimeLimit: 0,
  });
  const tokens = [];
  for (const line of lines) for (const token of line) {
    const style = indices.get(`${token.color?.toLowerCase()}:${Math.max(0, token.fontStyle ?? 0) & 7}`);
    if (style != null && token.content.length) tokens.push([token.offset, token.offset + token.content.length, style]);
  }
  return {tokens, styles};
}
