"""Per-category Wikidata SPARQL queries.

Design decisions:

- **`wdt:P31/wdt:P279*` only on NARROW classes.** For types with a small
  subclass tree, such as football club and political party, the subclass walk
  is cheap and necessary. On broad classes such as software (Q7397) or human
  (Q5), the same walk hits WDQS's 60 s wall; there we use plain `wdt:P31` + a
  high sitelink floor.
- **Sitelink count is a popularity proxy.** The floor is low for
  Turkey-focused categories (we want coverage) and high worldwide (only truly
  well-known entities; otherwise the dictionary bloats with noise and
  produces collisions).
- **People are narrowed by occupation + citizenship.** Scanning `wd:Q5` with a
  sitelink filter means 10M+ rows and times out.
- **Priority is used in collision resolution.** When the same alias points to
  two entities, priority is checked first, and the sitelink ratio if tied
  (see build.resolve_collisions).

⚠️ **QIDs that need verification:** `Q1520223` (Turkish district) is the least
reliable class in this list. A wrong QID raises no error — it silently returns
0 rows. That is why the build logs every category with no rows as a WARNING
and writes it to the report; when you see a 0-row category, verify the QID by
hand.
"""

from __future__ import annotations

from dataclasses import dataclass

TURKEY = "wd:Q43"


@dataclass(frozen=True)
class Category:
    """A single SPARQL fetch and its weight in the dictionary."""

    name: str
    selector: str  # triple patterns that bind ?item
    min_sitelinks: int
    priority: int  # the higher one wins on collision
    limit: int = 20000  # TOTAL row cap per category (all pages)

    # Whether the LAST WORD of the canonical name should also produce an alias
    # (surname).
    #
    # Why it is needed: people are referred to by surname but the Wikidata
    # label is the full name. Example: "Donald J. Trump" was in the
    # dictionary but the `trump` key was not, so "Trump" never matched.
    #
    # ⚠️ OFF for TURKISH person categories, and this is intentional. Turkish
    # surnames were chosen in 1934 from everyday words: Yavaş, Demir, Kaya,
    # Şahin, Aslan, Çelik, Yıldız... Producing `yavas` from "Mansur Yavaş"
    # would attach a mayor to every post containing "yavaş". In Turkish text
    # these people are already written with their full names, and Wikidata
    # gives the surname of the popular ones as an altLabel (erdogan, bahceli).
    surname_alias: bool = False


# Page size. `limit` is the total cap, while this is the number of rows
# requested in a single request: instead of hitting WDQS's 60 s wall with one
# giant query, we advance with `OFFSET`.
PAGE_SIZE = 10000


# ── Query template ──────────────────────────────────────────
# The tr label is preferred, falling back to en (names like "OpenAI" have no
# tr label but the English one is already the canonical form).
#
# ⚠️ `ORDER BY` is part of the contract, not decoration. SPARQL does NOT
# GUARANTEE ordering; paging with `LIMIT`/`OFFSET` without `ORDER BY` is
# unstable — which entities survive changes from run to run and, worse, an
# INCLUDED entity may arrive with an incomplete alias set. Sitelink
# (popularity proxy) descending, deterministic on ties via ?item.
_TEMPLATE = """SELECT ?item ?lblTr ?lblEn ?alias ?sitelinks WHERE {{
  {selector}
  ?item wikibase:sitelinks ?sitelinks .
  FILTER(?sitelinks >= {min_sitelinks})
  OPTIONAL {{ ?item rdfs:label ?lblTr . FILTER(LANG(?lblTr) = "tr") }}
  OPTIONAL {{ ?item rdfs:label ?lblEn . FILTER(LANG(?lblEn) = "en") }}
  FILTER(BOUND(?lblTr) || BOUND(?lblEn))
  OPTIONAL {{ ?item skos:altLabel ?alias . FILTER(LANG(?alias) IN ("tr", "en")) }}
}}
ORDER BY DESC(?sitelinks) ?item
LIMIT {limit}
OFFSET {offset}"""


def build_query(cat: Category, limit: int | None = None, offset: int = 0) -> str:
    """Single-page query. If `limit` is not given, the category cap is used."""
    return _TEMPLATE.format(
        selector=cat.selector.strip(),
        min_sitelinks=cat.min_sitelinks,
        limit=cat.limit if limit is None else limit,
        offset=offset,
    )


CATEGORIES: list[Category] = [
    # ── Politics ─────────────────────────────────────────────
    Category(
        name="political_party_tr",
        selector=f"?item wdt:P31/wdt:P279* wd:Q7278 ; wdt:P17 {TURKEY} .",
        min_sitelinks=1,
        priority=95,
    ),
    Category(
        name="political_party_world",
        selector="?item wdt:P31/wdt:P279* wd:Q7278 .",
        min_sitelinks=30,
        priority=72,
    ),
    Category(
        # Occupation + sitelink floor; citizenship is free (world leaders).
        name="politician_world",
        selector="?item wdt:P31 wd:Q5 ; wdt:P106 wd:Q82955 .",
        min_sitelinks=100,
        priority=58,
        surname_alias=True,
    ),
    Category(
        name="politician_tr",
        selector=f"?item wdt:P31 wd:Q5 ; wdt:P106 wd:Q82955 ; wdt:P27 {TURKEY} .",
        min_sitelinks=3,
        priority=62,
    ),
    # ── Sports ───────────────────────────────────────────────
    Category(
        # Q476028 = football club, Q103229495 = men's association football team.
        # The UNION is required: they are separate trees, neither is a
        # subclass of the other. Live WDQS: for FC Barcelona (Q7156)
        # `P31/P279* Q476028` → false, `P31/P279* Q103229495` → true.
        # When run with Q476028 alone, FC Barcelona never made it into the
        # dictionary; the only thing that did was its reserve team
        # "FC Barcelona B". The union raises world clubs from 458 → 640 (~1.6 s).
        name="football_club_tr",
        selector=(
            "{ ?item wdt:P31/wdt:P279* wd:Q476028 } UNION "
            "{ ?item wdt:P31/wdt:P279* wd:Q103229495 } "
            f"?item wdt:P17 {TURKEY} ."
        ),
        min_sitelinks=2,
        priority=90,
    ),
    Category(
        name="football_club_world",
        selector=(
            "{ ?item wdt:P31/wdt:P279* wd:Q476028 } UNION "
            "{ ?item wdt:P31/wdt:P279* wd:Q103229495 }"
        ),
        min_sitelinks=45,
        priority=78,
    ),
    Category(
        name="footballer_tr",
        selector=f"?item wdt:P31 wd:Q5 ; wdt:P106 wd:Q937857 ; wdt:P27 {TURKEY} .",
        min_sitelinks=3,
        priority=56,
    ),
    # ── Technology ───────────────────────────────────────────
    Category(
        # Q18388277 = technology company. NO subclass walk (broad class).
        name="tech_company",
        selector="?item wdt:P31 wd:Q18388277 .",
        min_sitelinks=15,
        priority=85,
    ),
    Category(
        # Q7397 = software. High floor, only well-known products.
        name="software",
        selector="?item wdt:P31 wd:Q7397 .",
        min_sitelinks=40,
        priority=80,
    ),
    Category(
        # Q3220391 = social networking service. Q202833, used in an earlier
        # attempt, returned only 2 entities.
        #
        # ⚠️ This class brings X, TikTok, Facebook, Reddit but
        # **does NOT bring Instagram, WhatsApp, Telegram, Snapchat, Pinterest,
        # Discord** (live count: 60 entities, none of them Instagram). The
        # note "Instagram is a direct instance of this class" is no longer
        # TRUE — Wikidata changed the classification. A subclass walk is not
        # the fix: with `P31/P279*` WDQS returns 502. Since these names are
        # very common in Turkish posts, the right place is `overrides.json`.
        name="social_platform",
        selector="?item wdt:P31 wd:Q3220391 .",
        min_sitelinks=20,
        priority=82,
    ),
    # ── Geography ────────────────────────────────────────────
    Category(
        # NO subclass walk, deliberately: with `P31/P279*` it goes from 215 →
        # 667 and the surplus is historical/unrecognized states. Since the
        # floor is min_sitelinks=0 there is no popularity filter either — the
        # current 215 is the match for the "country" concept that we want.
        name="country",
        selector="?item wdt:P31 wd:Q6256 .",
        min_sitelinks=0,
        priority=70,
    ),
    Category(
        # The subclass walk is REQUIRED: Istanbul's (Q406) P31 is "megacity",
        # "big city", "port city" — plain `P31 Q515` NEVER returns it. The
        # plain query gave 181 cities and Turkey's largest city was not in the
        # dictionary (while its 20+ districts were in district_tr).
        # Transitive: 1171 cities, ~2.7 s.
        name="city_tr",
        selector=f"?item wdt:P31/wdt:P279* wd:Q515 ; wdt:P17 {TURKEY} .",
        min_sitelinks=2,
        priority=66,
    ),
    Category(
        # Q1147395 = district. Verified against live WDQS: Kadıköy is an
        # instance of this class. Q1520223, used in the first attempt,
        # returned **0 rows** — Wikidata does not report a wrong class QID as
        # an error, it silently returns an empty result. The per-category
        # 0-row warning (build.fetch_category) exists precisely to catch this.
        name="district_tr",
        selector=f"?item wdt:P31 wd:Q1147395 ; wdt:P17 {TURKEY} .",
        min_sitelinks=1,
        priority=52,
    ),
    # ── Institutions ─────────────────────────
    # A curation review showed the dictionary had NO
    # institution/court/international-organization category: UN, NATO, EU,
    # TBMM, AFAD, Yargıtay, the Constitutional Court were all missing. Product
    # decision: **institutions yes, organizations no** — terror/armed
    # organizations and civil/religious organizations do NOT enter these
    # categories.
    Category(
        # Q48336 = province of Turkey. For provinces whose name equals the
        # provincial center (Ankara, Bursa...) the city record is already in
        # city_tr and both records "carry their own name"; since the priority
        # is LOWER than city_tr, the city wins. The real gain is provinces
        # whose center has a different name: `hatay` (center Antakya) was not
        # in the dictionary at all — 67 posts in the measurement.
        name="province_tr",
        selector="?item wdt:P31 wd:Q48336 .",
        min_sitelinks=1,
        priority=64,
    ),
    Category(
        # Government agency (Q327333) + court (Q41487) + legislature (Q11204) +
        # armed forces (Q772547), P17=Turkey, with a subclass walk.
        # ~160 records. The exclusions were found by measurement: foreign
        # embassies (Q3917681) and consulates (Q7843791) carry P17=Turkey, 27
        # records such as "TBMM 25th term members list" are legislative terms
        # (Q20988497), numbered governments are cabinets (Q640506).
        # ⚠️ Armed organization (Q17149090) is NOT excluded here: TSK is its
        # subclass and was dropping out of this category.
        name="state_tr",
        selector=(
            "VALUES ?cls { wd:Q327333 wd:Q41487 wd:Q11204 wd:Q772547 } "
            f"?item wdt:P31/wdt:P279* ?cls ; wdt:P17 {TURKEY} . "
            "FILTER NOT EXISTS { ?item wdt:P31/wdt:P279* ?bad . "
            "VALUES ?bad { wd:Q3917681 wd:Q7843791 wd:Q640506 wd:Q13406463 wd:Q20988497 } }"
        ),
        min_sitelinks=2,
        priority=63,
    ),
    Category(
        # Intergovernmental (Q245065), supranational union (Q1335818),
        # regional organization (Q4120211), military alliance (Q1127126),
        # international financial institution (Q1345691). PLAIN P31 — the
        # transitive walk gave 504 on WDQS.
        # ⚠️ The generic "international organization" (Q484652) is
        # DELIBERATELY absent: it brought NGOs and religious/political groups
        # (Muslim Brotherhood, Krishna Consciousness Society, College of
        # Cardinals, YMCA) — "organizations no".
        # ⚠️ Q7210356 is "political organization", NOT terror organization;
        # excluding it in the first attempt also dropped UN/NATO/EU. The
        # terror organization class is Q17127659.
        name="intl_org",
        selector=(
            "VALUES ?cls { wd:Q245065 wd:Q1335818 wd:Q4120211 wd:Q1127126 wd:Q1345691 } "
            "?item wdt:P31 ?cls . "
            "FILTER NOT EXISTS { ?item wdt:P31/wdt:P279* wd:Q17127659 }"
        ),
        min_sitelinks=40,
        priority=61,
    ),
    Category(
        # Same reasoning as city_tr: Munich/Vienna/Moscow/Athens are all in the
        # "big city" class and are not caught by plain `P31 Q515`.
        # Plain 899 → transitive 2026 entities (~1.8 s); the sitelink floor of
        # 70 already serves as the popularity filter.
        name="city_world",
        selector="?item wdt:P31/wdt:P279* wd:Q515 .",
        min_sitelinks=70,
        priority=60,
    ),
    # ── Popular culture ──────────────────────────────────────
    Category(
        # The subclasses are real series types such as anime/web series;
        # 737 → 1047 entities (~1.7 s).
        name="tv_series",
        selector="?item wdt:P31/wdt:P279* wd:Q5398426 .",
        min_sitelinks=25,
        priority=54,
    ),
    Category(
        name="musician_tr",
        selector=f"?item wdt:P31 wd:Q5 ; wdt:P106 wd:Q639669 ; wdt:P27 {TURKEY} .",
        min_sitelinks=3,
        priority=54,
    ),
    Category(
        name="actor_tr",
        selector=f"?item wdt:P31 wd:Q5 ; wdt:P106 wd:Q33999 ; wdt:P27 {TURKEY} .",
        min_sitelinks=3,
        priority=54,
    ),
    # ── Non-football sports ──────────────────────────────────
    # In Turkey basketball is the most talked-about sport after football but
    # it was not in the dictionary at all. The club priority is BELOW the
    # football club's (90): "Fenerbahce" exists in both leagues, and in prose
    # the football club is more likely to be meant.
    Category(
        # Q13393265 = basketball team. Live count: 139 TR.
        name="basketball_club_tr",
        selector=f"?item wdt:P31/wdt:P279* wd:Q13393265 ; wdt:P17 {TURKEY} .",
        min_sitelinks=1,
        priority=88,
    ),
    Category(
        name="basketball_club_world",
        selector="?item wdt:P31/wdt:P279* wd:Q13393265 .",
        min_sitelinks=30,
        priority=76,
    ),
    Category(
        # Q3665646 = basketball player. 544 entities.
        name="basketballer_tr",
        selector=f"?item wdt:P31 wd:Q5 ; wdt:P106 wd:Q3665646 ; wdt:P27 {TURKEY} .",
        min_sitelinks=2,
        priority=56,
    ),
    Category(
        # World footballers: `footballer_tr` only brought Turkish citizens, so
        # Messi/Ronaldo were NOT in the dictionary.
        #
        # ⚠️ The sitelink floor MUST be kept high: Q937857 is a class with
        # ~250k instances and even a COUNT with `?sl>=90` returns 502 on WDQS.
        # Measured: 80 → 115 entities / 7 s, 100 → 34 entities / 19 s,
        # 120 → 14 entities / 44 s. Do NOT lower the floor — the query cost
        # explodes and the category silently comes back empty.
        name="footballer_world",
        selector="?item wdt:P31 wd:Q5 ; wdt:P106 wd:Q937857 .",
        min_sitelinks=80,
        priority=53,
        surname_alias=True,
    ),
    # ── Media, institution, brand ────────────────────────────
    Category(
        # Q11032 = newspaper. Hurriyet/Milliyet/Sozcu — very common in text.
        name="newspaper_tr",
        selector=f"?item wdt:P31/wdt:P279* wd:Q11032 ; wdt:P17 {TURKEY} .",
        min_sitelinks=1,
        priority=86,
    ),
    Category(
        # Q2001305 = television channel.
        name="tv_channel_tr",
        selector=f"?item wdt:P31/wdt:P279* wd:Q2001305 ; wdt:P17 {TURKEY} .",
        min_sitelinks=1,
        priority=84,
    ),
    Category(
        # Q4830453 = business. Non-tech brands like THY/Migros/Ulker.
        # NO subclass walk: with `P31/P279*` WDQS returns 504 (measured).
        name="company_tr",
        selector=f"?item wdt:P31 wd:Q4830453 ; wdt:P17 {TURKEY} .",
        min_sitelinks=2,
        priority=83,
    ),
    Category(
        # Q3918 = university. Bogazici/ODTU/ITU.
        name="university_tr",
        selector=f"?item wdt:P31/wdt:P279* wd:Q3918 ; wdt:P17 {TURKEY} .",
        min_sitelinks=1,
        priority=68,
    ),
    # ── Works and music ──────────────────────────────────────
    Category(
        # Q7889 = video game.
        name="video_game",
        selector="?item wdt:P31/wdt:P279* wd:Q7889 .",
        min_sitelinks=45,
        priority=58,
    ),
    Category(
        # Q215380 = musical group (band). Since it is a group, not a person,
        # its priority is above the solo musician.
        name="band",
        selector="?item wdt:P31/wdt:P279* wd:Q215380 .",
        min_sitelinks=50,
        priority=57,
    ),
    Category(
        name="musician_world",
        selector="?item wdt:P31 wd:Q5 ; wdt:P106 wd:Q639669 .",
        min_sitelinks=90,
        priority=53,
        surname_alias=True,
    ),
    Category(
        # Q36180 = writer.
        name="writer_tr",
        selector=f"?item wdt:P31 wd:Q5 ; wdt:P106 wd:Q36180 ; wdt:P27 {TURKEY} .",
        min_sitelinks=3,
        priority=55,
    ),
    Category(
        # Q11424 = film. Its priority is the LOWEST: film titles are often
        # everyday words ("Nefes", "Ates", "Kayip") and when they collide with
        # another category the other must win. Precedent: "Ankara" (city, 60)
        # competes with "Ankara (film)" and the city wins.
        name="film",
        selector="?item wdt:P31/wdt:P279* wd:Q11424 .",
        min_sitelinks=60,
        priority=50,
    ),
]


def category_by_name(name: str) -> Category | None:
    for c in CATEGORIES:
        if c.name == name:
            return c
    return None
