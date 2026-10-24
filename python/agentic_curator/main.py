"""Entry point for the Post Curator gRPC service (Quality Analyst)."""

from __future__ import annotations

import logging
import os
from concurrent import futures

import grpc

import curator_pb2_grpc
from grpc_server import PostCuratorServicer

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
)
logger = logging.getLogger("serendipity.curator")


def load_llm_config() -> tuple[str, str, str]:
    """Reads the local LLM address, model name and (optional) API key from env.

    Base URL and model are REQUIRED, with no code default: a wrong/missing
    model name makes the server return 404, and if read per request this
    would only surface on the first traffic. The repo's established pattern
    is fail-fast at startup (like the EMBEDDING_DIM dimension check).

    The API key is OPTIONAL, `LOCAL_LLM_API_KEY`: empty/unset is valid, and no error is raised if the server
    runs without a key.
    """
    base_url = os.getenv("LOCAL_LLM_BASE_URL", "").strip()
    model = os.getenv("LOCAL_LLM_MODEL", "").strip()
    api_key = os.getenv("LOCAL_LLM_API_KEY", "").strip()
    missing = [
        name
        for name, value in (("LOCAL_LLM_BASE_URL", base_url), ("LOCAL_LLM_MODEL", model))
        if not value
    ]
    if missing:
        raise RuntimeError(
            "local LLM configuration missing: "
            + ", ".join(missing)
            + " (no code default; LOCAL_LLM_MODEL is vLLM's --served-model-name value)"
        )
    return base_url, model, api_key


def serve() -> None:
    base_url, model, api_key = load_llm_config()
    port = int(os.getenv("GRPC_PORT", "50061"))
    max_workers = int(os.getenv("MAX_WORKERS", "4"))

    server = grpc.server(futures.ThreadPoolExecutor(max_workers=max_workers))
    curator_pb2_grpc.add_PostCuratorServicer_to_server(
        PostCuratorServicer(base_url=base_url, model=model, api_key=api_key), server
    )
    server.add_insecure_port(f"[::]:{port}")
    server.start()
    logger.info(
        "agentic-curator gRPC server listening on :%d (llm=%s, model=%s)",
        port, base_url, model,
    )
    server.wait_for_termination()


if __name__ == "__main__":
    serve()
