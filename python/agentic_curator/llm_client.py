"""Local LLM client — sends chat-completion requests to a vLLM server running
on our own hardware. It NEVER goes to an external provider (human decision);
the address is the in-cluster `LOCAL_LLM_BASE_URL`.

The JSON guarantee comes from vLLM's guided decoding: the `guided_json` field
enforces the schema at grammar level, so the returned content is always valid
JSON matching the schema. It is the counterpart of Gemini's `responseSchema`
field and spares the caller extra validation/retry.

SAME responsibility boundary as `gemini_client.py`: a pure function that does
not read env and takes its parameters from the caller — so tests can run
without a network.
"""

from __future__ import annotations

import json

import httpx

DEFAULT_TIMEOUT = 30.0


def generate_json(
    system_instruction: str,
    user_text: str,
    response_schema: dict,
    *,
    base_url: str,
    model: str,
    api_key: str = "",
    timeout: float = DEFAULT_TIMEOUT,
    http_client: httpx.Client | None = None,
) -> dict:
    """Chat-completion call that forces the LLM to emit JSON matching the schema.

    base_url may be given with a trailing '/'; it is normalized.
    api_key is optional (`LOCAL_LLM_API_KEY`):
    if empty or whitespace-only, the `Authorization` header is not written at
    all — sending an empty Bearer header with an empty key yields 401, so it
    is deliberately skipped.
    Error policy: HTTP != 200 or empty `choices` → RuntimeError. If the
    content is not JSON, json.JSONDecodeError propagates — this signals the
    server was configured without guided decoding and must not be silently
    swallowed.
    """
    url = f"{base_url.rstrip('/')}/v1/chat/completions"
    body = {
        "model": model,
        "messages": [
            {"role": "system", "content": system_instruction},
            {"role": "user", "content": user_text},
        ],
        "temperature": 0.0,
        "max_tokens": 32,
        "guided_json": response_schema,
    }
    headers = {}
    api_key = api_key.strip()
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    if http_client is not None:
        resp = http_client.post(url, json=body, headers=headers)
    else:
        with httpx.Client(timeout=timeout) as client:
            resp = client.post(url, json=body, headers=headers)
    if resp.status_code != 200:
        raise RuntimeError(f"local llm http {resp.status_code}: {resp.text}")
    data = resp.json()
    choices = data.get("choices", [])
    if not choices:
        raise RuntimeError("local llm: empty choices in response")
    message = choices[0].get("message")
    if not isinstance(message, dict) or "content" not in message:
        raise RuntimeError("local llm: malformed response, missing choices[0].message.content")
    content = message["content"]
    return json.loads(content)
