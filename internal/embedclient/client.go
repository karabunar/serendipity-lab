// Package embedclient is the Go client of the embedding service (python/embedding_service).
package embedclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"serendipity/internal/embedpb"
)

// Kind says what purpose the text's vector will serve (models like e5 want
// separate prefixes for queries and documents).
type Kind int

const (
	Passage Kind = iota // content to be archived
	Query               // search query
)

// Embedder is the contract for anything that turns text into vectors; faked in tests.
type Embedder interface {
	// Embed returns the vectors preserving input order. Vectors are L2-normalized.
	Embed(ctx context.Context, texts []string, kind Kind) ([][]float32, error)
}

// Client is the gRPC implementation of Embedder.
type Client struct {
	conn    *grpc.ClientConn
	rpc     embedpb.EmbeddingServiceClient
	timeout time.Duration
}

var _ Embedder = (*Client)(nil)

// New builds a client for target (e.g. localhost:50061). The connection is opened on the first call.
func New(target string, timeout time.Duration) (*Client, error) {
	if target == "" {
		return nil, errors.New("embedclient: empty target address")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("embedclient: failed to connect: %w", err)
	}
	return &Client{conn: conn, rpc: embedpb.NewEmbeddingServiceClient(conn), timeout: timeout}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) Embed(ctx context.Context, texts []string, kind Kind) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	k := embedpb.EmbedKind_EMBED_KIND_PASSAGE
	if kind == Query {
		k = embedpb.EmbedKind_EMBED_KIND_QUERY
	}
	resp, err := c.rpc.Embed(ctx, &embedpb.EmbedRequest{Texts: texts, Kind: k})
	if err != nil {
		return nil, fmt.Errorf("embedclient: Embed: %w", err)
	}
	if len(resp.Vectors) != len(texts) {
		return nil, fmt.Errorf("embedclient: got %d vectors for %d texts", len(resp.Vectors), len(texts))
	}
	out := make([][]float32, len(resp.Vectors))
	for i, v := range resp.Vectors {
		if int32(len(v.Values)) != resp.Dim {
			return nil, fmt.Errorf("embedclient: vector %d has size %d, expected %d", i, len(v.Values), resp.Dim)
		}
		out[i] = v.Values
	}
	return out, nil
}

// Ready returns whether the service has loaded the model, and its dimension.
func (c *Client) Ready(ctx context.Context) (ready bool, model string, dim int, err error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.rpc.Health(ctx, &embedpb.HealthRequest{})
	if err != nil {
		return false, "", 0, fmt.Errorf("embedclient: Health: %w", err)
	}
	return resp.Ready, resp.Model, int(resp.Dim), nil
}
