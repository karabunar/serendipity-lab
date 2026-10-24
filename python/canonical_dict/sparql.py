"""Wikidata Query Service (WDQS) client.

Two operational facts shape this file:

1. **A User-Agent is required.** Wikimedia blocks library-default UAs (e.g.
   `python-urllib/3.x`, `httpx/0.x`). A default UA can get the client banned.
   A contact address can be supplied via the `WIKIDATA_USER_AGENT` env; if
   it is not, a descriptive UA is still sent.
2. **WDQS has a 60 s wall-clock limit per query.** A broad class walk
   (`wdt:P31/wdt:P279*` + `wd:Q5`) hits this wall. Queries are kept narrow
   per category and retried on 429/5xx with exponential backoff.

The script runs offline and infrequently (weekly/monthly); it does not use
aggressive parallelism — WDQS is a shared public resource.
"""

from __future__ import annotations

import logging
import os
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from email.utils import parsedate_to_datetime

import httpx

WDQS_ENDPOINT = "https://query.wikidata.org/sparql"

# Upper bound for `Retry-After` (seconds). We honor what the server says, but
# not without limit: a misconfigured proxy's "1 day from now" would hang the
# offline script for a whole day.
RETRY_AFTER_CAP = 300.0

ROBOT_POLICY_URL = "https://foundation.wikimedia.org/wiki/Policy:Wikimedia_Foundation_User-Agent_Policy"

log = logging.getLogger(__name__)


class SPARQLError(RuntimeError):
    """Unrecoverable error from WDQS."""


class UserAgentError(SPARQLError):
    """The User-Agent does not meet the policy — the request was never sent."""


@dataclass(frozen=True)
class SPARQLConfig:
    endpoint: str = WDQS_ENDPOINT
    user_agent: str = ""
    timeout: float = 90.0
    # 4 attempts × 2s base = ~14 s total backoff. A 30-category run takes ~13
    # minutes and WDQS outages can last MINUTES: two separate runs died on
    # the last category with "4 attempts exhausted", and both passed when
    # repeated by hand right afterwards. 6 attempts wait ~2 minutes — enough
    # to absorb a short outage, while still giving up in a reasonable time on
    # a persistent failure.
    max_retries: int = 6
    retry_base: float = 2.0
    # Politeness delay between consecutive queries (seconds). WDQS is a shared
    # public resource and treats back-to-back queries under its robot policy;
    # fetching 17 categories without waiting produces a 403.
    politeness_delay: float = 2.0

    @staticmethod
    def from_env() -> "SPARQLConfig":
        return SPARQLConfig(
            endpoint=os.getenv("WIKIDATA_SPARQL_ENDPOINT", "").strip() or WDQS_ENDPOINT,
            user_agent=os.getenv("WIKIDATA_USER_AGENT", "").strip(),
        )

    def validate(self) -> None:
        """Checks the User-Agent policy before sending any request.

        **There is deliberately no code default.** The Wikimedia policy
        requires the UA to carry a reachable contact address (email or URL); a
        made-up default appears to work until it gets blocked, and when the
        block comes it reads like a query error. The same pattern is applied
        to `LOCAL_LLM_MODEL`.
        """
        ua = self.user_agent.strip()
        if not ua:
            raise UserAgentError(
                "WIKIDATA_USER_AGENT is empty. The Wikimedia robot policy requires a User-Agent "
                f"with contact information ({ROBOT_POLICY_URL}). Example:\n"
                '  export WIKIDATA_USER_AGENT="Serendipity/1.0 (team@example.com)"'
            )
        if "@" not in ua and "http" not in ua.lower():
            raise UserAgentError(
                f"WIKIDATA_USER_AGENT has no contact information: {ua!r}. "
                f"Add an email or URL ({ROBOT_POLICY_URL})."
            )


class SPARQLClient:
    """Thin client that requests JSON results from WDQS."""

    def __init__(self, cfg: SPARQLConfig | None = None, http_client: httpx.Client | None = None):
        self.cfg = cfg or SPARQLConfig.from_env()
        self.cfg.validate()
        self._own_client = http_client is None
        self._client = http_client or httpx.Client(timeout=self.cfg.timeout)
        # The first query does not wait; the politeness delay applies BETWEEN queries.
        self._sent_any = False

    def close(self) -> None:
        if self._own_client:
            self._client.close()

    def __enter__(self) -> "SPARQLClient":
        return self

    def __exit__(self, *_exc) -> None:
        self.close()

    def select(self, query: str) -> list[dict]:
        """Runs a SELECT query and returns the `results.bindings` list.

        Each returned item is Wikidata's raw binding dict:
        `{"item": {"value": "http://www.wikidata.org/entity/Q1"}, ...}`
        """
        headers = {
            "User-Agent": self.cfg.user_agent,
            "Accept": "application/sparql-results+json",
        }
        last_err: Exception | None = None

        # Politeness delay: fetching 17 categories back-to-back produces a 403
        # under the robot policy.
        if self._sent_any and self.cfg.politeness_delay > 0:
            time.sleep(self.cfg.politeness_delay)
        self._sent_any = True

        for attempt in range(self.cfg.max_retries):
            try:
                resp = self._client.post(
                    self.cfg.endpoint,
                    data={"query": query, "format": "json"},
                    headers=headers,
                )
            except httpx.HTTPError as exc:  # network error — retryable
                last_err = exc
            else:
                if resp.status_code == 200:
                    # A 200 does not guarantee the body is COMPLETE. On long
                    # results WDQS can cut the stream midway and send truncated
                    # JSON; measured: a 30-category build died entirely at
                    # minute 9 with
                    # `JSONDecodeError: Expecting ',' delimiter: line 86025`.
                    # This is the same class of transient failure as the HTTP
                    # errors and must be retried with the same backoff —
                    # otherwise a single truncated response throws away hours
                    # of work.
                    try:
                        payload = resp.json()
                    except ValueError as exc:
                        last_err = SPARQLError(f"wdqs kesik/bozuk JSON: {exc}")
                        _sleep(0, attempt, self.cfg)  # 0 → exponential backoff
                        continue
                    return payload.get("results", {}).get("bindings", [])
                if resp.status_code in (429, 500, 502, 503, 504):
                    last_err = SPARQLError(f"wdqs http {resp.status_code}: {resp.text[:200]}")
                    retry_after = _parse_retry_after(resp.headers.get("Retry-After"))
                    if retry_after:
                        _sleep(retry_after, attempt, self.cfg)
                        continue
                elif resp.status_code == 403:
                    # Robot policy block. Backoff does not fix it; the UA or the
                    # request rate must be corrected. If the message does not
                    # say so, a 403 reads like a query error.
                    raise UserAgentError(
                        f"wdqs 403 (robot policy): {resp.text[:300]}\n"
                        f"Check: does WIKIDATA_USER_AGENT carry contact info, "
                        f"is politeness_delay ({self.cfg.politeness_delay}s) sufficient. "
                        f"{ROBOT_POLICY_URL}"
                    )
                else:
                    # 400 = bad query. Retrying is pointless.
                    raise SPARQLError(f"wdqs http {resp.status_code}: {resp.text[:500]}")

            _sleep(0, attempt, self.cfg)

        raise SPARQLError(f"wdqs: retries exhausted ({self.cfg.max_retries})") from last_err


def _sleep(retry_after: float, attempt: int, cfg: SPARQLConfig) -> None:
    wait = retry_after if retry_after > 0 else cfg.retry_base * (2**attempt)
    log.warning("wdqs retry attempt=%d wait=%.1fs", attempt + 1, wait)
    time.sleep(wait)


def _parse_retry_after(value: str | None) -> float:
    """`Retry-After` → seconds. Returns 0 if it cannot be parsed (the caller
    falls back to exponential backoff).

    RFC 9110 defines two forms: delta-seconds and an RFC-1123 DATE. Parsing
    only the integer would mean continuing to hammer a server that explicitly
    says "wait until now" as soon as it answers in date form — that is exactly
    the road to WDQS's robot-policy 403, and this module was written to
    prevent it.

    The result is clamped to `[0, RETRY_AFTER_CAP]`: a "1 day from now" value
    sent by a badly configured proxy would hang the offline script for a whole
    day.
    """
    if not value:
        return 0.0
    value = value.strip()
    try:
        return _clamp_retry_after(float(value))
    except ValueError:
        pass
    try:
        when = parsedate_to_datetime(value)
    except (TypeError, ValueError):
        return 0.0
    if when is None:
        return 0.0
    if when.tzinfo is None:  # RFC-1123 dates are GMT
        when = when.replace(tzinfo=timezone.utc)
    return _clamp_retry_after((when - datetime.now(timezone.utc)).total_seconds())


def _clamp_retry_after(secs: float) -> float:
    if secs != secs or secs <= 0:  # NaN, or past/negative
        return 0.0
    return min(secs, RETRY_AFTER_CAP)


def qid_of(uri: str) -> str:
    """`http://www.wikidata.org/entity/Q123` → `Q123`."""
    return uri.rsplit("/", 1)[-1]
