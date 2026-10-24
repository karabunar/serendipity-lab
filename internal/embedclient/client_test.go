package embedclient

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"serendipity/internal/embedpb"
)

type fakeServer struct {
	embedpb.UnimplementedEmbeddingServiceServer
	gotKind  embedpb.EmbedKind
	dim      int32
	shortFor int // if >0, returns this many fewer vectors
}

func (f *fakeServer) Embed(_ context.Context, r *embedpb.EmbedRequest) (*embedpb.EmbedResponse, error) {
	f.gotKind = r.Kind
	n := len(r.Texts) - f.shortFor
	vs := make([]*embedpb.Vector, n)
	for i := range vs {
		vals := make([]float32, f.dim)
		vals[0] = float32(i + 1)
		vs[i] = &embedpb.Vector{Values: vals}
	}
	return &embedpb.EmbedResponse{Vectors: vs, Dim: f.dim, Model: "sahte"}, nil
}

func (f *fakeServer) Health(context.Context, *embedpb.HealthRequest) (*embedpb.HealthResponse, error) {
	return &embedpb.HealthResponse{Ready: true, Model: "sahte", Dim: f.dim}, nil
}

func startServer(t *testing.T, f *fakeServer) *Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	embedpb.RegisterEmbeddingServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	c, err := New(lis.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestEmbed_PreservesOrderAndSendsKind(t *testing.T) {
	f := &fakeServer{dim: 3}
	c := startServer(t, f)
	vs, err := c.Embed(context.Background(), []string{"a", "b"}, Query)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || vs[0][0] != 1 || vs[1][0] != 2 || len(vs[0]) != 3 {
		t.Fatalf("wrong vectors: %v", vs)
	}
	if f.gotKind != embedpb.EmbedKind_EMBED_KIND_QUERY {
		t.Fatalf("kind sorgu olarak gitmedi: %v", f.gotKind)
	}
	if _, err := c.Embed(context.Background(), []string{"x"}, Passage); err != nil || f.gotKind != embedpb.EmbedKind_EMBED_KIND_PASSAGE {
		t.Fatalf("wrong passage kind: %v %v", err, f.gotKind)
	}
}

func TestEmbed_EmptyInputNoCall(t *testing.T) {
	c := startServer(t, &fakeServer{dim: 3})
	vs, err := c.Embed(context.Background(), nil, Passage)
	if err != nil || vs != nil {
		t.Fatalf("empty input must return nil: %v %v", vs, err)
	}
}

func TestEmbed_CountMismatchIsError(t *testing.T) {
	c := startServer(t, &fakeServer{dim: 3, shortFor: 1})
	_, err := c.Embed(context.Background(), []string{"a", "b"}, Passage)
	if err == nil || !strings.Contains(err.Error(), "vectors for") {
		t.Fatalf("expected a count mismatch error: %v", err)
	}
}

func TestReady(t *testing.T) {
	c := startServer(t, &fakeServer{dim: 5})
	ok, model, dim, err := c.Ready(context.Background())
	if err != nil || !ok || model != "sahte" || dim != 5 {
		t.Fatalf("wrong Ready result: %v %v %v %v", ok, model, dim, err)
	}
}

func TestNew_EmptyTarget(t *testing.T) {
	if _, err := New("", time.Second); err == nil {
		t.Fatal("empty target was accepted")
	}
}
