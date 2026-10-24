"""LangGraph state schema — Quality Analyst only.

Strategy 3 (Vector-Native Matching): category text/embedding is NOT produced
HERE — niche matching is done on the Go side (curatedcontent.Fetcher) using
the post's ALREADY existing original embedding.

There is NO Fetcher node: the Go side (booster.go) has ALREADY read the content
via postmeta.Hydrate and carries it in the same text/has_media gRPC request —
fetching again in Python would be an unnecessary round-trip (spec § 3.2,
brainstorm decision).

"""

from __future__ import annotations

from typing import TypedDict


class CuratorState(TypedDict):
    post_id: str
    text: str
    has_media: bool
    llm_base_url: str
    llm_model: str
    llm_api_key: str
    quality_score: float
