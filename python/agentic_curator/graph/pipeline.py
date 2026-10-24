"""Compiled LangGraph graph — Quality Analyst only.

Strategy 3 (Vector-Native Matching): there is NO Auto-Tagger node — instead of
generating category text and embedding it separately (~90% extra LLM cost),
niche matching is done on the Go side (curatedcontent.Fetcher) using the
post's ALREADY existing original embedding (from the post vector store). Even though the graph has a single node, the LangGraph
skeleton is kept — so another independent enrichment node can be added later.

"""

from __future__ import annotations

from langgraph.graph import END, START, StateGraph

from graph.quality_analyst import score_quality
from graph.state import CuratorState


def _quality_analyst_node(state: CuratorState) -> dict:
    quality_score = score_quality(
        state["text"],
        base_url=state["llm_base_url"],
        model=state["llm_model"],
        api_key=state["llm_api_key"],
    )
    return {"quality_score": quality_score}


def build_graph():
    graph = StateGraph(CuratorState)
    graph.add_node("quality_analyst", _quality_analyst_node)
    graph.add_edge(START, "quality_analyst")
    graph.add_edge("quality_analyst", END)
    return graph.compile()


_COMPILED_GRAPH = build_graph()


def enrich_post(
    post_id: str, text: str, has_media: bool, *, base_url: str, model: str, api_key: str = ""
) -> dict:
    """Runs a single post through the Quality Analyst.
    Returns {"post_id", "quality_score"} — does NOT carry category/embedding
    (Strategy 3, see this file's top docstring).
    """
    result = _COMPILED_GRAPH.invoke(
        {"post_id": post_id, "text": text, "has_media": has_media,
         "llm_base_url": base_url, "llm_model": model, "llm_api_key": api_key,
         "quality_score": 0.0}
    )
    return {
        "post_id": post_id,
        "quality_score": result["quality_score"],
    }
