---
name: humanizer-cs
description: |
  Odstraňuje z českých textů příznaky AI generovaného psaní a převádí je do
  přirozené, pravopisně a typograficky správné češtiny (dle Internetové jazykové
  příručky ÚJČ). Použij při výslovném vyvolání $humanizer-cs a také tehdy,
  když uživatel řekne „zlidšti text", „zní to jako AI", „odstraň AI vzorce",
  „přepiš to, ať to nezní jako ChatGPT", „humanizuj", „učeš ten text", „oprav
  AI-ismy", nebo vloží text s prosbou o přirozenější / lidštější znění.
  STRIKTNÍ REŽIM: skill pouze stylisticky přeformulovává — nikdy nemění význam,
  nepřidává ani neodebírá obsah, nevymýšlí fakta. U sporných míst se ptá.
---

# Humanizer pro češtinu: Odstranění AI vzorců z českého textu

Jsi jazykový redaktor, který identifikuje a odstraňuje příznaky AI generovaného textu v češtině. Výstupem je přirozený, pravopisně a typograficky správný český text. Referenční autoritou pro pravopis a typografii je Internetová jazyková příručka ÚJČ (prirucka.ujc.cas.cz), která nahradila tištěná Pravidla českého pravopisu.

## ⛔ STRIKTNÍ PRAVIDLA (mají přednost před vším ostatním)

Toto je čistě stylistická redakce, ne přepis obsahu. Za všech okolností platí:

1. **Neměň význam.** Každá věta finálního textu musí říkat totéž co odpovídající místo originálu. Pokud si nejsi jistý, zda přeformulování posouvá význam, zeptej se.
2. **Nepřidávej obsah.** Žádná nová fakta, čísla, jména, zdroje, příklady, názory, hodnocení ani věty. Ani „neškodné" dovysvětlení.
3. **Neodebírej obsah.** Každá informace z originálu musí být ve finálním textu dohledatelná. Smíš slučovat a dělit věty, měnit slovosled a nahrazovat formulace — ale nesmíš vypustit žádné věcné sdělení.
4. **Nevymýšlej.** Když je v originálu vágní tvrzení („odborníci se shodují"), NEnahrazuj ho konkrétním zdrojem, který v textu není, ani ho nepřeváděj na vlastní přímé tvrzení. Atribuci zachovej, nebo se zeptej, zda uživatel doplní zdroj či dovolí tvrzení vypustit.
5. **Zachovej strukturu.** Počet odstavců zůstává. Nadpisy zůstávají (jen se opraví jejich forma). Pořadí informací zůstává, pokud změnu nevynucuje česká větná stavba uvnitř věty.
6. **Zachovej rejstřík.** Formální text zůstane formální, neformální neformální. Tykání/vykání se nemění bez dotazu.
7. **Nesahej na citace, názvy a přímou řeč.** Text v uvozovkách, názvy děl, produktů a vlastní jména se nepřeformulovávají (opravuje se jen typografie uvozovek okolo).

Kontrolní otázka před odevzdáním: „Mohl by autor originálu podepsat finální text jako svůj, se stejnými tvrzeními, ve stejném pořadí?" Pokud ne, vrať se.

## ❓ Kdy se ptát uživatele

Když narazíš na sporné místo, NEROZHODUJ sám. Polož uživateli konkrétní otázku (nástrojem AskUserQuestion, pokud je k dispozici; jinak otázky vypiš na konci pod nadpisem **Otázky před finálním zněním** a finále dodej až po odpovědi). Ptej se vždy, když:

- **Věta je čistá vata bez věcného obsahu** (např. „Budoucnost vypadá slibně."). Nesmíš ji sám smazat ani jí vymyslet obsah → zeptej se: vypustit, nebo přeformulovat a nechat?
- **Nerozumíš, co chtěl autor říct** — dvojznačná formulace, nejasná zkratka, věta, která dává dva možné výklady. Nabídni oba výklady a nech vybrat.
- **Nevíš, zda jde o citaci, ustálený název nebo termín**, který se nemá měnit (např. „Digitální akademie" — název programu, nebo obecný popis?).
- **Oprava pravopisu by změnila význam** (např. čárka, která mění smysl věty; velké písmeno, které rozhoduje, zda jde o instituci).
- **Rejstřík je nejednotný** (text střídá tykání a vykání, formální a hovorové pasáže) — zeptej se, který je zamýšlený.
- **Tvrzení stojí na vágní autoritě** („odborníci se shodují", „studie ukazují") a bez atribuce by působilo jako autorovo vlastní tvrzení. Atribuci sám neodstraňuj; zeptej se, zda ji zachovat, doložit zdrojem, nebo celé tvrzení vypustit.
- **Odborný žargon vs. AI slovník**: slovo jako „robustní" nebo „škálovat" může být v oboru autora legitimní termín. Když kontext nestačí k rozhodnutí, zeptej se.

Otázky formuluj konkrétně a s návrhem řešení: „Věta X nenese věcnou informaci. Mám ji (a) vypustit, (b) nechat a jen zestručnit?"

## Kalibrace hlasu (volitelné)

Pokud uživatel dodá vzorek vlastního psaní, před přepisem ho analyzuj: délky a rytmus vět, úroveň slovní zásoby, jak začíná odstavce, interpunkční návyky, opakující se obraty, tykání/vykání. Ve finálním textu pak nahrazuj AI vzorce obraty ze vzorku — ne generickou „čistou" češtinou. Bez vzorku piš neutrální přirozenou češtinou v rejstříku originálu. Kalibrace hlasu NIKDY nepovoluje přidat obsah — mění jen způsob formulace.

---

# VZORCE K DETEKCI

## A. OBSAHOVÉ VZORCE

### 1. Inflace významu a „širších souvislostí"

**Sledovat:** představuje zásadní milník, hraje klíčovou/zásadní roli, podtrhuje význam, odráží širší trend(y), symbolizuje, v kontextu doby, zanechal nesmazatelnou stopu, formuje podobu, měnící se krajina, klíčový bod obratu

**Problém:** AI nafukuje důležitost dovětky o tom, co věc „představuje" nebo „symbolizuje".

**Před:**
> Knihovna byla založena v roce 1897, což představovalo zásadní milník ve vývoji regionálního vzdělávání a odráželo širší emancipační trendy tehdejší společnosti.

**Po:**
> Knihovna byla založena v roce 1897.

*(Pozn.: dovětek o „širších trendech" nenese ověřitelné sdělení nad rámec data — pokud si nejsi jistý, zda ho autor chce zachovat, zeptej se.)*

### 2. Zdůrazňování významnosti a mediálního pokrytí

**Sledovat:** přední odborník, renomovaný, získal si pozornost médií, aktivně působí na sociálních sítích, ceněný

**Problém:** AI čtenáře přesvědčuje o důležitosti místo věcného sdělení. Přeformuluj věcně, nálepky významnosti zmírni — ale nevymýšlej konkrétní údaje, které v textu nejsou.

### 3. Vlečné „což"-věty a falešná hloubka analýzy

**Sledovat:** , což podtrhuje…, což odráží…, což dokládá…, což svědčí o…, čímž přispívá k…; archaické přechodníky (symbolizujíc, odrážejíce)

**Problém:** Český ekvivalent anglických „-ing" dovětků. AI přilepí za věcnou větu interpretační ocásek, který nic nedokládá.

**Před:**
> Fasáda kombinuje modrou a zlatou, což odráží hluboké sepětí komunity s krajinou a podtrhuje nadčasovost celého záměru.

**Po:**
> Fasáda kombinuje modrou a zlatou.

*(Interpretační dovětek bez obsahu → zeptej se, zda vypustit, nebo zachovat zestručněný.)*

### 4. Reklamní jazyk

**Sledovat:** pyšní se, vibrantní/pulzující, malebný, uhnízděný, v srdci, dechberoucí, ohromující, jedinečný, nezapomenutelný, must-see, prémiový (figurativně), špičkový, revoluční, přelomový

**Problém:** AI neudrží neutrální tón. Přeformuluj věcně se zachováním informace.

**Před:**
> Ostrava se pyšní jedinečnou industriální atmosférou a dechberoucí proměnou Dolních Vítkovic.

**Po:**
> Ostrava má výraznou industriální atmosféru; areál Dolních Vítkovic prošel rozsáhlou proměnou.

### 5. Vágní autority

**Sledovat:** odborníci se shodují, studie ukazují, experti varují, podle mnohých, obecně se má za to, kritici namítají (bez uvedení kterých)

**Problém:** AI připisuje tvrzení anonymním autoritám. POZOR — striktní režim: NIKDY nedoplňuj konkrétní zdroj, který v originálu není, ani atribuci nemaž ve prospěch přímého tvrzení. Pokud ji nelze přirozeně přeformulovat a zachovat, zeptej se, zda ji ponechat, doložit, nebo celé tvrzení vypustit.

### 6. Formulaické sekce „Výzvy a budoucnost"

**Sledovat:** Navzdory těmto výzvám…, čelí řadě výzev, I přes uvedené překážky… nadále vzkvétá, Budoucí vyhlídky

**Problém:** Šablonovitý závěr „výzvy — ale optimismus". Přeformuluj věcně; pokud pasáž nenese konkrétní obsah, zeptej se.

### 7. Pravidlo tří

**Problém:** AI nutí výčty do trojic („inovace, inspirace a nové kontakty"). Trojici z originálu zachovej obsahově — ale rozbij rytmus: dvě položky do jedné věty, třetí zvlášť, nebo změň pořadí větných členů. Žádnou položku nevypouštěj.

### 8. Cyklování synonym

**Problém:** AI se bojí opakování: „protagonista… hlavní hrdina… ústřední postava… hrdina". Čeština opakování klíčového slova snese (a zájmena a elipsa pomohou). Sjednoť pojmenování na nejjasnější variantu.

### 9. Falešné rozsahy

**Sledovat:** od X po Y (kde X a Y neleží na žádné škále)

**Před:** „od velkého třesku po temnou hmotu" → **Po:** vyjmenuj témata prostě vedle sebe: „velký třesk, vznik hvězd a temná hmota".

## B. JAZYKOVÉ VZORCE

### 10. Český AI slovník

**Vysoce frekventovaná AI slova:** klíčový, zásadní, komplexní, robustní, efektivní, dynamický, inovativní, unikátní, nicméně, v neposlední řadě, v dnešní (digitální/rychlé) době, ať už… nebo…, je důležité si uvědomit / poznamenat, pojďme se podívat / ponořit, v rámci (nadužívané), díky (kde jde o neutrální příčinu), krajina (kalk „landscape": „krajina AI nástrojů"), tapiserie, rezonovat s, svědectví (kalk „testament")

**Problém:** Tato slova se v post-2023 textech vyskytují násobně častěji a shlukují se. Jednotlivý výskyt nevadí — nahrazuj při nakupení. Náhrady: klíčový → důležitý/hlavní/podstatný (nebo nic), nicméně → ale/jenže, v rámci → v/při/během.

### 11. Vyhýbání se sponě („je"/„má")

**Sledovat:** představuje, slouží jako, tvoří, disponuje, nabízí (kde jde o prosté vlastnictví), pyšní se, funguje jako

**Problém:** AI (a český úřední styl) nahrazuje prosté „je" a „má" honosnými konstrukcemi.

**Před:**
> Galerie představuje hlavní výstavní prostor spolku a disponuje čtyřmi sály o celkové ploše 300 m².

**Po:**
> Galerie je hlavní výstavní prostor spolku. Má čtyři sály o celkové ploše 300 m².

### 12. Negativní paralelismy a useknuté negace

**Sledovat:** Nejde jen o X, ale o Y; Není to jen nástroj, je to…; …, žádné hádání; …, žádná ztráta času

**Problém:** Nadužívaná figura „ne A, ale B" a anglicky střižené dovětky („no guessing").

**Před:**
> Nejde jen o rychlost, jde o celkový zážitek. Vše najdete na jednom místě, žádné přepínání mezi aplikacemi.

**Po:**
> Rychlost je jen část celkového zážitku. Vše najdete na jednom místě, takže nemusíte přepínat mezi aplikacemi.

### 13. Trpný rod a nominalizace

**Sledovat:** bylo rozhodnuto, je zajišťováno, dochází k, provádět kontrolu / realizovat školení (místo kontrolovat / školit), ze strany

**Problém:** AI kombinuje anglické pasivum s českým úřednickým stylem. Česká stylistika preferuje činný rod a zvratné pasivum, pokud je jasný konatel — ale konatele NEVYMÝŠLEJ; když v originálu není, použij zvratné pasivum („zajišťuje se") nebo se zeptej.

**Před:**
> Registrace je zajišťována organizátorem. Následně dochází k odeslání potvrzení.

**Po:**
> Registraci zajišťuje organizátor a poté odešle potvrzení.

### 14. Slovosled: anglické kalky a aktuální členění

**Problém:** AI drží anglický pevný slovosled a ignoruje, že čeština klade známou informaci (téma) na začátek a novou/důležitou (réma) na konec věty. Výsledek je gramaticky správný, ale „nečesky" plochý. Při přepisu uspořádej věty tak, aby nová informace stála na konci. Významová stavba se nemění — jen pořadí slov.

**Před:**
> Nová funkce byla přidána do aplikace minulý týden. Uživatelé mohou nyní exportovat data jedním kliknutím.

**Po:**
> Minulý týden do aplikace přibyla nová funkce: data teď uživatelé exportují jedním kliknutím.

### 15. Nadbytečná zájmena (kalk z angličtiny)

**Sledovat:** tento/tato/toto jako kalk „this" na začátcích vět; váš/vaše v marketingovém rytmu („váš byznys, vaše cíle, vaši zákazníci"); nadbytečné „my"/„vy" tam, kde stačí slovesná osoba

**Problém:** Čeština zájmena vypouští — osoba je ve tvaru slovesa. Hromadění přivlastňovacích a ukazovacích zájmen je spolehlivý překladový tell.

**Před:**
> Tento nástroj vám pomůže zefektivnit vaši práci a dosáhnout vašich cílů.

**Po:**
> Nástroj pomáhá pracovat efektivněji a dosáhnout cílů, které si stanovíte.

### 16. Svůj vs. jeho/její/jejich

**Problém:** AI chybuje v reflexivním přivlastňování: „firma zveřejnila její výsledky" (správně „své výsledky"), a naopak nadužívá „svůj" tam, kam nepatří. Oprav podle vztahu k podmětu věty (IJP, kap. Konkurence přivlastňovacích zájmen).

### 17. Přemíra knižnosti a nejednotný rejstřík

**Sledovat:** lze, je nutno, je třeba, veškerý, taktéž, kupříkladu, jenž — v textu, který je jinak konverzační

**Problém:** Opak anglického problému: česká AI sklouzává do formálnějšího rejstříku, než jaký text má. „Lze" v newsletteru psaném tykáním je tell. Sjednoť rejstřík podle převažujícího tónu originálu; při nejednoznačnosti se zeptej.

### 18. Vata a výplňové fráze

**Před → Po:**
- „za účelem dosažení" → „aby / k / pro"
- „z důvodu, že" → „protože"
- „v současné době" → „teď / nyní" (nebo vypustit)
- „v případě, že potřebujete" → „pokud potřebujete"
- „systém má schopnost zpracovat" → „systém zpracuje"
- „je důležité poznamenat, že data ukazují" → „data ukazují"
- „co se týče ceny" → „cena…" (přestavět větu)

### 19. Nadměrné zajišťování (hedging)

**Před:** „Dalo by se potenciálně argumentovat, že opatření by mohlo mít určitý vliv…" → **Po:** „Opatření může ovlivnit…" — jedna míra nejistoty stačí; stupeň nejistoty originálu zachovej (nejisté tvrzení nesmíš přepsat na jisté).

### 20. Generické pozitivní závěry

**Sledovat:** Budoucnost vypadá slibně, čekají nás vzrušující časy, správný krok správným směrem, na cestě k dokonalosti

**Problém:** Prázdný optimistický závěr. STRIKTNÍ REŽIM: nesmíš ho nahradit konkrétním plánem, který v textu není, ani ho sám smazat → zeptej se uživatele (vypustit / zestručnit).

**Výjimka — marketingové a lákací texty:** výzva k akci (CTA) je legitimní žánrový prvek, ne vata. CTA nemaž ani na ni nezakládej otázku; jen ji zbav prázdných frází a přeformuluj věcně z informací, které text už obsahuje (termín, vstupenka, místo). „Nenechte si ujít tuto jedinečnou událost — budoucnost vypadá slibně!" → „Vstupenky jsou v prodeji na webu festivalu; termín je 15.–18. října."

## C. TYPOGRAFIE A PRAVOPIS (dle IJP — pozor, jiná logika než v angličtině!)

### 21. Uvozovky: české „ ", ne anglické " "

**Pravidlo:** Správné české uvozovky jsou „dole a nahoře" (příp. vnitřní ‚jednoduché'). Silný AI tell v českém textu jsou **anglické oblé uvozovky " "** — vždy převést na české. Rovné uvozovky "…" jsou přijatelné v neformálním webovém textu; v profesionálním textu převáděj na „…". Pokud je v textu smíšený úzus, sjednoť na české.

### 22. Pomlčky: počeštit, ne vymýtit

**Pravidlo (liší se od anglického humanizeru!):** Pomlčka (–) je legitimní česká interpunkce, ale v běžném textu VZÁCNÁ. Cílem není nula pomlček, ale jejich výjimečnost:
- **Dlouhá pomlčka — bez mezer** (anglický em dash) je anglicismus a spolehlivý AI tell → vždy odstranit (viz náhrady níže).
- **Pomlčka jako lepidlo vět** („Světla jsou jen začátek – festival mění, jak se díváte na město.") je kalk anglického em dashe a v češtině se běžně nedělá. Výchozí náhrady v tomto pořadí: (a) rozdělit na dvě věty tečkou, (b) spojit čárkou se spojkou (a, ale, protože, takže), (c) dvojtečka, pokud druhá část vysvětluje nebo uvozuje výčet. Totéž platí pro pomlčku před dovětkem („…nadšenci – a stejně dobře jen za zážitkem") → obyčejná čárka.
- **Vsuvkové dvojice pomlček** („text – vsuvka – text") → přestav na čárky, závorky nebo samostatnou větu.
- **Tvrdý limit:** ve finálním textu smí zůstat NEJVÝŠE JEDNA pomlčka s mezerami na celý běžný text (u dlouhých textů na ~5 odstavců), a jen pokud nese skutečný zvrat či důraz, který by tečka zabila. Všechny ostatní přepiš.
- **Rozsahy a intervaly** píše čeština pomlčkou bez mezer: 9–17 h, s. 12–15, Praha–Brno. Tyto NEODSTRAŇUJ a do limitu se nepočítají.
- Spojovník (-) a pomlčka (–) nejsou totéž: spojovník spojuje (česko-anglický, bude-li), pomlčka odděluje.

### 23. Velká písmena v nadpisech (Title Case)

**Pravidlo:** Čeština Title Case nemá. „Strategická Partnerství A Nové Příležitosti" je stoprocentní AI tell → velké písmeno jen na začátku nadpisu a u vlastních jmen. Zároveň zkontroluj velká písmena dle IJP (instituce vs. obecné pojmenování) — když velké/malé písmeno mění význam, zeptej se.

### 24. Mezery, čísla, procenta, datum (IJP / ČSN 01 6910)

- **Procenta:** 5 % = pět procent (s mezerou), 5% = pětiprocentní (bez mezery). AI to zaměňuje.
- **Datum:** 19. 7. 2026 (s mezerami za tečkami).
- **Tisíce:** 10 000 (mezera, ne čárka ani tečka).
- **Jednotky:** 300 m², 25 °C (s mezerou).
- **Tři tečky:** jeden znak … , ne tři tečky za sebou; před nimi bez mezery.
- **Řadové číslovky:** tečka a mezera (na 1. místě).

### 25. Tučné písmo, emoji, odrážkové pseudo-nadpisy

- Mechanické **tučnění** klíčových slov v každé větě → zruš, tučné jen kde plní funkci.
- **Emoji** v nadpisech a odrážkách (🚀, ✅, 💡) → odstranit, pokud nejsou autorský záměr (u neformálního textu se zeptej).
- **Odrážky s tučným návěštím** („**Výkon:** Výkon se zlepšil…") → převést do souvislé věty; obsah položek zachovat beze zbytku.

### 26. Fragmentované nadpisy

**Problém:** Nadpis + jednořádkový odstavec, který nadpis jen zopakuje („## Výkon / Rychlost je důležitá."). Opakující větu slouč s následujícím obsahem — informaci nezahazuj, jen ruš duplicitu.

## D. KOMUNIKAČNÍ VZORCE

### 27. Chatbot artefakty

**Sledovat:** Doufám, že to pomůže!, Samozřejmě!, Rád(a) vysvětlím, Dejte mi vědět, pokud…, Chcete, abych…?, Zde je přehled…

**Problém:** Do textu se dostala konverzační omáčka z chatu. Odstraň — toto je jediný případ, kdy se text maže bez ptaní, protože nejde o autorský obsah, ale o artefakt nástroje.

### 28. Servilní tón

**Sledovat:** Skvělá otázka!, Máte naprostou pravdu!, Výborný postřeh!

**Problém:** Přehnaně vstřícné fráze → odstraň podle stejné logiky jako č. 27.

### 29. Disclaimery a spekulativní vycpávky

**Sledovat:** k datu mé poslední aktualizace, dostupné zdroje uvádějí jen omezené informace, pravděpodobně vyrůstal…, má se za to, že…, udržuje si soukromí (jako výplň chybějících dat)

**Problém:** (a) Zbytky knowledge-cutoff hlášek → odstranit jako artefakt. (b) Spekulativní domýšlení chybějících faktů → NIKDY nenahrazuj vlastní spekulací; ponech konstatování „není doloženo", nebo se zeptej, zda pasáž vypustit.

### 30. Ohlašování a metakomentáře

**Sledovat:** Pojďme se podívat na…, Pojďme se ponořit do…, Zde je vše, co potřebujete vědět, Nyní se zaměříme na, Bez dlouhých řečí

**Problém:** Text ohlašuje, co udělá, místo aby to udělal. Odstraň ohlášku, obsah začni rovnou — informace z ohlášky (pokud nějakou nese) přesuň do první věcné věty.

### 31. Vyrobené pointy a staccato drama

**Problém:** Série krátkých úderných fragmentů pro efekt („Žádné kompromisy. Žádné čekání. Jen výsledky."). Jedna krátká věta pro důraz je v pořádku; série je vyrobená → spoj do souvislé věty se zachováním všech položek.

### 32. Aforistické vzorce

**Sledovat:** X je jazykem Y, X je měnou Z, X není nástroj, ale zrcadlo, architektura důvěry

**Problém:** Pseudohluboká formule místo věcného tvrzení. Přeformuluj na konkrétní tvrzení, které formule opisuje — ale jen pokud je z kontextu jasné, co autor mínil; jinak se zeptej.

### 33. Falešně důvěrné otvíráky

**Sledovat:** Upřímně?, Řekněme si to na rovinu, Popravdě…, Věc se má tak, jako samostatné teatrální pauzy před běžným sdělením

**Problém:** Vyrobená důvěrnost. Sdělení řekni rovnou; otvírák odstraň, pokud nenese význam.

---

# DETEKCE: CO NEHLÁSIT (falešné poplachy)

Kvalitní lidský text může zasáhnout několik vzorců výše. Před přepisem ověř, že nekucháš legitimní prózu. Samy o sobě NEJSOU spolehlivé indikátory:

- **Bezchybný pravopis a konzistentní styl.** Mnoho autorů je profesionálů nebo prošli korekturou.
- **Formální slovní zásoba.** AI nadužívá KONKRÉTNÍ slova (č. 10), ne všechna knižní slova. „Nicméně" jednou za text není tell.
- **Jedna pomlčka, jedna trojice, jedno „klíčový".** Hledej SHLUKY tellů, ne izolované výskyty. Jedna pomlčka nic neznamená; pomlčky + pravidlo tří + „vibrantní tapestrie" + generický závěr = přiznání.
- **České uvozovky a správná typografie.** Pozor na obrácenou logiku: v češtině je správná typografie („ ", – s mezerami, 5 %) známkou pečlivého autora NEBO dobře nastaveného nástroje — tell jsou anglické návyky (" ", —, Title Case).
- **Krátká úderná věta.** Lidé jimi zakončují pointu. Hlásit až sérii fragmentů.
- **Odborný žargon.** „Robustní řešení" od vývojáře může být termín. V pochybnostech se zeptej.
- **Text v uvozovkách, názvy, příklady.** Sledovaná fráze uvnitř citace nebo tam, kde se o ní mluví (místo aby se používala), se nepřepisuje.

## Známky lidského psaní (zachovat!)

Když je vidíš, nech prózu na pokoji — nadměrná redakce zničí to, co ji dělá lidskou:

- Konkrétní, těžko vymyslitelný detail (přesná adresa, zvláštní citát, „právnička, co sedávala nad ordinací mého zubaře").
- Smíšené pocity a nevyřešené napětí („většinou se mi to líbí, ale něco mi na tom vadí a neumím říct co").
- Dobové odkazy, slang, interní vtipy vázané na konkrétní rok a subkulturu.
- Střídání krátkých a dlouhých vět; skutečné vsuvky, závorky a sebeopravy.
- Nářeční a regionální prvky (moravismy, obecná čeština) — NEOPRAVUJ je na spisovné, pokud jsou konzistentní součástí hlasu; při pochybnosti se zeptej.

---

# PROCES A VÝSTUP

1. **Přečti celý vstup** a označ si všechny výskyty vzorců výše. Zapiš si věcný obsah každého odstavce (kontrolní seznam informací — nic se nesmí ztratit).
2. **Sesbírej sporná místa** (viz Kdy se ptát uživatele). Pokud nějaká jsou, polož otázky HNED — před psaním finále. Je-li k dispozici AskUserQuestion, použij ho; jinak otázky vypiš a počkej na odpověď.
3. **Napiš pracovní přepis.** Zkontroluj: čte se přirozeně nahlas? Střídá délky vět? Drží české aktuální členění? Používá „je/má" místo opisů? Drží rejstřík originálu?
4. **Audit:** polož si otázku „Co na tomto textu pořád křičí, že ho psala AI?" — odpověz stručně v bodech.
5. **Kontrola obsahu proti seznamu z kroku 1:** každá informace originálu je ve finále, žádná nepřibyla. Pokud ne, oprav.
6. **Finální přepis:** vyřeš zbylé telly, projdi typografii (uvozovky „ ", pomlčky, mezery, procenta, data). Skenuj na: (a) — em dash bez mezer a anglické uvozovky " " — jakýkoli nález znamená, že finále není hotové; (b) spočítej pomlčky s mezerami (rozsahy typu 15.–18. se nepočítají) — víc než jedna na běžný text znamená, že finále není hotové: rozsekej věty tečkou nebo spoj čárkou (viz č. 22).

**Odevzdej:** finální text + stručný přehled provedených změn (co a proč) + případný seznam míst, kde ses držel originálu navzdory tellu (např. autorská pomlčka), aby měl uživatel kontrolu.

---

# UKÁZKOVÝ PŘÍKLAD

**Před (AI čeština):**
> V dnešní rychlé digitální době představuje umělá inteligence klíčový nástroj pro neziskové organizace. Nejde jen o technologii — jde o zásadní změnu způsobu práce. AI nabízí řadu benefitů: efektivitu, inovaci a úsporu času. "Experti se shodují," že organizace, které AI nevyužívají, zaostávají. Pojďme se podívat na to, jak může AI transformovat vaši organizaci, vaše procesy a vaši komunikaci. Budoucnost vypadá slibně! 🚀

**Otázky před finálním zněním:**
1. Věta „Budoucnost vypadá slibně!" nenese věcnou informaci — vypustit, nebo ponechat věcněji formulovanou?
2. „Experti se shodují" nemá v textu zdroj — mám atribuci zachovat, doplníš zdroj, nebo chceš celé tvrzení vypustit?
3. Emoji 🚀 — odstranit, nebo je součást tvého stylu pro tento kanál?

**Po (za předpokladu odpovědí: 1 vypustit, 2 zachovat atribuci, 3 odstranit):**
> Umělá inteligence je pro neziskové organizace důležitý nástroj a mění způsob, jakým pracují. Přináší úsporu času, efektivnější procesy i prostor pro nové postupy. Odborníci se shodují, že organizace, které ji nevyužívají, zaostávají. AI přitom může proměnit fungování organizace, její procesy i komunikaci.

**Provedené změny:** odstraněn AI slovník (v dnešní digitální době, klíčový, zásadní), negativní paralelismus (nejde jen o…), em dash, anglické uvozovky, pravidlo tří rozbito se zachováním všech tří položek, metakomentář (pojďme se podívat) a kalk přivlastňovacích zájmen (vaši/vaše). Vágní atribuce zůstala zachována, protože uživatel nedodal zdroj ani nepovolil vypuštění tvrzení. Generický závěr a emoji byly vypuštěny podle jeho odpovědi. Veškerý ostatní věcný obsah zůstal zachován.

---

# REFERENCE

- **Internetová jazyková příručka ÚJČ** (prirucka.ujc.cas.cz) — závazná reference pro pravopis a typografii; nahradila tištěná Pravidla českého pravopisu (poslední kodifikační úprava 1993, Dodatek 1994).
- **Akademická příručka českého jazyka** (3. vydání) — knižní protějšek IJP.
- **ČSN 01 6910** — úprava dokumentů, typografická pravidla.
- Vzorce AI psaní vycházejí z Wikipedia: Signs of AI writing (WikiProject AI Cleanup), adaptováno pro češtinu.
