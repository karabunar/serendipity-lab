"""Gemini Developer API REST client. It speaks the plain REST contract
(`batchEmbedContents`, `generateContent`). The SDK (google-genai) is NOT
used — for the repo's few-dependencies philosophy and to keep the wire format
easy to verify.

"""

from __future__ import annotations

import json

import httpx

DEFAULT_BASE_URL = "https://generativelanguage.googleapis.com"
DEFAULT_TIMEOUT = 10.0


def embed_text(
    text: str,
    *,
    api_key: str,
    model: str = "gemini-embedding-001",
    output_dim: int = 3072,
    task_type: str = "SEMANTIC_SIMILARITY",
    base_url: str = DEFAULT_BASE_URL,
    timeout: float = DEFAULT_TIMEOUT,
    http_client: httpx.Client | None = None,
) -> list[float]:
    """Embeds text (batchEmbedContents, single request). Model and dimension
    (gemini-embedding-001, 3072-dim) must match those of the vectors it is
    compared with, otherwise cosine similarity is computed in two different
    bases.
    """
    url = f"{base_url}/v1beta/models/{model}:batchEmbedContents"
    body = {
        "requests": [
            {
                "model": f"models/{model}",
                "content": {"parts": [{"text": text}]},
                "taskType": task_type,
                "outputDimensionality": output_dim,
            }
        ]
    }
    headers = {"x-goog-api-key": api_key, "Content-Type": "application/json"}
    if http_client is not None:
        resp = http_client.post(url, json=body, headers=headers)
    else:
        with httpx.Client(timeout=timeout) as client:
            resp = client.post(url, json=body, headers=headers)
    if resp.status_code != 200:
        raise RuntimeError(f"gemini embed http {resp.status_code}: {resp.text}")
    data = resp.json()
    if "error" in data:
        raise RuntimeError(f"gemini embed api error: {data['error']}")
    embeddings = data.get("embeddings", [])
    if not embeddings:
        raise RuntimeError("gemini embed: empty embeddings in response")
    return embeddings[0]["values"]


def generate_json(
    system_instruction: str,
    user_text: str,
    response_schema: dict,
    *,
    api_key: str,
    model: str = "gemini-2.5-flash-lite",
    base_url: str = DEFAULT_BASE_URL,
    timeout: float = DEFAULT_TIMEOUT,
    http_client: httpx.Client | None = None,
) -> dict:
    """generateContent call that forces the LLM to emit structured JSON.
    JSON is forced via responseSchema instead of free text + validate.
    """
    url = f"{base_url}/v1beta/models/{model}:generateContent"
    body = {
        "systemInstruction": {"parts": [{"text": system_instruction}]},
        "contents": [{"role": "user", "parts": [{"text": user_text}]}],
        "generationConfig": {
            "responseMimeType": "application/json",
            "responseSchema": response_schema,
        },
    }
    headers = {"x-goog-api-key": api_key, "Content-Type": "application/json"}
    if http_client is not None:
        resp = http_client.post(url, json=body, headers=headers)
    else:
        with httpx.Client(timeout=timeout) as client:
            resp = client.post(url, json=body, headers=headers)
    if resp.status_code != 200:
        raise RuntimeError(f"gemini generateContent http {resp.status_code}: {resp.text}")
    data = resp.json()
    candidates = data.get("candidates", [])
    if not candidates:
        raise RuntimeError("gemini generateContent: empty candidates in response")
    parts = candidates[0]["content"]["parts"]
    if not parts:
        raise RuntimeError("gemini generateContent: empty parts in response")
    text = parts[0]["text"]
    return json.loads(text)
