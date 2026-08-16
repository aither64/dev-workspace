---
name: vpsfree-user-facing-writing
description: Edit or write natural English and Czech user-facing text for vpsFree.cz. Use for knowledge-base articles, vpsAdmin documentation and interface copy, user-visible errors and help, mail templates, website copy, and release or operational messages intended for members. Preserve technical meaning and machine-significant markup while removing formulaic AI writing.
---

# vpsFree user-facing writing

Apply the matching pinned Humanizer before finalizing user-facing prose:

- For English, read `../humanizer/SKILL.md` completely.
- For Czech, read `../humanizer-cz/SKILL.md` completely.
- For a bilingual document, edit each language independently. Do not mechanically
  translate the humanized English version into Czech.

Treat the upstream skill as editorial guidance. The rules below override it for
vpsFree technical documentation.

## Workflow

1. Establish the supported behavior and factual claims before editing style.
2. Inventory every fact, command, value, link, identifier, and warning that must
   survive.
3. Rewrite the prose in embedded mode. Keep the draft, self-audit, and comparison
   internal; return or commit only the final text.
4. Compare the result with the source for factual and structural drift.
5. Read the result as a member would, then remove remaining filler, repetition,
   vague transitions, and chatbot phrasing.

Do the main rewrite in the agent that owns the task context. Do not hand the
draft to a context-poor subagent for wholesale rewriting. A fresh agent may audit
the finished result for naturalness and fidelity.

## Voice

- Write technical and reference material in a neutral, plain voice. Do not add
  first person, opinions, jokes, marketing language, or decorative personality.
- Prefer direct sentences and concrete verbs. Remove meta-commentary that merely
  announces what the text will explain.
- Keep useful repetition when it prevents ambiguity. Do not vary established
  technical terms merely to avoid repeating them.
- Never invent a fact, version, default, source, date, number, limitation, or
  recommendation. Stop and verify a material ambiguity instead of smoothing it
  over.

## Czech profile

- Use standard Czech and address an individual member informally: `můžeš`,
  `potřebuješ`, `nainstaluj`, `použij`. Do not use formal `vy` or plural
  imperatives as a polite form.
- Avoid colloquial or obecná čeština unless the existing genre deliberately uses
  it. Do not make technical documentation conversational for its own sake.
- Prefer natural Czech word order, concise verbs, and `je` or `má` over formal
  nominal constructions.
- Preserve correct Czech typography. Remove em dashes. Keep an en dash when it
  correctly expresses a range or relation; do not replace it with a spaced
  hyphen merely to follow the English Humanizer.
- Allow sentence and paragraph reordering when it improves the explanation.
  Preserve the facts and required task sequence, not the original paragraph
  boundaries.
- Do not pause for questions when the register, audience, and facts are already
  established. Ask only about a genuine ambiguity that would change the result.

## Protected technical content

For DokuWiki and other technical sources, preserve unless the task explicitly
requires a change:

- page IDs, reciprocal-language markers, anchors, link and media targets;
- `<kb-managed>`, `<vpsadmin-nav>`, and other semantic tags and their IDs;
- code, file, nowiki, HTML, frontmatter, gettext, and placeholder syntax;
- commands, options, executable lines, configuration keys and values;
- IP addresses, ports, paths, hashes, versions, names, and URLs;
- table structure and list syntax that affects rendering.

Visible labels and explanatory prose may change. In localized code samples,
translate human-readable comments but preserve shebangs, tool directives,
commands, identifiers, and configuration values.

For a managed KB page, keep executable samples synchronized with the article
contract and update prose fingerprints only after the final wording is stable.

See `references/sources.md` for provenance, local deviations, and the pinned
upstream revisions.
