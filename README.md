# serendipity-lab

**Help a social feed surprise its users with posts they never asked for, but will care about.**

A feed built only from the people you follow is a closed loop: it can never show you anything
new. *Serendipity* is the opposite, an unexpected find that still turns out to be relevant.
This repository explores two ways to engineer it without a heavy ranking model:

- **Curated post-boost** resurfaces proven older posts at fixed feed positions, but only when
  they match the user's niche (a topic the user keeps coming back to).
- **Entity-driven discovery** recommends by the *named things* a user cares about (a football
  club, a city, a technology) through an inverted index, so every candidate has an explainable reason.

Around them sit the supporting pieces both need: an embedding service, a Qdrant adapter,
a per-user interaction log, and a synthetic data generator.

It is a sandbox to **read and tinker with**, not a service to deploy. Every component has a
narrow interface and a thin `main.go` showing how it would be wired. The test suite runs offline:
no Docker, no GPU, no network, and no model download (tests that need a model skip when it is not cached).

## How it works

### 1. Curated post-boost

Pre-compute a pool of high-quality, older posts; at request time, inject one or a few of
them at fixed feed positions, but only if they match the user's niche.

```
authors' recent posts ──► curated-booster ──► serendipity:curated:boosted (ZSET, score = author trust × content signal)
                          (optional LLM quality score via gRPC)
                                                         │
user's recent interactions ──► niche-profiler ──► serendipity:user:niche:{uid}   (up to 3 niches, each an embedding)
   (LLM names ≤3 niches, embedding service                │
    turns each into a vector)                             ▼
                                              content.Fetcher: read top-N from the pool, keep only
                                              candidates whose post vector is cosine-close to a niche
                                                         │
                                                         ▼
                                              CuratedInjector: insert at fixed positions, skip already-seen
```

The pool is computed ahead of time, so the request path only reads Redis (plus one batched
vector read from the vector database).

### 2. Entity-driven discovery

Treat named things ("Kapadokya", "Redis", "Yeşilçam") as first-class signals instead of
embedding everything.

```
post text ──► postentity (dictionary match, optional NER)  ──► entities stored on the post
                │ canonical dictionary built from Wikidata
                ▼
        entityindex: entity ──► recent posts (inverted index, self-pruning window)

user interactions ──► m04interest: weighted entity profile (stable + recent half-lives,
                       negatives from skips, optional author-graph prior)
                                   │
                                   ▼
        entityslot: query the index with the user's top entities (IDF-scaled so that very common
        entities don't swamp personalization), distribute a candidate quota across entities
```

It needs no vector search at all, which makes it cheap, explainable (every candidate has a
named reason), and a good counterpart to the embedding-based pipeline.

## Example

Real output from a local run (`synth-data -users 20 -seed 1`, a local Redis, Qdrant and the
default e5 model). The data is synthetic, so the posts are template sentences, but every number
below was produced by the code in this repository. The synthetic generator knows each user's true
topics, which is what lets us score the results.

**Entity discovery** (pipeline 2). `user-interest` turned each user's interaction log into a
weighted entity profile; `entityslot` then asked the index for candidates:

```
u003 profile:  kubernetes=18.97  qdrant=14.04  redis=12.39  go=5.08
  p0022 by a02  "Kubernetes ve Redis hakkında ölçek ve sunucu: veritabanı üzerine kısa bir not."
  p0016 by a02  "Kubernetes hakkında dağıtım ve sunucu: veritabanı üzerine kısa bir not."
  p0106 by a08  "Kubernetes hakkında ölçek ve kod: veritabanı üzerine kısa bir not."
  ...

u006 profile:  baklava=15.60  iskender=9.80  manti=8.04  lahmacun=6.11
  p0080 by a06  "iskender ve baklava hakkında restoran ve lezzet: sofra üzerine kısa bir not."
  p0079 by a06  "baklava ve lahmacun hakkında mutfak ve restoran: tarif üzerine kısa bir not."
  ...
```

Every candidate carries the entity that produced it, so "why am I seeing this" always has an
answer. The profile keys are folded (`manti`, not `mantı`). The profile also lists weak, one-off
interests (u003 has `antalya`); entities far below the user's strongest one are not queried, so
they no longer put off-topic posts into the result.

**Curated post-boost** (pipeline 1). `niche-profiler` asked a local LLM (Qwen2.5-1.5B-Instruct)
for each user's niches: 50 of the 55 categories it produced were on a topic the user really
prefers. The fetcher then kept only pool posts whose stored vector is close to a niche. Pool of
126 posts, 20 users, up to 20 candidates each:

```
cut                                   candidates/user   users with any   on-topic share
fixed cosine 0.80                           6.0              16/20            0.54
fixed cosine 0.86                           0.9               9/20            0.83
fixed cosine 0.90                           0.0               0/20             n/a
within 0.04 of the user's best (default)   16.9              20/20            0.82
```

Embedding similarities sit in a narrow high band, so any fixed cut is too loose for some users and
too strict for others. Cutting relative to each user's own best match keeps the precision of the
good fixed cut and gives every user candidates. The injector then places one at the configured
position (`[7]`): the curated post becomes the 7th item of an otherwise organic feed.

## Tech stack

| Layer | What is used |
|---|---|
| Core jobs and libraries | **Go 1.25** (module `serendipity`). Dependencies: `go-redis/v9`, `grpc` 1.80 and `protobuf` 1.36, `prometheus/client_golang`. Tests use `miniredis`, so the Go suite needs no Redis. |
| Python services | **Python 3.13**: `grpcio` 1.68 + `protobuf` (gRPC servers), `sentence-transformers` (embeddings), `spaCy` 3.8 with `tr_core_news_lg` (NER), `LangGraph` (curator graph), `httpx`, `redis`. Tests: `pytest`, `fakeredis`. |
| Storage | **Redis** protocol (Valkey works) for every key under `serendipity:`; **Qdrant** v1.12 (REST) for post vectors, started from `docker-compose.yml`. |
| Models | Embeddings: `intfloat/multilingual-e5-small` (384-d), loaded lazily from the Hugging Face cache. LLM: any OpenAI-compatible `/v1/chat/completions` server, built for **vLLM** `guided_json` (`LOCAL_LLM_BASE_URL`, `LOCAL_LLM_MODEL`). The curator can also call the Gemini REST API. NER: spaCy `tr_core_news_lg`. |
| Knowledge source | **Wikidata**: the public SPARQL endpoint, or a local JSON dump (`--dump`). |
| Contracts and tooling | gRPC + Protocol Buffers (`proto/`, generated with `protoc` 3.21 and `grpc_tools`), Prometheus metrics on `:2112`, Docker Compose for Qdrant. |

## Quick start

```bash
make test     # Go and Python tests, offline
make check    # gofmt + go vet + tests
make help     # all targets
```

Requirements: Go 1.25 and Python 3.13. Redis, Qdrant and the models are needed only to run the
jobs, not the tests.

```bash
go run ./cmd/synth-data -users 20 -seed 1                  # synthetic posts, users, interactions (needs a local Redis)
USER_INTEREST_ENABLED=true go run ./cmd/user-interest       # entity-interest profiles
```

The embedding-based pipeline additionally needs the embedding service, Qdrant and an
OpenAI-compatible LLM server; [`docs/getting-started.md`](docs/getting-started.md) walks through it.

## Documentation

| Document | Contents |
|---|---|
| [`docs/getting-started.md`](docs/getting-started.md) | Running the pipelines end to end, regenerating gRPC stubs, loading your own interaction data. |
| [`docs/wikidata-dictionary.md`](docs/wikidata-dictionary.md) | Building the canonical entity dictionary from the SPARQL endpoint or a local dump. |
| [`docs/configuration.md`](docs/configuration.md) | Environment variables for every job and service. |
| [`docs/architecture.md`](docs/architecture.md) | Building blocks and design notes. |
| [`python/canonical_dict/README.md`](python/canonical_dict/README.md) | Dictionary build rules, publish gates and exit codes. |

## Repository layout

```
cmd/        thin entry points (curated-booster, niche-profiler, user-interest, entity-link-builder,
            post-indexer, interaction-seed, synth-data); every job is off unless its *_ENABLED is true
internal/   curated/ (pipeline 1) · postentity, entitylink, entityindex, entityslot, m04interest,
            canonicaldict, trfold (pipeline 2) · embedclient, vectordb, postindex · interactionlog,
            synth · core, postmeta and other shared pieces
python/     embedding_service, ner_service, agentic_curator, canonical_dict
proto/      embedding.proto, ner.proto, curator.proto
docs/       guides
```

## Status

The unit tests cover every component and run offline. Beyond them, the pipelines have been run
end to end on a laptop against a local Redis, Qdrant, the default e5 model and a small local
LLM, using the synthetic data generator; the numbers in the example above come from that run.

A few paths are still waiting for broader validation, and are worth a quick check before you rely
on them:

- **Real data.** Synthetic posts are template sentences, so real-world precision will differ.
- **vLLM.** The niche profiler is written for vLLM's `guided_json`; it has been tried against a
  local OpenAI-compatible server, and a run against vLLM itself is still to come.
- **Wikidata dump reader.** It is tested on a small synthetic dump in the real layout; a run on a
  full dump, compared with a SPARQL build, is still to come.

## Limitations

- This is an experiment, not a service: there is no ranking chain, feed assembly, auth, or
  deployment tooling here, only the retrieval and profiling pieces.
- The interaction log is capped (200 events per user, 30-day TTL), so skip counts and
  author affinity reflect that window only. Event weights (`interactionlog.Weight`) are a fixed
  table; tune them for your product.
- The default embedder downloads its model on first service start; plan for the download size.

## Language

Code, comments and docs are in English. The canonical dictionary and the test fixtures stay in
**Turkish**, because the entity pipeline is built for Turkish text (diacritic folding, suffix
handling, Turkish named entities).

## License

[MIT](LICENSE)
