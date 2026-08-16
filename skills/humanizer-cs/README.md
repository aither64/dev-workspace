# Katerinas Humanizer CZ

Agent skill, který odstraňuje z **českých textů** příznaky AI generovaného psaní a převádí je do přirozené, pravopisně a typograficky správné češtiny podle [Internetové jazykové příručky ÚJČ](https://prirucka.ujc.cas.cz/).

Je to česká adaptace skillu [blader/humanizer](https://github.com/blader/humanizer), ale ne prostý překlad — vzorce jsou přepracované pro češtinu a několik pravidel funguje **obráceně** než v angličtině (uvozovky, pomlčky, velká písmena v nadpisech). Celý skill je čistý Markdown (`SKILL.md`), takže běží v jakémkoli prostředí, které podporuje skill-style instrukce.

## Čím se liší od originálu

**1. Striktní režim.** Skill nikdy nemění význam, nepřidává ani neodebírá obsah a nevymýšlí fakta či zdroje. Jen stylisticky přeformulovává. Před finálním zněním kontroluje, že každá informace originálu je ve výstupu dohledatelná a žádná nepřibyla.

**2. Ptá se.** U sporných míst (věta bez věcného obsahu, dvojznačnost, možná citace, nejednotné tykání/vykání, oborový žargon vs. AI slovník) skill nerozhoduje sám — položí uživateli konkrétní otázky s návrhem řešení a čeká na odpověď.

**3. Česká typografie s obrácenou logikou.** Anglický humanizer učí: oblé uvozovky jsou tell, pomlčky vymýtit. V češtině je to jinak:
- správné české uvozovky jsou „dole a nahoře" — tell jsou naopak anglické oblé " "
- pomlčka (–) je legitimní česká interpunkce, ale vzácná: skill hlídá „lepidlové" pomlčky mezi větami (kalk anglického em dashe) a drží tvrdý limit nejvýše jedné pomlčky s mezerami na text; rozsahy (15.–18. 10.) zůstávají
- Title Case v nadpisech čeština nemá vůbec — stoprocentní tell

**4. Nové, čistě české vzorce.** Slovosled a aktuální členění větné (téma–réma), kalky ukazovacích a přivlastňovacích zájmen („tento", „váš byznys, vaše cíle"), svůj vs. jeho/její, přemíra knižnosti („lze", „je nutno" v konverzačním textu), opisné pasivum a nominalizace, mezery u procent a dat podle IJP.

## Instalace

### Claude.ai (web a aplikace)

Stáhni [`SKILL.md`](SKILL.md), v Claude.ai otevři **Settings → Capabilities → Skills** a nahraj ho jako nový skill. Hotovo.

### Claude Code — plugin

```
/plugin marketplace add katerina-svi/katerinas-humanizer-cz
/plugin install humanizer-cz@katerinas-humanizer-cz
```

### Skills CLI (cross-agent)

```
npx skills add katerina-svi/katerinas-humanizer-cz
```

### Ručně

Zkopíruj `SKILL.md` do složky, kde tvůj agent očekává skilly:

```
git clone https://github.com/katerina-svi/katerinas-humanizer-cz.git /cesta/ke/skillum/humanizer-cz
```

## Použití

```
/humanizer

[vlož text ke zlidštění]
```

Nebo prostě: „Zlidšti mi tenhle text: …" / „Přepiš to, ať to nezní jako ChatGPT."

Skill nejdřív text projde, vypíše nalezené vzorce, **položí otázky ke sporným místům** a teprve po odpovědích dodá finální znění s přehledem změn.

### Kalibrace hlasu

Když přiložíš 2–3 odstavce vlastního psaní, skill analyzuje tvůj rytmus vět, slovník a návyky a přepíše text ve tvém hlase místo generické „čisté" češtiny:

```
/humanizer

Vzorek mého psaní pro sladění hlasu:
[vlož vzorek]

Zlidšti tento text:
[vlož text]
```

## Co skill detekuje (33 vzorců ve 4 blocích)

**A. Obsahové:** inflace významu („zásadní milník"), vlečné „což podtrhuje"-věty, reklamní jazyk, vágní autority („odborníci se shodují"), formulaické sekce „výzvy a budoucnost", pravidlo tří, cyklování synonym, falešné rozsahy.

**B. Jazykové:** český AI slovník (klíčový, komplexní, nicméně, v dnešní digitální době…), „představuje/disponuje" místo je/má, negativní paralelismy („nejde jen o…"), trpný rod a nominalizace, anglický slovosled vs. téma–réma, kalky zájmen, svůj vs. jeho, přemíra knižnosti, vata, hedging, generické závěry (s výjimkou pro CTA v marketingových textech).

**C. Typografie a pravopis dle IJP:** české uvozovky „ ", pomlčky (počeštit, ne vymýtit — tvrdý limit), Title Case, mezery/procenta/data/tisíce, tučné písmo, emoji, fragmentované nadpisy.

**D. Komunikační:** chatbot artefakty („Doufám, že to pomůže!"), servilní tón, disclaimery a spekulativní vycpávky, ohlašování („Pojďme se podívat…"), staccato drama, aforistické vzorce, falešně důvěrné otvíráky.

Součástí je sekce falešných poplachů (kdy NEzasahovat — mj. nářeční a regionální prvky se neopravují) a známek lidského psaní, které se mají zachovat.

## Historie verzí

- **1.0.0** — první veřejná verze. 33 vzorců adaptovaných pro češtinu, striktní režim (žádná změna významu, žádný nový obsah), povinné dotazy u sporných míst, tvrdý limit pomlček, výjimka pro CTA v marketingových textech, reference IJP.

## Autorka

[Kateřina Švidrnochová](https://svidrnochova.cz) — konzultantka, learning designerka a lektorka AI vzdělávání. Vzniklo mj. pro komunitu [Fajne prompty](https://svidrnochova.cz) v Ostravě.

## Poděkování a zdroje

- [blader/humanizer](https://github.com/blader/humanizer) (MIT) — původní anglický skill, ze kterého tato adaptace vychází
- [Wikipedia: Signs of AI writing](https://en.wikipedia.org/wiki/Wikipedia:Signs_of_AI_writing) — původní katalog vzorců (WikiProject AI Cleanup)
- [Internetová jazyková příručka ÚJČ](https://prirucka.ujc.cas.cz/) — závazná reference pro český pravopis a typografii

## Licence

MIT
