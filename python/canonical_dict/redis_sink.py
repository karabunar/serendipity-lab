"""Writing the dictionary to Redis.

Keys:

    serendipity:canonical:alias   HASH    folded alias → canonical name
    serendipity:canonical:entity  HASH    canonical name → {"qid","category","sitelinks"} JSON
    serendipity:canonical:meta    STRING  build meta (generation time, counts)

**Why no TTL.** Keys without a TTL are usually a trap in a memory-limited Redis.
Here it is deliberate: the dictionary is a
managed artifact produced by an offline script and REWRITTEN regularly.
Adding a TTL would silently destroy the dictionary if the script missed a
single run, and the normalization layer would shut down without any warning.
Freshness is tracked via `:meta.generated_at`, not via TTL.

**Why an atomic swap.** Updating the live HASH in place with `HSET` means
serving a half-written dictionary during the write; also, removed aliases
would never be cleaned up. Instead, we write to `:build`-suffixed keys and
then swap them in a single Lua EVAL — a reader sees either the whole old
dictionary or the whole new one.

**Why Lua, not MULTI.** An empty HASH does not exist in Redis at all, and
`RENAME` on a missing key fails with "no such key". MULTI/EXEC does NOT stop
on this error: the remaining commands still run, leaving a half-state where
the alias was swapped but the entity was not. Lua does the `EXISTS` check in
the same atomic block as the swap and cleans up the empty side with `DEL`.
The same EXISTS-gate pattern guards the other Lua writes in this repo.

**Capacity note.** Memory is limited, so keys should be small and
expire when abandoned. Realistic figures: ~50k aliases ≈ **~5 MB** (alias HASH) +
~20k entity JSONs ≈ **~3.5 MB** (entity HASH). Since two generations coexist
during the swap, peak usage rises to **~17 MB**. It still fits, but the
`--max-aliases` cap is a safety valve against accidental bloat.

**`:build` keys have a TTL, live keys do NOT.** If the script crashed before
the swap, ~8.5 MB of orphaned `:build` data would stay around without a TTL;
so build keys are written with `_BUILD_TTL`. Because the swap uses `RENAME`
and RENAME CARRIES the TTL over, the Lua script calls `PERSIST` right after
the rename — otherwise the live dictionary would silently evaporate an hour
later.
"""

from __future__ import annotations

import json
import logging

KEY_ALIAS = "serendipity:canonical:alias"
KEY_ENTITY = "serendipity:canonical:entity"
KEY_META = "serendipity:canonical:meta"

_BUILD_SUFFIX = ":build"
_CHUNK = 2000

# Lifetime of the `:build` keys. The swap normally happens within seconds;
# this TTL exists only to avoid leaving orphaned data when the script
# crashed before the swap.
_BUILD_TTL = 3600

log = logging.getLogger(__name__)

# KEYS come in pairs: (build, live). If the build key exists, RENAME;
# otherwise (an empty HASH is never created) UNLINK the live key — so an
# empty dictionary is also published consistently and no half-swap occurs.
#
# ⚠️ RENAME CARRIES the source's TTL. `:build` keys are written with a TTL so
# they don't linger as orphans; without PERSIST the live dictionary would
# silently evaporate after _BUILD_TTL and the normalization layer would shut
# down without any warning.
#
# UNLINK (not DEL): deleting a 50k-field HASH with DEL blocks the server
# synchronously.
_SWAP_SCRIPT = """
for i = 1, #KEYS, 2 do
  if redis.call('EXISTS', KEYS[i]) == 1 then
    redis.call('RENAME', KEYS[i], KEYS[i + 1])
    redis.call('PERSIST', KEYS[i + 1])
  else
    redis.call('UNLINK', KEYS[i + 1])
  end
end
return 1
"""


def estimate_bytes(doc: dict) -> int:
    """Rough memory estimate (key + value, excluding Redis overhead)."""
    total = 0
    for k, v in doc["aliases"].items():
        total += len(k.encode()) + len(v.encode())
    for k, v in doc["entities"].items():
        total += len(k.encode()) + len(json.dumps(v, ensure_ascii=False).encode())
    return total


def read_live_meta(rdb) -> dict | None:
    """Returns the live `:meta`, or None if missing or unreadable.

    The shrink guard uses this and interprets None as "skip the check":
    on first install there is no meta and that run must not be rejected.
    """
    try:
        raw = rdb.get(KEY_META)
    except Exception as exc:  # noqa: BLE001 — guard, not the main path
        log.warning("could not read live meta, skipping shrink check: %s", exc)
        return None
    if not raw:
        return None
    try:
        doc = json.loads(raw)
    except (TypeError, ValueError):
        log.warning("live meta is not JSON, skipping shrink check")
        return None
    return doc if isinstance(doc, dict) else None


def write(rdb, doc: dict, dry_run: bool = False) -> None:
    """Writes the dictionary to the `:build` keys and swaps atomically.

    `rdb` is redis.Redis-like (fakeredis is used in tests).
    """
    alias_build = KEY_ALIAS + _BUILD_SUFFIX
    entity_build = KEY_ENTITY + _BUILD_SUFFIX
    meta_build = KEY_META + _BUILD_SUFFIX

    if dry_run:
        log.info(
            "dry-run: %d alias, %d entity, ~%d bytes — not written to Redis",
            len(doc["aliases"]),
            len(doc["entities"]),
            estimate_bytes(doc),
        )
        return

    # Clean up a previous half-finished build if any. UNLINK: deleting a
    # 50k-field HASH with DEL blocks the server synchronously.
    rdb.unlink(alias_build, entity_build, meta_build)

    _hset_chunked(rdb, alias_build, doc["aliases"])
    _hset_chunked(
        rdb,
        entity_build,
        {k: json.dumps(v, ensure_ascii=False, separators=(",", ":")) for k, v in doc["entities"].items()},
    )
    # The meta STRING is written with a TTL; PERSIST after RENAME removes it.
    rdb.set(meta_build, json.dumps(doc["meta"], ensure_ascii=False), ex=_BUILD_TTL)
    for key in (alias_build, entity_build):
        rdb.expire(key, _BUILD_TTL)  # no-op if the HASH is empty/missing

    # Atomic swap: a reader never sees a half dictionary.
    rdb.eval(
        _SWAP_SCRIPT,
        6,
        alias_build,
        KEY_ALIAS,
        entity_build,
        KEY_ENTITY,
        meta_build,
        KEY_META,
    )

    log.info(
        "redis: wrote %d aliases + %d entities (~%d bytes)",
        len(doc["aliases"]),
        len(doc["entities"]),
        estimate_bytes(doc),
    )


def _hset_chunked(rdb, key: str, mapping: dict[str, str]) -> None:
    """Chunked write instead of one giant HSET — a large command blocks Redis."""
    if not mapping:
        return
    items = list(mapping.items())
    for i in range(0, len(items), _CHUNK):
        rdb.hset(key, mapping=dict(items[i : i + _CHUNK]))
