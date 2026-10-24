"""Quality Analyst LangGraph node — turns a post text's discussion potential/
quality into a score between 0.0 and 1.0.

"""

from __future__ import annotations

import logging

from llm_client import generate_json

logger = logging.getLogger("serendipity.curator.quality_analyst")

_SYSTEM_INSTRUCTION = """Sen bir sosyal medya platformu için içerik kalite analistisin.
Sana bir gönderi metni verilecek. Bu gönderinin kalitesini ve tartışma/etkileşim
potansiyelini değerlendirip 0.0 (düşük kalite) ile 1.0 (yüksek kalite) arasında
bir skor ver.

Değerlendirme kriterleri:
- İçerik derinliği ve özgünlüğü
- Tartışmaya/yoruma açık olması
- Bilgilendirici veya ilgi çekici olması
- Spam, anlamsız veya çok kısa içerik düşük puan almalı"""

# guided_json expects STANDARD JSON Schema (lowercase types). Gemini's
# uppercase OBJECT/NUMBER form is rejected by vLLM.
_RESPONSE_SCHEMA = {
    "type": "object",
    "properties": {"quality_score": {"type": "number"}},
    "required": ["quality_score"],
}


def score_quality(text: str, *, base_url: str, model: str, api_key: str = "") -> float:
    """Converts post text into a quality score between 0.0 and 1.0. Empty text → 0.0
    (no LLM call is made). LLM error → 0.0 (fail-soft — booster.go's
    below-threshold elimination logic naturally leaves this post out of the ZSET).
    If the score is out of range it is clamped to [0.0, 1.0].
    """
    if not text:
        return 0.0
    try:
        result = generate_json(
            _SYSTEM_INSTRUCTION,
            text,
            _RESPONSE_SCHEMA,
            base_url=base_url,
            model=model,
            api_key=api_key,
        )
        score = float(result.get("quality_score", 0.0))
        return max(0.0, min(1.0, score))
    except Exception as exc:  # noqa: BLE001 — fail-soft boundary
        # %r (NOT %s): JSONDecodeError.__str__ does not include the exception type
        # ("Expecting value: line 1 column 1 (char 0)") — the G2 acceptance criterion
        # depends on the type being visible on this line (see llm_client.py docstring).
        logger.warning("quality-analyst LLM call failed, returning 0.0: %r", exc)
        return 0.0
