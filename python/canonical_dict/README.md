# Canonical Entity Dictionary (Wikidata)

Reduces the variants found in social media text to a single canonical name:

```
"FB", "Fener", "Sarı Lacivert"        → Fenerbahçe
"ChatGPT", "Sam Altman", "AçıkAI"     → OpenAI
"CHP", "Ana Muhalefet"                → Cumhuriyet Halk Partisi
```

This dictionary is the deterministic answer to the entity-normalization
inconsistency problem. It runs *in front of* any LLM normalization: a term that
is in the dictionary is never sent to the model.

**This is not a service.** It is an offline script, run by hand (or by a weekly
job). Its output lives in two places: `canonical_dict.json` and the
`serendipity:canonical:*` keys in Redis.

---

## Setup and usage

```bash
pip install -r python/canonical_dict/requirements.txt

# REQUIRED. Wikimedia's bot policy requires the User-Agent to carry a reachable
# contact address (e-mail or URL). There is NO default in the code: a made-up UA
# works until it is blocked, and then the block reads like a query error. The
# script stops without sending any request if the UA has no contact information.
export WIKIDATA_USER_AGENT="Serendipity/1.0 (team@example.com)"

# Write the file and the report; do not touch Redis
python python/build_canonical_dict.py \
    --out canonical_dict.json --report canonical_report.json

# Also write to Redis
python python/build_canonical_dict.py \
    --out canonical_dict.json --redis-url redis://localhost:6379/0

# Build from a local Wikidata JSON dump instead (no network, no User-Agent)
python python/build_canonical_dict.py \
    --dump latest-all.json.bz2 --out canonical_dict.json --report canonical_report.json

# Try a single category (QID check)
python python/build_canonical_dict.py --category football_club_tr --dry-run -v

# Load an existing JSON into Redis without touching the network
python python/build_canonical_dict.py \
    --from-json canonical_dict.json --redis-url redis://...
```

### Building from a dump (`dump.py`)

`--dump` swaps the WDQS client for `DumpClient`, which answers the same category
queries from a Wikidata entity dump (one JSON entity per line; `.bz2`, `.gz` or plain).
Everything after the fetch (collision rules, blocklist, overrides, gates, report) is the
unchanged pipeline.

- The category `selector` text is **parsed**, not re-implemented, so the two sources cannot
  drift. The supported grammar is exactly what `queries.CATEGORIES` uses (direct class,
  `P31/P279*` walk, `UNION`, `VALUES`, `FILTER NOT EXISTS`); anything else raises
  `DumpError` rather than silently matching nothing.
- Two passes over the dump: first the subclass graph, then every item is tested against every
  selected category; only matches are kept, so memory follows the dictionary size, not the
  dump size. Expect the dump to be read twice.
- WDQS semantics that are reproduced: `wdt:` truthiness (deprecated statements ignored,
  preferred rank wins), `wikibase:sitelinks` = number of `sitelinks` entries, one row per
  alias, the `min_sitelinks` floor, and `ORDER BY DESC(?sitelinks) ?item` paging.
- `--dump` and `--from-json` are mutually exclusive; `--category` works with both sources.
- The tests (`tests/test_dump.py`) run on a tiny synthetic dump in the real layout. The
  reader has not been run on a full Wikidata dump yet; expect to check the per-category row
  counts in the report against a WDQS build before trusting a new dump.

### Publish gates (exit codes)

Writing the dictionary to Redis means swapping the **whole** live dictionary.
Publishing a partial or broken pull would silently switch the normalization
layer off. So publishing goes through several gates:

| Code | Condition | Override flag |
|---|---|---|
| `2` | Unknown `--category` name | n/a |
| `3` | Alias count exceeded the cap | `--max-aliases N` |
| `4` | **No Wikidata-sourced aliases.** The gate looks at `pre_override_alias_count`: `overrides.json` adds ~30 entries, so the total is never 0 and the old gate could never fire | `--allow-empty` |
| `5` | At least one category returned **0 rows** (the class QID may have changed) | `--allow-empty-categories` |
| `6` | At least one category hit the `LIMIT` cap (result truncated) | `--allow-truncated-categories` |
| `7` | The new dictionary is **more than 25%** smaller than the live one (partial query-service failure) | `--force` |
| `8` | `--category` + `--redis-url`: a partial dictionary would replace the whole one | `--partial-ok` |

`5` and `6` are **separate** flags on purpose: the two failures need different
fixes (one is a QID correction, the other is splitting the query), and an
operator who knowingly accepts a truncated category should not also silence a
newly broken QID.

`7` (the shrink guard) reads the live `:meta`; if it is missing or unreadable
the check is skipped (first install). The reader, `canonicaldict.RefreshOnce`,
keeps the old snapshot **only on error**, not on shrinkage; this gate closes
that gap.

Test:

```bash
cd python && python3 -I -m pytest -q
```

---

## Output

### `canonical_dict.json`

```json
{
  "meta":     { "generated_at": "...", "alias_count": 48213, "entity_count": 19044 },
  "entities": { "Fenerbahçe": { "qid": "Q13214", "category": "football_club_tr", "sitelinks": 42 } },
  "aliases":  { "fenerbahce": "Fenerbahçe", "fener": "Fenerbahçe", "fb": "Fenerbahçe" }
}
```

### Redis

| Key | Type | Content |
|---|---|---|
| `serendipity:canonical:alias` | HASH | folded alias → canonical name |
| `serendipity:canonical:entity` | HASH | canonical name → `{qid, category, sitelinks}` JSON |
| `serendipity:canonical:meta` | STRING | build metadata |

**No TTL**, on purpose. The dictionary is a managed artifact; a TTL would
silently switch the normalization layer off the day the script misses a run.
Freshness is tracked through `:meta.generated_at`.

**Writes are atomic.** Data is written first to keys with a `:build` suffix,
then swapped in a single Lua `EVAL`. MULTI/EXEC is not used: an empty HASH does
not exist in Redis at all, `RENAME` on a missing key errors, and MULTI does not
stop at that error, which would leave a half state (alias swapped, entity not).
The Lua script does the `EXISTS` check in the same block as the swap.

**The `:build` keys DO have a TTL** (1 hour), so that a crash before the swap
does not leave ~8.5 MB of orphaned data. `RENAME` carries the TTL over, so the
Lua script calls `PERSIST` right after the rename; otherwise the live
dictionary would silently evaporate an hour later.

**Size.** ~50k aliases ≈ **~5 MB** (alias HASH) + ~20k entity JSON documents
≈ **~3.5 MB** (entity HASH). Both generations coexist during the swap, so peak
usage is about **~17 MB**. `--max-aliases` (default 200k) is the valve against
accidental bloat.

---

## ⚠️ Normalization contract

Keys are produced by `fold.py`; **the lookup on the Go side must use the same
folding**. The single owner on the Go side is `internal/trfold`
(`canonicaldict.Fold` and `postentity.fold` delegate to it). If the two sides
diverge, lookups miss **silently**: the key exists in Redis, Go never finds it,
and there is no error or metric; it just looks like "the dictionary never
matches".

Both sides are locked by `testdata/fold_golden.json`. If you change the
folding, change all three together: `fold.py`, `trfold/fold.go`, and the golden
fixture, and run the Python and Go test suites together (the Go side alone can
stay green).

**The one known divergence:** the Greek final sigma. Go maps `"ΑΣ"` to `ασ`,
Python to `ας`. It was deliberately left out of the fixture (it would break the
Python test); entity names come from Turkish and English labels, so it has no
practical effect. If Greek aliases ever enter the dictionary, fix this first.

Order is part of the contract too: **accent folding first, then lowercase.** In
the opposite order Python produces a combined dotted `i̇` (U+0069 U+0307) for
`"İ"`, which diverges from Go's `strings.ToLower`.

---

## Categories and conflict policy

`queries.py` defines 17 categories: political parties, football clubs, tech
companies/software/platforms, countries/cities/districts, Turkish politicians/
footballers/musicians/actors, world leaders, TV series.

Each category has a **priority** and a **sitelink floor**. The sitelink count is
a proxy for popularity: the floor is low for Turkey-focused categories (we want
coverage) and high for world-wide ones (otherwise the dictionary fills with
noise).

When the same alias points to two entities ("Fenerbahçe" = club ∨ district):

1. The higher priority wins.
2. If priorities are equal, the one with at least **3×** the sitelinks wins.
3. Otherwise the alias is **dropped** and written to the report.

Silently picking one would produce wrong personalization; keeping both would
break the `alias → single canonical` contract.

### Query-service robot policy (403)

The Wikidata query service treats back-to-back queries under its robot policy.
Pulling 17 categories without waiting produces a **403**. There are two
protections: the mandatory contact UA, and a `politeness_delay` between queries
(default 2 s). The 403 message points at both, because otherwise a 403 reads
like a query error.

### A category that returns 0 rows = a wrong QID

A class QID that does not exist in Wikidata does **not** raise an error; it
silently returns an empty result. The script logs this as a WARNING and writes
it to the report under `empty_categories`. If you see such a row, verify that
category's QID by hand.

`limit_hit_categories` says the result reached `LIMIT`, that is, it was
truncated; split that category into narrower queries. In that case publishing
is **refused** (exit code `6`).

### Paging and `ORDER BY`

The template produces an (item × altLabel) product, so `politician_world`,
`city_world`, `software` and `football_club_world` realistically exceed one
page. The pull is paged with `ORDER BY DESC(?sitelinks) ?item` + `OFFSET`
(page size `queries.PAGE_SIZE`, default 10000), until a short page arrives or
the `Category.limit` cap is reached.

`ORDER BY` is a contract, not decoration: SPARQL does not **guarantee** order,
and with an unordered `LIMIT`/`OFFSET` the set of surviving entities changes
from run to run; worse, an included entity may arrive with an incomplete alias
set. The politeness delay also applies between pages (`SPARQLClient.select`
applies it on every consecutive request).

---

## Manual maintenance

| File | Purpose |
|---|---|
| `overrides.json` | Jargon that Wikidata lacks ("Sarı Lacivert", "Ana Muhalefet", "Cimbom") and hand fixes for wrongly resolved conflicts. **Always wins.** |
| `blocklist.txt` | Everyday words that must not be accepted as aliases ("spor", "takım", "yeni"). They appear in every text, so they would attach a wrong entity to every post and silently kill precision. |
| `district_bare_allow.txt` | Bare district names allowed into the dictionary once the "ilçesi" suffix is stripped ("Bafra ilçesi" → `bafra`). **It is an allowlist:** no blanket stripping, because some district names are common nouns or day names (`alaca`, `çay`, `çarşamba`, `kale`). A bare name goes through the blocklist and ambiguity rules the normal way; a name that is on the list but yields no match produces a WARNING at build time. |

If a mapping in `overrides.json` overrides one coming from Wikidata, it is
written to the report under `override_conflicts`: no silent overriding.

An override's canonical name does not have to exist in Wikidata; local jargon is
a valid target (it goes only into `aliases`, not into `entities`).

### Overrides are SUBJECT to the blocklist

`apply_overrides` checks every key against the blocklist; a key on the blocklist
is **rejected** and written to the report under `rejected_overrides`. For a
deliberate exception, the value can be written in dictionary form:

```json
{ "ts": { "value": "Trabzonspor", "force": true } }
```

> ⚠️ `MIN_ALIAS_LEN` was raised from 2 to **3** (for aliases derived from
> Wikidata). On a sample of real posts, two-letter surfaces produced
> many matches that an independent NER did not support; the top of
> the list was entirely ISO 3166 country codes (`na`→Namibia,
> `tl`→Timor-Leste, `kg`→Kyrgyzstan). The abbreviations the
> threshold was meant to protect were dead in the same corpus (`fb`, `gs`,
> `ak` almost never matched). Curated `overrides.json` entries are exempt through
> `MIN_OVERRIDE_ALIAS_LEN=2`; the scan floor on the Go side
> (`canonicaldict.minAliasRunes`) therefore stays at 2.

Why: the rationale for the substring-grounding decision was that "the blocklist
plus `MIN_ALIAS_LEN` already reduce the risk". If overrides were exempt from
both, that rationale would be empty, and indeed `"aslan" → Galatasaray` (a
common noun and a very common surname), `"fener" → Fenerbahçe` (fener = lamp;
also a neighborhood in Fatih), `"gemini" → Google` (a zodiac sign) and
`"bard" → Google` came in through exactly that door. All four were **removed**
from `overrides.json`.

**Abbreviations that were kept, and why** (JSON does not allow comments, so
they are listed here):

| Key | Why it was kept |
|---|---|
| `ts` | The established abbreviation of Trabzonspor. In Turkish text a standalone "ts" is not a common noun; the risk is low and the coverage loss would be high |
| `gpt` | Has no everyday Turkish meaning; it almost always refers to the model |
| `fb`, `gs`, `bjk` | Club abbreviations, not common nouns |
| `bist`, `tcmb`, `chp`, `akp`, `mhp` | Institution abbreviations |

Before adding a new single-word **common-noun** override, ask whether it belongs
on the blocklist instead; `force: true` is an escape hatch, not the default.
