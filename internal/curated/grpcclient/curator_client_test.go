package grpcclient

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"serendipity/internal/curatorpb"
)

// startFakeCuratorTCPServer follows the same pattern as startFakeServer in embedding_client_test.go:
// it connects to a real TCP listener (not bufconn) — it exercises NewCuratorClient's
// dns:///+passthrough:/// scheme selection and the real dial flow end to end
// (Finding #3: the constructor itself was not called in any test).
func startFakeCuratorTCPServer(t *testing.T, srv *fakePostCuratorServer) (string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	curatorpb.RegisterPostCuratorServer(s, srv)
	go func() {
		_ = s.Serve(lis)
	}()
	return lis.Addr().String(), func() { s.GracefulStop() }
}

const bufSize = 1024 * 1024

type fakePostCuratorServer struct {
	curatorpb.UnimplementedPostCuratorServer
	response *curatorpb.EnrichPostsResponse
	err      error
}

func (s *fakePostCuratorServer) EnrichPosts(_ context.Context, _ *curatorpb.EnrichPostsRequest) (*curatorpb.EnrichPostsResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.response, nil
}

func (s *fakePostCuratorServer) HealthCheck(_ context.Context, _ *curatorpb.HealthCheckRequest) (*curatorpb.HealthCheckResponse, error) {
	return &curatorpb.HealthCheckResponse{Ready: true}, nil
}

func startFakeCuratorServer(t *testing.T, srv *fakePostCuratorServer) (*grpc.ClientConn, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	s := grpc.NewServer()
	curatorpb.RegisterPostCuratorServer(s, srv)
	go func() {
		_ = s.Serve(lis)
	}()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn, func() { s.Stop(); conn.Close() }
}

func TestCuratorClient_EnrichPosts_Success(t *testing.T) {
	fake := &fakePostCuratorServer{
		response: &curatorpb.EnrichPostsResponse{
			Results: []*curatorpb.EnrichedPost{
				{PostId: "p1", QualityScore: 0.8},
			},
		},
	}
	conn, cleanup := startFakeCuratorServer(t, fake)
	defer cleanup()

	c := &CuratorClient{client: curatorpb.NewPostCuratorClient(conn), timeout: time.Second}
	got, err := c.EnrichPosts(context.Background(), []EnrichCandidate{{PostID: "p1", Text: "test", HasMedia: false}})
	if err != nil {
		t.Fatalf("EnrichPosts: %v", err)
	}
	if len(got) != 1 || got[0].PostID != "p1" || got[0].QualityScore != 0.8 {
		t.Errorf("unexpected result: %+v", got)
	}
}

func TestCuratorClient_EnrichPosts_ServerError_ReturnsError(t *testing.T) {
	fake := &fakePostCuratorServer{err: context.DeadlineExceeded}
	conn, cleanup := startFakeCuratorServer(t, fake)
	defer cleanup()

	c := &CuratorClient{client: curatorpb.NewPostCuratorClient(conn), timeout: time.Second}
	_, err := c.EnrichPosts(context.Background(), []EnrichCandidate{{PostID: "p1", Text: "test"}})
	if err == nil {
		t.Error("expected error when server fails")
	}
}

func TestCuratorClient_EnrichPosts_EmptyCandidates_NoCall(t *testing.T) {
	fake := &fakePostCuratorServer{err: context.DeadlineExceeded} // the test fails if it is called
	conn, cleanup := startFakeCuratorServer(t, fake)
	defer cleanup()

	c := &CuratorClient{client: curatorpb.NewPostCuratorClient(conn), timeout: time.Second}
	got, err := c.EnrichPosts(context.Background(), nil)
	if err != nil {
		t.Fatalf("EnrichPosts with empty input should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %d", len(got))
	}
}

// TestNewCuratorClient_DefaultsAndRoundTrip calls the NewCuratorClient constructor
// itself (the other tests bypassed it with &CuratorClient{...} —
// Finding #3). It verifies the timeout<=0 and logger=nil default-value branches, and that the
// dns:/// scheme + insecure TLS dial flow over a real TCP connection
// works end to end.
func TestNewCuratorClient_DefaultsAndRoundTrip(t *testing.T) {
	fake := &fakePostCuratorServer{
		response: &curatorpb.EnrichPostsResponse{
			Results: []*curatorpb.EnrichedPost{
				{PostId: "p1", QualityScore: 0.5},
			},
		},
	}
	addr, cleanup := startFakeCuratorTCPServer(t, fake)
	defer cleanup()

	// timeout<=0 and logger=nil → exercises the default-value branches (10s / slog.Default()).
	c, err := NewCuratorClient(addr, 0, false, nil)
	if err != nil {
		t.Fatalf("NewCuratorClient: %v", err)
	}
	defer c.Close()

	if c.timeout != 10*time.Second {
		t.Errorf("expected default timeout 10s, got %v", c.timeout)
	}
	if c.logger == nil {
		t.Error("expected default logger to be set (slog.Default())")
	}

	got, err := c.EnrichPosts(context.Background(), []EnrichCandidate{{PostID: "p1", Text: "test"}})
	if err != nil {
		t.Fatalf("EnrichPosts round-trip via real dial: %v", err)
	}
	if len(got) != 1 || got[0].PostID != "p1" {
		t.Errorf("unexpected result: %+v", got)
	}

	// Double-close must not panic (grpc.ClientConn tolerates double-close).
	if err := c.Close(); err != nil {
		t.Errorf("close error: %v", err)
	}
}
