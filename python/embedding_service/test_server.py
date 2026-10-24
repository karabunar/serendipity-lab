import grpc
import pytest

import embedding_pb2
import embedding_pb2_grpc
from embedder import E5Embedder
from server import EmbeddingServicer, build_server


@pytest.fixture()
def stub(embedder):
    server, port = build_server(embedder, 0)
    server.start()
    channel = grpc.insecure_channel(f"localhost:{port}")
    yield embedding_pb2_grpc.EmbeddingServiceStub(channel)
    channel.close()
    server.stop(0)


def test_health_reports_loaded_model(stub, embedder):
    h = stub.Health(embedding_pb2.HealthRequest())
    assert (h.ready, h.model, h.dim) == (True, embedder.model_name, 384)


def test_embed_returns_one_vector_per_text(stub):
    resp = stub.Embed(embedding_pb2.EmbedRequest(texts=["kedi", "köpek"], kind=embedding_pb2.EMBED_KIND_QUERY))
    assert resp.dim == 384
    assert [len(v.values) for v in resp.vectors] == [384, 384]


def test_embed_rejects_oversized_batch(embedder):
    servicer = EmbeddingServicer(embedder, max_batch=2)
    server = grpc.server(__import__("concurrent.futures").futures.ThreadPoolExecutor(2))
    embedding_pb2_grpc.add_EmbeddingServiceServicer_to_server(servicer, server)
    port = server.add_insecure_port("[::]:0")
    server.start()
    try:
        with grpc.insecure_channel(f"localhost:{port}") as ch:
            s = embedding_pb2_grpc.EmbeddingServiceStub(ch)
            with pytest.raises(grpc.RpcError) as err:
                s.Embed(embedding_pb2.EmbedRequest(texts=["a", "b", "c"]))
            assert err.value.code() == grpc.StatusCode.INVALID_ARGUMENT
    finally:
        server.stop(0)


def test_embed_before_model_is_loaded_is_unavailable():
    server, port = build_server(E5Embedder(), 0)  # never loaded
    server.start()
    try:
        with grpc.insecure_channel(f"localhost:{port}") as ch:
            s = embedding_pb2_grpc.EmbeddingServiceStub(ch)
            with pytest.raises(grpc.RpcError) as err:
                s.Embed(embedding_pb2.EmbedRequest(texts=["a"]))
            assert err.value.code() == grpc.StatusCode.UNAVAILABLE
            assert s.Health(embedding_pb2.HealthRequest()).ready is False
    finally:
        server.stop(0)
