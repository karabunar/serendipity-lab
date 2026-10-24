"""ner-service: Turkish proper-noun extraction (spaCy, CPU-only).

Why CPU: the small spaCy model is fast enough on CPU, and keeping the entity
pipeline free of GPU requirements makes it easy to run anywhere.
"""
import logging
import os
import re
import threading
from concurrent import futures

import grpc

import ner_pb2
import ner_pb2_grpc

# Types that carry proper names. NORP and WORK_OF_ART are DELIBERATELY excluded: in
# measurement WORK_OF_ART fires on quoted poem lines, and NORP produces overly
# generic keys like "Turk"/"Musluman".
KEEP_LABELS = {"PERSON", "PER", "ORG", "LOC", "GPE", "FAC", "EVENT", "PRODUCT"}

# An entity name does not exceed 6 tokens. In measurement the longest span was 357 tokens (a hashtag block).
MAX_TOKENS = 6

MODEL = os.getenv("NER_MODEL", "tr_core_news_lg")

# gRPC message size cap. As a precedent, the embedding pipeline also uses 100 MB.
#
# WHY IT IS SET EXPLICITLY: the gRPC default is 4 MB in the RECEIVE direction. The
# client sends ENTITY_NER_BATCH items per round in a single request; if the cap
# is exceeded the error is RESOURCE_EXHAUSTED and the client's fail-closed gate
# skips the WHOLE round without writing. The cap must be set at BOTH ends; one end is not enough.
MAX_MSG_BYTES = int(os.getenv("NER_MAX_MESSAGE_MB", "100")) * 1024 * 1024

_APOSTROPHE = re.compile(r"['’]")

log = logging.getLogger("ner-service")

# parser/tagger/morphologizer/lemmatizer are unnecessary for NER — only tok2vec+ner.
# Loading the model at the first RPC rather than at import time keeps the config/health
# tests from depending on a 500 MB model load (A3). `import spacy` is also HERE,
# not at module top: in an environment without the package the module can be imported
# and the config/health tests can be collected.
_nlp = None
_NLP_LOCK = threading.Lock()


def _get_nlp():
    global _nlp
    if _nlp is not None:
        return _nlp
    # Double-checked lock: if NER_WORKERS threads enter at the same time on the first
    # request, the model (~500 MB) is loaded ONLY ONCE.
    with _NLP_LOCK:
        if _nlp is None:
            import spacy  # lazy — see the comment above

            _nlp = spacy.load(MODEL, exclude=["parser", "tagger", "morphologizer",
                                              "trainable_lemmatizer"])
    return _nlp


def _normalize(surface: str) -> str:
    """Drops everything after the apostrophe: "Fenerbahce'nin" → "Fenerbahce"."""
    return _APOSTROPHE.split(surface, maxsplit=1)[0].strip()


def _spans_from_doc(doc) -> list[dict]:
    out, seen = [], set()
    for e in doc.ents:
        if e.label_ not in KEEP_LABELS:
            continue
        t = _normalize(e.text)
        if len(t) < 2 or len(t.split()) > MAX_TOKENS:
            continue
        key = (t.casefold(), e.label_)
        if key in seen:
            continue
        seen.add(key)
        # score is a constant 1.0: spaCy doc.ents does NOT return a confidence score
        # per span (no logit/softmax concept as in transformer pipelines) —
        # there is deliberately no threshold or dial here.
        out.append({"text": t, "label": e.label_, "score": 1.0,
                    "start": e.start_char, "end": e.end_char})
    return out


def extract_spans(text: str) -> list[dict]:
    # The spaCy pipeline object is shared across worker threads; since the model
    # gives no thread-safety guarantee for __call__, we lock inference at a single
    # point. gRPC worker parallelism stays in the request queue, and the
    # model state does not race.
    with _NLP_LOCK:
        return _spans_from_doc(_get_nlp()(text))


def extract_spans_batch(texts: list[str]) -> list[list[dict]]:
    """Processes an RPC's documents through spaCy's batch path."""
    with _NLP_LOCK:
        return [_spans_from_doc(doc) for doc in _get_nlp().pipe(texts)]


class NERServicer(ner_pb2_grpc.NERServiceServicer):
    def ExtractBatch(self, request, context):
        resp = ner_pb2.ExtractResponse()
        texts = [p.text for p in request.posts]
        for post, doc_spans in zip(request.posts, extract_spans_batch(texts)):
            if not doc_spans:
                continue  # empty list = does not appear in the response at all
            sl = resp.results[post.post_id]
            for s in doc_spans:
                sl.spans.add(text=s["text"], label=s["label"],
                             score=s["score"], start=s["start"], end=s["end"])
        return resp


def serve() -> None:
    port = os.getenv("NER_PORT", "50053")
    workers = int(os.getenv("NER_WORKERS", "4"))
    server = grpc.server(
        futures.ThreadPoolExecutor(max_workers=workers),
        options=[
            ("grpc.max_receive_message_length", MAX_MSG_BYTES),
            ("grpc.max_send_message_length", MAX_MSG_BYTES),
        ],
    )
    ner_pb2_grpc.add_NERServiceServicer_to_server(NERServicer(), server)
    server.add_insecure_port(f"[::]:{port}")
    server.start()
    log.info("ner-service listening on :%s (model=%s)", port, MODEL)
    server.wait_for_termination()


if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO)
    serve()
