package vectordb

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// idPayloadKey is where the application ID lives in the payload. Qdrant accepts only uint or
// UUID IDs, so the string ID is converted to a UUIDv5 and the original value is stored here.
const idPayloadKey = "_id"

// namespace is the fixed namespace for UUIDv5 generation; if it changes, existing records are orphaned.
var namespace = [16]byte{0x6f, 0x6e, 0x65, 0x72, 0x69, 0x2d, 0x64, 0x65, 0x6e, 0x65, 0x79, 0x69, 0x2d, 0x76, 0x64, 0x62}

// Qdrant is the Qdrant REST adapter of VectorDB (net/http only; no extra dependencies).
type Qdrant struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

var _ VectorDB = (*Qdrant)(nil)

// NewQdrant builds the adapter with baseURL (e.g. http://localhost:6333) and an optional apiKey.
func NewQdrant(baseURL, apiKey string, timeout time.Duration) *Qdrant {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Qdrant{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  &http.Client{Timeout: timeout},
	}
}

// PointID deterministically converts a string ID to Qdrant's UUID format (UUIDv5).
func PointID(id string) string {
	h := sha1.New()
	h.Write(namespace[:])
	h.Write([]byte(id))
	sum := h.Sum(nil)
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// ErrDimMismatch is returned when the existing collection's dimension differs from the requested one.
var ErrDimMismatch = errors.New("vectordb: koleksiyon size mismatch")

func (q *Qdrant) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, q.baseURL+path, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if q.apiKey != "" {
		req.Header.Set("api-key", q.apiKey)
	}
	resp, err := q.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusNotFound {
		return resp.StatusCode, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return resp.StatusCode, fmt.Errorf("qdrant %s %s: %d %s", method, path, resp.StatusCode, msg)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("failed to decode qdrant response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func (q *Qdrant) EnsureCollection(ctx context.Context, name string, dim int) error {
	if dim <= 0 {
		return fmt.Errorf("vectordb: invalid dimension %d", dim)
	}
	path := "/collections/" + url.PathEscape(name)
	var info struct {
		Result struct {
			Config struct {
				Params struct {
					Vectors struct {
						Size int `json:"size"`
					} `json:"vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	status, err := q.do(ctx, http.MethodGet, path, nil, &info)
	if err != nil {
		return err
	}
	if status != http.StatusNotFound {
		if got := info.Result.Config.Params.Vectors.Size; got != dim {
			return fmt.Errorf("%w: %s existing=%d requested=%d", ErrDimMismatch, name, got, dim)
		}
		return nil
	}
	body := map[string]any{"vectors": map[string]any{"size": dim, "distance": "Cosine"}}
	_, err = q.do(ctx, http.MethodPut, path, body, nil)
	return err
}

func (q *Qdrant) Upsert(ctx context.Context, collection string, points []Point) error {
	if len(points) == 0 {
		return nil
	}
	items := make([]map[string]any, 0, len(points))
	for _, p := range points {
		if p.ID == "" {
			return errors.New("vectordb: empty ID")
		}
		payload := make(map[string]any, len(p.Payload)+1)
		for k, v := range p.Payload {
			payload[k] = v
		}
		payload[idPayloadKey] = p.ID
		items = append(items, map[string]any{"id": PointID(p.ID), "vector": p.Vector, "payload": payload})
	}
	path := "/collections/" + url.PathEscape(collection) + "/points?wait=true"
	_, err := q.do(ctx, http.MethodPut, path, map[string]any{"points": items}, nil)
	return err
}

func (q *Qdrant) Search(ctx context.Context, collection string, req SearchRequest) ([]Hit, error) {
	if len(req.Vector) == 0 {
		return nil, errors.New("vectordb: empty query vector")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	body := map[string]any{"vector": req.Vector, "limit": limit, "with_payload": true}
	if req.MinScore > 0 {
		body["score_threshold"] = req.MinScore
	}
	if len(req.Match) > 0 {
		must := make([]map[string]any, 0, len(req.Match))
		for k, v := range req.Match {
			must = append(must, map[string]any{"key": k, "match": map[string]any{"value": v}})
		}
		body["filter"] = map[string]any{"must": must}
	}
	var out struct {
		Result []struct {
			ID      string         `json:"id"`
			Score   float64        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	path := "/collections/" + url.PathEscape(collection) + "/points/search"
	status, err := q.do(ctx, http.MethodPost, path, body, &out)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("vectordb: collection not found: %s", collection)
	}
	hits := make([]Hit, 0, len(out.Result))
	for _, r := range out.Result {
		id, _ := r.Payload[idPayloadKey].(string)
		if id == "" {
			id = r.ID
		}
		delete(r.Payload, idPayloadKey)
		hits = append(hits, Hit{ID: id, Score: r.Score, Payload: r.Payload})
	}
	return hits, nil
}

func (q *Qdrant) GetVectors(ctx context.Context, collection string, ids []string) (map[string][]float32, error) {
	out := make(map[string][]float32, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	pids := make([]string, len(ids))
	for i, id := range ids {
		pids[i] = PointID(id)
	}
	body := map[string]any{"ids": pids, "with_vector": true, "with_payload": []string{idPayloadKey}}
	var resp struct {
		Result []struct {
			ID      string         `json:"id"`
			Vector  []float32      `json:"vector"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	path := "/collections/" + url.PathEscape(collection) + "/points"
	status, err := q.do(ctx, http.MethodPost, path, body, &resp)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("vectordb: collection not found: %s", collection)
	}
	for _, r := range resp.Result {
		id, _ := r.Payload[idPayloadKey].(string)
		if id == "" || len(r.Vector) == 0 {
			continue
		}
		out[id] = r.Vector
	}
	return out, nil
}

func (q *Qdrant) Delete(ctx context.Context, collection string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	pids := make([]string, len(ids))
	for i, id := range ids {
		pids[i] = PointID(id)
	}
	path := "/collections/" + url.PathEscape(collection) + "/points/delete?wait=true"
	_, err := q.do(ctx, http.MethodPost, path, map[string]any{"points": pids}, nil)
	return err
}
