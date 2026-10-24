"""Starts the service. Environment variables:

EMBEDDING_MODEL    model name (default intfloat/multilingual-e5-small)
EMBEDDING_DEVICE   cpu | cuda (default cpu)
EMBEDDING_PORT     port to listen on (default 50061)
EMBEDDING_BATCH    in-model batch size (default 32)

The model is loaded at startup; if it is not in the cache it is downloaded from HuggingFace.
"""
import logging
import os

from embedder import DEFAULT_MODEL, E5Embedder
from server import build_server


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
    log = logging.getLogger("serendipity.embedding")
    embedder = E5Embedder(
        model_name=os.getenv("EMBEDDING_MODEL", DEFAULT_MODEL),
        device=os.getenv("EMBEDDING_DEVICE", "cpu"),
        batch_size=int(os.getenv("EMBEDDING_BATCH", "32")),
    )
    log.info("loading model: %s", embedder.model_name)
    embedder.load()
    server, port = build_server(embedder, int(os.getenv("EMBEDDING_PORT", "50061")))
    server.start()
    log.info("embedding service listening on :%d (model=%s, dim=%d)", port, embedder.model_name, embedder.dim)
    server.wait_for_termination()


if __name__ == "__main__":
    main()
