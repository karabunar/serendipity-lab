"""EmbeddingService gRPC server (embedding.proto)."""
from __future__ import annotations

import logging
from concurrent import futures

import grpc

import embedding_pb2
import embedding_pb2_grpc
from embedder import Embedder

logger = logging.getLogger("serendipity.embedding")

# Maximum number of texts handled in a single request; INVALID_ARGUMENT is returned if exceeded.
MAX_BATCH = 256
# gRPC message size cap (bytes).
MAX_MESSAGE_BYTES = 64 * 1024 * 1024


class EmbeddingServicer(embedding_pb2_grpc.EmbeddingServiceServicer):
    def __init__(self, embedder: Embedder, max_batch: int = MAX_BATCH) -> None:
        self._embedder = embedder
        self._max_batch = max_batch

    def Embed(self, request, context):
        if not self._embedder.ready:
            context.abort(grpc.StatusCode.UNAVAILABLE, "model not loaded yet")
        if len(request.texts) > self._max_batch:
            context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                f"at most {self._max_batch} texts allowed, got {len(request.texts)}",
            )
        try:
            vectors = self._embedder.encode(list(request.texts), int(request.kind))
        except Exception as exc:  # a model error goes to the client as INTERNAL
            logger.exception("embed error")
            context.abort(grpc.StatusCode.INTERNAL, f"embed failed: {exc}")
        return embedding_pb2.EmbedResponse(
            vectors=[embedding_pb2.Vector(values=v) for v in vectors],
            dim=self._embedder.dim,
            model=self._embedder.model_name,
        )

    def Health(self, request, context):
        return embedding_pb2.HealthResponse(
            ready=self._embedder.ready,
            model=self._embedder.model_name,
            dim=self._embedder.dim,
        )


def build_server(embedder: Embedder, port: int, workers: int = 4) -> tuple[grpc.Server, int]:
    """Builds the server (does not start it). If port=0 is given a free port is chosen; the returned value is the real port."""
    server = grpc.server(
        futures.ThreadPoolExecutor(max_workers=workers),
        options=[
            ("grpc.max_receive_message_length", MAX_MESSAGE_BYTES),
            ("grpc.max_send_message_length", MAX_MESSAGE_BYTES),
        ],
    )
    embedding_pb2_grpc.add_EmbeddingServiceServicer_to_server(EmbeddingServicer(embedder), server)
    bound = server.add_insecure_port(f"[::]:{port}")
    if bound == 0:
        raise RuntimeError(f"could not bind port: {port}")
    return server, bound
