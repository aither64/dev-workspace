# AGENTS.md

Pokyny pro AI agenty (Claude Code, Codex apod.) pracující v tomto repozitáři.

## Co tento repozitář je

Přenositelný agent skill implementovaný čistě v Markdownu. Runtime artefakt je `SKILL.md`: YAML frontmatter (metadata + povolené nástroje) následovaný redakčním promptem. Žádný build, žádný kód.

## Klíčové soubory

- `SKILL.md` — skill samotný, zdroj pravdy. 33 číslovaných vzorců s příklady před/po, striktní pravidla (⛔ sekce) a pravidla dotazování (❓ sekce).
- `README.md` — pro lidi: instalace, použití, přehled vzorců, historie verzí.
- `.claude-plugin/` — volitelné manifesty pro Claude Code plugin a single-repo marketplace.

## Kontrakt údržby

- `SKILL.md` a `README.md` musí zůstat v synchronu: při změně vzorců aktualizuj přehled v README i historii verzí.
- Verze se udržuje v `.claude-plugin/plugin.json` a v README (frontmatter `SKILL.md` pole `version` záměrně NEMÁ — validátor skillů v Claude.ai ho nepovoluje).
- ⛔ STRIKTNÍ PRAVIDLA a ❓ sekce dotazování jsou jádro odlišnosti tohoto skillu — žádná úprava je nesmí oslabit ani obejít.
- Typografická pravidla se řídí Internetovou jazykovou příručkou ÚJČ; při úpravách zachovej obrácenou logiku vůči anglickému originálu (uvozovky „ ", limit pomlček, zákaz Title Case).
- Příklady před/po musí samy splňovat pravidla skillu (žádné em dashe, anglické uvozovky ani lepidlové pomlčky v „Po" textech).
