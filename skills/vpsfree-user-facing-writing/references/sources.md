# Sources and local policy

The workspace keeps lean, language-specific runtime skills. Upstream repository
metadata, CI, issue templates, plugin manifests, and READMEs are intentionally
not copied into the installed skill directories.

## English

- Local skill: `skills/humanizer-en` (`$humanizer-en`)
- Repository: `https://github.com/blader/humanizer`
- Revision: `523374dee72d67c7b2b5f858ea0094ffda49c3ac` (`v2.9.1`)
- Upstream `SKILL.md` SHA-256:
  `70938f3cce25970e1ded5fdd194b755c03f1e4e4fa76820958ce78f86b677b1a`
- Upstream and local `LICENSE` SHA-256:
  `4ac4810254ab36d45419141aeb8e69bf50652cfafe5b2dab947d06d44e5cbf96`
- Local `SKILL.md` SHA-256:
  `81ec80d551d2865e856204b8ce6beb43d3e6f71962328defbcc6fb30ea0a69a4`
- License: MIT

Local changes are limited to the `humanizer-en` name, an explicit English
trigger, portable frontmatter, and OpenAI UI metadata. The editorial prompt
otherwise matches upstream v2.9.1.

## Czech

- Local skill: `skills/humanizer-cs` (`$humanizer-cs`)
- Repository: `https://github.com/katerina-svi/katerinas-humanizer-cz`
- Revision: `bda1f7ae7129142c476187966cfb59b2a32c0013`
- Upstream `SKILL.md` SHA-256:
  `656f0fb59124eb85f0ce620019a5852a46d3b1d44fd86682c478f3a868bb4ada`
- Upstream `LICENSE` SHA-256:
  `8dbb39f6f49c81b63d96a091d3e1fdcc5f6bad6a18fd1b898849fa54c2d76cc7`
- Local `LICENSE` SHA-256:
  `6baf827e0d512db5708ebf71b2cff2e71e24be1fbdd4361547feec5e6fa99c69`
- Local `SKILL.md` SHA-256:
  `d6a81649c5deb4b4685a8c5fb81169b73907199a0dd4c553976bd8060895a711`
- License: MIT

Local packaging changes use the ISO 639-1 `cs` language code, portable
frontmatter, and OpenAI UI metadata. The local license retains Siqi Chen's MIT
notice and identifies the Czech adaptation. The editorial prompt preserves
attributed claims unless the user authorizes a content change.

## Updating the pinned sources

Fetch a proposed upstream revision into a temporary directory and review it
before copying only `SKILL.md` and `LICENSE` into the locale skill. Reapply the
documented local changes, run the workspace security and behavior checks, then
record the new revision and both upstream and local hashes here. Never update
from a moving branch without pinning the reviewed commit.

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
