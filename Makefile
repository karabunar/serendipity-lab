.PHONY: help build vet fmt fmt-check test test-go test-python check proto up down

PYTHON ?= python3

help:
	@echo "build        compile all Go packages"
	@echo "vet          go vet"
	@echo "fmt          format Go code"
	@echo "fmt-check    fail if Go code is not formatted"
	@echo "test         Go and Python tests (offline)"
	@echo "check        fmt-check + vet + test"
	@echo "proto        regenerate Go and Python gRPC stubs"
	@echo "up / down    start / stop Qdrant (docker compose)"

build:
	go build ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

test: test-go test-python

test-go:
	go test ./... -count=1

# canonical_dict, agentic_curator and ner_service run from python/ (see pytest.ini);
# the embedding service has its own conftest and skips when the model is not cached.
test-python:
	cd python && $(PYTHON) -I -m pytest -q
	cd python/embedding_service && $(PYTHON) -I -m pytest -q

check: fmt-check vet test

proto:
	protoc -I proto --go_out=. --go_opt=module=serendipity \
		--go-grpc_out=. --go-grpc_opt=module=serendipity proto/*.proto
	$(PYTHON) -m grpc_tools.protoc -I proto --python_out=python/embedding_service \
		--grpc_python_out=python/embedding_service proto/embedding.proto
	$(PYTHON) -m grpc_tools.protoc -I proto --python_out=python/ner_service \
		--grpc_python_out=python/ner_service proto/ner.proto
	$(PYTHON) -m grpc_tools.protoc -I proto --python_out=python/agentic_curator \
		--grpc_python_out=python/agentic_curator proto/curator.proto

up:
	docker compose up -d qdrant

down:
	docker compose down
