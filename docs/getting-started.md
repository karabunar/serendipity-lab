# Getting started

Requirements: Go 1.25, Python 3.13 for the Python parts, `protoc` only if you regenerate stubs.
Redis is needed only to run the jobs, not the tests.

```bash
make test        # Go + Python, offline
make check       # gofmt check + go vet + tests
```

`make test` runs `go test ./...` and the Python suites. The embedding service tests use the real
model and skip when it is not in the local Hugging Face cache (they never download it); the NER
tests that need spaCy skip when it is not installed.

Regenerate the gRPC stubs after editing a `.proto` file:

```bash
protoc -I proto --go_out=. --go_opt=module=serendipity --go-grpc_out=. --go-grpc_opt=module=serendipity proto/*.proto
python3 -m grpc_tools.protoc -I proto --python_out=python/<service> --grpc_python_out=python/<service> proto/<name>.proto
```

Try the pipelines end to end against a local Redis:

```bash
go run ./cmd/synth-data -users 20 -seed 1                  # write synthetic posts, users, interactions
USER_INTEREST_ENABLED=true go run ./cmd/user-interest       # build entity-interest profiles

# Pipeline 1 additionally needs the embedding service, Qdrant and a local LLM:
docker compose up -d qdrant                                 # optional, see docker-compose.yml
(cd python/embedding_service && pip install -r requirements.txt && python main.py)   # downloads the model on first start
POST_INDEXER_ENABLED=true go run ./cmd/post-indexer
NICHE_PROFILER_ENABLED=true LOCAL_LLM_BASE_URL=http://localhost:8000 LOCAL_LLM_MODEL=<model> go run ./cmd/niche-profiler
CURATED_MIN_AGE=1h CURATED_QUALITY_THRESHOLD=0 go run ./cmd/curated-booster   # fill the curated pool (synthetic posts are recent)
```

The niche profiler talks to any OpenAI-compatible `/v1/chat/completions` server (vLLM, for
example). It sends vLLM's `guided_json` field to force a JSON answer; servers that ignore the
field may return free text, which the client reports as an error instead of guessing.

To use your own data instead of synthetic data, pipe JSONL events into `interaction-seed`:

```bash
echo '{"user_id":"u1","post_id":"p1","author_id":"a1","type":"like"}' | go run ./cmd/interaction-seed
```

`type` is one of `like`, `reply`, `reblog`, `dwell`, `image_view`, `detail_view`, `video_watch`,
`video_completion`, `skip`. Optional fields: `value`, `dwell_ms`, `timestamp` (RFC 3339).
Invalid lines are skipped and counted; the exit code is 2 if any were invalid.

The entity pipeline needs a canonical dictionary; see
[`wikidata-dictionary.md`](wikidata-dictionary.md) for building one.
