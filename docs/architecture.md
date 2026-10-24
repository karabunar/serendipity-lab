# Architecture

## Building blocks

| Block | What it is |
|---|---|
| `python/embedding_service` | gRPC embedding service. Default model: multilingual `intfloat/multilingual-e5-small` (384-d, Turkish-capable); e5 query/passage prefixes are added automatically. The model is loaded by `load()` at service start, **not** at import, so nothing is downloaded by reading or installing the code. |
| `internal/embedclient` | Go client and `Embedder` interface for it. |
| `internal/vectordb` | `VectorDB` interface and a Qdrant REST adapter (stdlib `net/http`, no extra dependency). String IDs are mapped to UUIDv5; the original ID is kept in the payload. |
| `internal/postindex` | `Indexer` (embed posts and upsert), `Similar` (embed a query and search), `VectorSource` (feeds the curated fetcher). |
| `internal/interactionlog` | The single source of user behavior: a capped per-user Redis list with one writer and four derived readers (recent events, recent post IDs, skip analytics, favorite authors). |
| `internal/synth` | Deterministic synthetic data: topical authors and posts with entities, users with topic preferences, matching interactions, F2 author sets. |
| `python/canonical_dict`, `internal/canonicaldict`, `internal/trfold` | Wikidata-based canonical entity dictionary (build and read) and the Turkish folding rule shared by Go and Python (locked by a golden file). |
| `python/ner_service` | spaCy NER gRPC service for the hybrid extractor. |
| `python/agentic_curator` | LangGraph service that scores post quality with a local LLM. |


## Design notes

- **Interfaces at every seam.** Readers depend on small interfaces (`Embedder`, `VectorDB`,
  `IPostVectorSource`, `RingReader`, `Categorizer`, …), so the Redis, Qdrant, gRPC and LLM
  implementations can each be swapped for a different backend.
- **Fail-soft on the request path.** A missing profile, an empty pool, or an unreachable
  dependency yields "no candidates", never an error that would break the feed. Batch jobs
  isolate per-user failures and keep the previous profile when a refresh fails.
- **One behavior log feeds everything.** Positive signals, negative signals (skips), favorite
  authors and niche inputs are all derived from `interactionlog`, so they cannot disagree.
- **Relative cuts over fixed thresholds.** Embedding cosines cluster in a narrow band, so the
  curated fetcher cuts relative to the user's best match (`RelativeMargin`), scans a wider part
  of the pool before filtering (`ScanLimit`) and breaks ties by similarity. The entity slot
  skips entities far below the user's strongest one (`MinRelativeWeight`) and softly limits one
  author's share of the result (`MaxAuthorShare`). All four are config fields with defaults, and a
  negative value turns each one off.
- **Contracts are tested at the boundary.** Writer/reader format agreements are locked by tests
  (for example, the niche profiler's output must be readable by the curated fetcher, and the Go
  and Python folding rules must agree on a shared golden file).

