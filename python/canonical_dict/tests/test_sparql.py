"""WDQS client — with httpx MockTransport, without touching the network."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from email.utils import format_datetime

import httpx
import pytest

from canonical_dict.sparql import (
    RETRY_AFTER_CAP,
    SPARQLClient,
    SPARQLConfig,
    SPARQLError,
    UserAgentError,
    _parse_retry_after,
    qid_of,
)

FAST = SPARQLConfig(
    user_agent="TestUA/1.0 (test@ornek.com)",
    max_retries=3,
    retry_base=0.001,
    politeness_delay=0,
)


def client_with(handler) -> SPARQLClient:
    transport = httpx.MockTransport(handler)
    return SPARQLClient(FAST, http_client=httpx.Client(transport=transport))


def ok_body(rows: list[dict]) -> dict:
    return {"results": {"bindings": rows}}


def test_select_returns_bindings():
    def handler(_request):
        return httpx.Response(200, json=ok_body([{"item": {"value": "http://www.wikidata.org/entity/Q1"}}]))

    with client_with(handler) as c:
        rows = c.select("SELECT ?item WHERE {}")
    assert qid_of(rows[0]["item"]["value"]) == "Q1"


def test_user_agent_is_sent():
    # Wikimedia blocks library-default UAs — this header is required.
    seen = {}

    def handler(request):
        seen["ua"] = request.headers.get("User-Agent")
        seen["accept"] = request.headers.get("Accept")
        return httpx.Response(200, json=ok_body([]))

    with client_with(handler) as c:
        c.select("SELECT ?item WHERE {}")
    assert seen["ua"] == "TestUA/1.0 (test@ornek.com)"
    assert seen["accept"] == "application/sparql-results+json"


def test_5xx_is_retried():
    calls = {"n": 0}

    def handler(_request):
        calls["n"] += 1
        if calls["n"] == 1:
            return httpx.Response(503, text="busy")
        return httpx.Response(200, json=ok_body([{"item": {"value": "x/Q9"}}]))

    with client_with(handler) as c:
        rows = c.select("SELECT ?item WHERE {}")
    assert calls["n"] == 2
    assert len(rows) == 1


def test_truncated_json_is_retried():
    # Regression: WDQS can return 200 and cut the body MIDWAY. Previously
    # resp.json() raised JSONDecodeError, no except caught it, and the
    # 30-category build died entirely at minute 9.
    calls = {"n": 0}

    def handler(_request):
        calls["n"] += 1
        if calls["n"] == 1:
            return httpx.Response(200, text='{"results": {"bindings": [{"item"')
        return httpx.Response(200, json=ok_body([{"item": {"value": "x/Q7"}}]))

    with client_with(handler) as c:
        rows = c.select("SELECT ?item WHERE {}")
    assert calls["n"] == 2
    assert len(rows) == 1


def test_429_retry_after_is_honored(monkeypatch):
    # The old test sent "0"; since _parse_retry_after returned it as falsy, the
    # relevant branch was NEVER ENTERED — the test passed without the behavior
    # being measured.
    slept: list[float] = []
    monkeypatch.setattr("canonical_dict.sparql.time.sleep", lambda s: slept.append(s))
    calls = {"n": 0}

    def handler(_request):
        calls["n"] += 1
        if calls["n"] == 1:
            return httpx.Response(429, headers={"Retry-After": "7"}, text="slow down")
        return httpx.Response(200, json=ok_body([]))

    with client_with(handler) as c:
        c.select("SELECT ?item WHERE {}")
    assert calls["n"] == 2
    # Not the exponential backoff, but the duration the server said.
    assert slept == [7.0]


def test_retry_after_date_format_is_parsed():
    # RFC 9110 defines two forms. Parsing only the integer would mean continuing
    # to hammer a server that answers in date form — the road to WDQS's robot
    # policy 403.
    when = datetime.now(timezone.utc) + timedelta(seconds=42)
    got = _parse_retry_after(format_datetime(when, usegmt=True))
    assert 35 <= got <= 45


def test_retry_after_past_date_returns_zero():
    when = datetime.now(timezone.utc) - timedelta(hours=1)
    assert _parse_retry_after(format_datetime(when, usegmt=True)) == 0.0


def test_retry_after_garbage_value_returns_zero():
    for bad in ("yakinda", "", None, "1e", "NaN"):
        assert _parse_retry_after(bad) == 0.0


def test_retry_after_negative_returns_zero():
    assert _parse_retry_after("-5") == 0.0


def test_retry_after_is_clamped_to_upper_bound():
    # A misconfigured proxy's "1 day from now" would hang the offline script for
    # a whole day.
    assert _parse_retry_after("86400") == RETRY_AFTER_CAP
    when = datetime.now(timezone.utc) + timedelta(days=1)
    assert _parse_retry_after(format_datetime(when, usegmt=True)) == RETRY_AFTER_CAP


def test_400_is_not_retried():
    # The query is bad; resending produces the same error.
    calls = {"n": 0}

    def handler(_request):
        calls["n"] += 1
        return httpx.Response(400, text="malformed query")

    with client_with(handler) as c, pytest.raises(SPARQLError):
        c.select("BOZUK")
    assert calls["n"] == 1


def test_403_is_not_retried_and_points_to_policy():
    # Robot policy block; backoff does not fix it, human intervention is needed.
    # If the message does not mention UA/rate, a 403 reads like a query error.
    calls = {"n": 0}

    def handler(_request):
        calls["n"] += 1
        return httpx.Response(403, text="respect our robot policy")

    with client_with(handler) as c, pytest.raises(UserAgentError, match="robot policy"):
        c.select("SELECT ?item WHERE {}")
    assert calls["n"] == 1


def test_user_agent_without_contact_is_rejected_before_sending():
    def handler(_request):  # pragma: no cover — must never get here
        raise AssertionError("istek gonderilmemeliydi")

    cfg = SPARQLConfig(user_agent="Serendipity/1.0", politeness_delay=0)
    with pytest.raises(UserAgentError, match="has no contact information"):
        SPARQLClient(cfg, http_client=httpx.Client(transport=httpx.MockTransport(handler)))


def test_empty_user_agent_is_rejected():
    with pytest.raises(UserAgentError, match="WIKIDATA_USER_AGENT is empty"):
        SPARQLClient(SPARQLConfig(user_agent="  ", politeness_delay=0))


def test_user_agent_containing_url_is_accepted():
    def handler(_request):
        return httpx.Response(200, json=ok_body([]))

    cfg = SPARQLConfig(user_agent="Serendipity/1.0 (https://ornek.com/iletisim)", politeness_delay=0)
    with SPARQLClient(cfg, http_client=httpx.Client(transport=httpx.MockTransport(handler))) as c:
        assert c.select("SELECT ?item WHERE {}") == []


def test_politeness_delay_absent_on_first_query_present_afterwards(monkeypatch):
    # politeness_delay being defined but NEVER USED produced a 403 from
    # the endpoint; this test locks that regression.
    slept: list[float] = []
    monkeypatch.setattr("canonical_dict.sparql.time.sleep", lambda s: slept.append(s))

    def handler(_request):
        return httpx.Response(200, json=ok_body([]))

    cfg = SPARQLConfig(user_agent="Serendipity/1.0 (test@ornek.com)", politeness_delay=2.0)
    with SPARQLClient(cfg, http_client=httpx.Client(transport=httpx.MockTransport(handler))) as c:
        c.select("SELECT ?item WHERE {}")
        assert slept == []
        c.select("SELECT ?item WHERE {}")
        c.select("SELECT ?item WHERE {}")
    assert slept == [2.0, 2.0]


def test_error_when_all_attempts_exhausted():
    def handler(_request):
        return httpx.Response(503, text="busy")

    with client_with(handler) as c, pytest.raises(SPARQLError, match="retries exhausted"):
        c.select("SELECT ?item WHERE {}")


def test_qid_of():
    assert qid_of("http://www.wikidata.org/entity/Q42") == "Q42"
