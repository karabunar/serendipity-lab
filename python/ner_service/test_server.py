import importlib.util
import os
import subprocess
import sys

import pytest

import server as server_mod
from server import extract_spans, KEEP_LABELS, MAX_TOKENS, MAX_MSG_BYTES


def _model_available() -> bool:
    if importlib.util.find_spec("spacy") is None:
        return False
    return importlib.util.find_spec(server_mod.MODEL) is not None


# Tests that need the model are skipped with an EXPLICIT reason if spaCy + NER_MODEL are missing.
# `import spacy` used to be at module top and in those environments the WHOLE file
# failed at collection — config/health tests included (A3 leftover).
requires_model = pytest.mark.skipif(
    not _model_available(), reason="spaCy and/or NER_MODEL is not installed")


def test_imports_without_spacy():
    """A3: the server module must be importable WITHOUT the spaCy package.

    The model was already loaded lazily but `import spacy` was at module top;
    in environments without the package, config/health tests could not be
    collected. Setting spaCy to None in sys.modules makes it look 'not installed'.
    """
    code = "import sys; sys.modules['spacy'] = None; import server; print(server.MAX_MSG_BYTES)"
    result = subprocess.run([sys.executable, "-c", code], cwd=os.path.dirname(__file__) or ".",
                           capture_output=True, text=True)
    assert result.returncode == 0, result.stderr


@requires_model
def test_finds_proper_noun():
    text = "Fenerbahçe'nin transferi İstanbul'a geldi"
    spans = extract_spans(text)
    surfaces = {s["text"] for s in spans}
    assert "Fenerbahçe" in surfaces
    fener = next(s for s in spans if s["text"] == "Fenerbahçe")
    assert fener["start"] == 0
    assert text[fener["start"]:fener["end"]].startswith("Fenerbahçe")


@requires_model
def test_finds_nothing_in_everyday_sentence():
    # Measured false-positive class: the dictionary fired on these, NER does not.
    assert extract_spans("Bana göre bu şey gerçekten saçma") == []


@requires_model
def test_norp_and_work_of_art_are_filtered():
    # NO DIRECT reference to KEEP_LABELS: if the constant is widened by mistake
    # (e.g. NORP added) this test must break with its own expected set —
    # otherwise it would be a tautological check where every span passes.
    expected_labels = {"PERSON", "PER", "ORG", "LOC", "GPE", "FAC", "EVENT", "PRODUCT"}
    assert KEEP_LABELS == expected_labels

    # The model finds two entities in this sentence: "Türk" (NORP, must be dropped) and
    # "Fenerbahçe'nin" (ORG, must stay — "Fenerbahçe" after normalization). This proves
    # the filter REALLY works (without falling into the empty-list trap).
    spans = extract_spans("Türk milleti Fenerbahçe'nin şampiyonluğunu kutladı")
    assert spans  # guard against the empty-list trap
    surfaces = {s["text"] for s in spans}
    assert "Fenerbahçe" in surfaces
    assert "Türk" not in surfaces
    for s in spans:
        assert s["label"] in expected_labels


@requires_model
def test_long_span_is_dropped():
    # The model labels "Türkiye Büyük Millet Meclisi Başkanlık Divanı Genel Kurulu" as
    # a SINGLE 8-token ORG span (verified with the venv) — it exceeds MAX_TOKENS=6
    # and must be dropped ENTIRELY. "Fenerbahçe" (1 token) in the same text must
    # stay; this proves both that the cap really filters and that the
    # empty-list trap is avoided.
    text = ("Fenerbahçe transfer yaptı. Türkiye Büyük Millet Meclisi Başkanlık "
             "Divanı Genel Kurulu bugün toplandı.")
    spans = extract_spans(text)
    assert spans  # guard against the empty-list trap
    surfaces = {s["text"] for s in spans}
    assert "Fenerbahçe" in surfaces
    assert not any("Meclisi" in s["text"] for s in spans)
    for s in spans:
        assert len(s["text"].split()) <= MAX_TOKENS


def test_server_sets_message_ceiling(monkeypatch):
    """The gRPC message cap must be set explicitly at BOTH ends.

    The gRPC default is 4 MB in the RECEIVE direction. The client sends
    ENTITY_NER_BATCH items per round in a single request; if the cap is
    exceeded the error is RESOURCE_EXHAUSTED and the client's fail-closed gate
    skips the WHOLE round without writing. The client side must set the same cap.

    The test REALLY calls serve() (it does not just read the constant): if the
    options parameter is dropped from serve(), this breaks.
    """
    captured = {}

    class FakeServer:
        def add_insecure_port(self, _adres):
            return 1

        def start(self):
            pass

        def wait_for_termination(self):
            raise SystemExit  # finish without blocking serve()

    def fake_grpc_server(_executor, options=None):
        captured["options"] = dict(options or [])
        return FakeServer()

    monkeypatch.setattr(server_mod.grpc, "server", fake_grpc_server)
    monkeypatch.setattr(server_mod.ner_pb2_grpc,
                        "add_NERServiceServicer_to_server", lambda _s, _srv: None)

    with pytest.raises(SystemExit):
        server_mod.serve()

    opts = captured["options"]
    assert opts.get("grpc.max_receive_message_length") == MAX_MSG_BYTES
    assert opts.get("grpc.max_send_message_length") == MAX_MSG_BYTES
    assert MAX_MSG_BYTES == 100 * 1024 * 1024
