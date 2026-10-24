// Package grpcclient: curator_client.go provides the CuratorClient that connects
// to the python/agentic_curator (Python, LangGraph) service over gRPC.
// The same connection pattern as the embedding client (internal/embedclient): TLS/insecure
// credential selection, dns:/// vs passthrough:/// scheme selection (K8s headless-service
// round-robin), OTel stats handler, Prometheus client interceptors,
// WithDefaultServiceConfig round-robin and message size call options are mirrored
// one-to-one. There is NO retry+backoff wrapping (unlike the embedding client) — this
// is a deliberate difference: the booster side handles the fallback in Task 4, a single-attempt
// RPC + upper-layer fallback is enough.
package grpcclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"serendipity/internal/curatorpb"
)

// curatorMaxMessageMB is the gRPC message size limit (MB). Same as the embedding client's
// MaxMessageMB<=0 fallback value (100 MB) — the curator RPC response only
// carries post_id+quality_score (Strategy 3 — no category/embedding), a small
// payload, so there is no need to open a separate env/config field.
const curatorMaxMessageMB = 100

// EnrichCandidate is a candidate that has passed the booster's (Go) heuristic pre-filter
// post type. A separate Go type so as not to leak the proto type outside.
type EnrichCandidate struct {
	PostID   string
	Text     string
	HasMedia bool
}

// EnrichedPost is the Quality Analyst output. It carries NO category/embedding
// (Strategy 3 — Vector-Native Matching, content.Fetcher does niche
// matching with the post's original embedding).
type EnrichedPost struct {
	PostID       string
	QualityScore float64
}

// CuratorClient is the client that connects to the PostCurator gRPC service.
type CuratorClient struct {
	client  curatorpb.PostCuratorClient
	conn    *grpc.ClientConn
	timeout time.Duration
	logger  *slog.Logger
}

// NewCuratorClient connects to the given endpoint and returns a CuratorClient.
// If timeout<=0, the 10s default is used (CURATOR_GRPC_TIMEOUT).
// useTLS has the same meaning as UseTLS in the embedding client config: it connects
// with TLS (ngrok / public endpoint) and selects the passthrough:/// scheme; if false,
// insecure credentials + dns:/// scheme (K8s headless-service round-robin).
func NewCuratorClient(endpoint string, timeout time.Duration, useTLS bool, logger *slog.Logger) (*CuratorClient, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}

	target := endpoint
	if !strings.Contains(target, "://") {
		if useTLS {
			// passthrough: avoids getting stuck on IPv6 with mixed IPv6/IPv4 DNS results;
			// TLS SNI uses the host name directly (ngrok / public endpoint).
			target = "passthrough:///" + target
		} else {
			target = "dns:///" + target
		}
	}

	transportCreds := grpc.WithTransportCredentials(insecure.NewCredentials())
	if useTLS {
		transportCreds = grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{}))
	}

	maxMsgBytes := curatorMaxMessageMB * 1024 * 1024

	conn, err := grpc.NewClient(
		target,
		transportCreds,
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallSendMsgSize(maxMsgBytes),
			grpc.MaxCallRecvMsgSize(maxMsgBytes),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("curator client dial %s: %w", endpoint, err)
	}

	logger.Debug("curator client dial ok",
		slog.String("endpoint", endpoint),
		slog.Bool("use_tls", useTLS),
		slog.Duration("timeout", timeout),
	)

	return &CuratorClient{
		client:  curatorpb.NewPostCuratorClient(conn),
		conn:    conn,
		timeout: timeout,
		logger:  logger,
	}, nil
}

// Close closes the underlying gRPC connection.
func (c *CuratorClient) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// EnrichPosts enriches the given candidates via PostCurator.EnrichPosts.
// Empty candidates → (nil, nil), no RPC call is made (if the booster's heuristic
// pre-filter left no candidates, an unnecessary round-trip is avoided).
func (c *CuratorClient) EnrichPosts(ctx context.Context, candidates []EnrichCandidate) ([]EnrichedPost, error) {
	if len(candidates) == 0 {
		return nil, nil
	}

	cCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req := &curatorpb.EnrichPostsRequest{
		Candidates: make([]*curatorpb.PostCandidate, len(candidates)),
	}
	for i, cand := range candidates {
		req.Candidates[i] = &curatorpb.PostCandidate{
			PostId:   cand.PostID,
			Text:     cand.Text,
			HasMedia: cand.HasMedia,
		}
	}

	resp, err := c.client.EnrichPosts(cCtx, req)
	if err != nil {
		return nil, fmt.Errorf("curator EnrichPosts: %w", err)
	}

	out := make([]EnrichedPost, len(resp.Results))
	for i, r := range resp.Results {
		out[i] = EnrichedPost{
			PostID:       r.PostId,
			QualityScore: r.QualityScore,
		}
	}
	return out, nil
}
