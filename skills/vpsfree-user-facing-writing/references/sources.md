# Sources and local policy

## English

- Repository: `https://github.com/blader/humanizer`
- Revision: `523374dee72d67c7b2b5f858ea0094ffda49c3ac` (`v2.9.1`)
- `SKILL.md` SHA-256:
  `70938f3cce25970e1ded5fdd194b755c03f1e4e4fa76820958ce78f86b677b1a`
- `LICENSE` SHA-256:
  `4ac4810254ab36d45419141aeb8e69bf50652cfafe5b2dab947d06d44e5cbf96`
- License: MIT

## Czech

- Repository: `https://github.com/katerina-svi/katerinas-humanizer-cz`
- Revision: `bda1f7ae7129142c476187966cfb59b2a32c0013`
- `SKILL.md` SHA-256:
  `656f0fb59124eb85f0ce620019a5852a46d3b1d44fd86682c478f3a868bb4ada`
- `LICENSE` SHA-256:
  `8dbb39f6f49c81b63d96a091d3e1fdcc5f6bad6a18fd1b898849fa54c2d76cc7`
- License: MIT

The canonical workspace copies are `skills/humanizer` and
`skills/humanizer-cz`. Update them with the workspace's installed
`skill-installer` helper, pinning a reviewed commit instead of a moving branch.
After an update, record the new revision and checksums here and review the
vpsFree overrides for conflicts.

## vpsFree deviations

The wrapper deliberately overrides these generic behaviors:

- Technical and reference material remains neutral; no personality is injected.
- Czech documentation uses standard Czech and established informal singular
  address, not automatically added colloquial language.
- Correct Czech en dashes remain in ranges and relations.
- Paragraphs may be reorganized when clarity improves, provided facts and task
  order remain intact.
- Known register and product decisions do not trigger interactive questions.
- DokuWiki markup and tested technical material receive explicit protection.
