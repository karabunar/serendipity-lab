"""Minimal service test verifying that the HealthCheck RPC works."""

import curator_pb2
from grpc_server import PostCuratorServicer


class _FakeContext:
    def set_code(self, code):
        self.code = code

    def set_details(self, details):
        self.details = details


def test_health_check_returns_ready():
    servicer = PostCuratorServicer(base_url="http://llm.local:8000", model="m1")
    response = servicer.HealthCheck(curator_pb2.HealthCheckRequest(), _FakeContext())
    assert response.ready is True
