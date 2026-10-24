# Wikidata canonical dictionary

Pipeline 2 matches entities against a **canonical dictionary built from Wikidata**. The build
is an offline script that queries the public Wikidata SPARQL endpoint (the one step that needs
the network and a contact address), so it is not part of the tests:

```bash
pip install -r python/canonical_dict/requirements.txt
export WIKIDATA_USER_AGENT="Serendipity/1.0 (you@example.com)"   # required: a reachable contact

python3 python/build_canonical_dict.py --category football_club_tr --dry-run -v   # try one category
python3 python/build_canonical_dict.py --out canonical_dict.json --report canonical_report.json
python3 python/validate_canonical_dict.py canonical_dict.json --report canonical_report.json
python3 python/build_canonical_dict.py --from-json canonical_dict.json --redis-url redis://localhost:6379/0
```

If you already have a Wikidata JSON dump (`latest-all.json.bz2`, `.gz`, or plain), build from it
instead: no network, no User-Agent, and the same dump always gives the same dictionary.

```bash
python3 python/build_canonical_dict.py --dump latest-all.json.bz2 --out canonical_dict.json --report canonical_report.json
```

The last command loads an existing JSON into Redis (`serendipity:canonical:*`) without touching
the network. Publishing swaps the whole dictionary, so the build refuses empty, truncated, or
suddenly much smaller results unless you pass the matching override flag; see
[`python/canonical_dict/README.md`](python/canonical_dict/README.md) for the exit codes.
Without a loaded dictionary, `postentity` writes nothing.

A dump build reads the file **twice** (first the subclass graph, then every item against every
category) and keeps only matching items in memory, so memory follows the dictionary size, not the
dump size. A full `latest-all.json.bz2` is hundreds of GB and takes hours to read. The reader is
covered by tests on a small synthetic dump in the real layout; it has not been run on a full dump,
so compare the per-category row counts in the report with a SPARQL build before trusting a new
dump. The category selectors are parsed rather than re-implemented, and an unsupported pattern
raises an error instead of silently matching nothing.
