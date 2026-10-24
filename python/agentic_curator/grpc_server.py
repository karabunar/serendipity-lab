"""PostCurator gRPC servicer — Quality Analyst bridge only.

Strategy 3 (Vector-Native Matching): category text/embedding is not
produced here (see the graph/pipeline.py docstring).

"""

from __future__ import annotations

import logging

import curator_pb2
import curator_pb2_grpc
from graph.pipeline import enrich_post

logger = logging.getLogger("serendipity.curator")


class PostCuratorServicer(curator_pb2_grpc.PostCuratorServicer):
    """The local LLM address and model name are validated AT STARTUP and
    injected via the constructor (main.load_llm_config); this class does NOT read env."""

    def __init__(self, *, base_url: str, model: str, api_key: str = "") -> None:
        self._base_url = base_url
        self._model = model
        self._api_key = api_key

    def HealthCheck(self, request, context):
        return curator_pb2.HealthCheckResponse(ready=True)

    def EnrichPosts(self, request, context):
        results = []
        for candidate in request.candidates:
            enriched = enrich_post(
                candidate.post_id, candidate.text, candidate.has_media,
                base_url=self._base_url, model=self._model, api_key=self._api_key,
            )
            results.append(
                curator_pb2.EnrichedPost(
                    post_id=enriched["post_id"],
                    quality_score=enriched["quality_score"],
                )
            )
        return curator_pb2.EnrichPostsResponse(results=results)
