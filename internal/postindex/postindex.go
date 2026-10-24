// Package postindex holds the Indexer that turns post text into vectors via the embedding service and writes them to the vector
// database, Similar that searches for similar posts, and VectorSource that reads stored post
// vectors by ID. Both sides depend on interfaces
// (embedclient.Embedder, vectordb.VectorDB); the concrete service is not known.
package postindex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"serendipity/internal/embedclient"
	"serendipity/internal/vectordb"
)

// DefaultBatch is the maximum number of posts per single embed call
// (below 256, the embedding service's per-request cap).
const DefaultBatch = 64

// Post holds the required fields of a post to be indexed.
type Post struct {
	ID       string
	AuthorID string
	Category string
	Text     string
}

// Indexer embeds posts and writes them to the collection.
type Indexer struct {
	emb        embedclient.Embedder
	db         vectordb.VectorDB
	collection string
	batch      int

	once    sync.Once
	ensured error
}

// NewIndexer builds an indexer that writes to the collection named collection.
// If batch<=0, DefaultBatch is used.
func NewIndexer(emb embedclient.Embedder, db vectordb.VectorDB, collection string, batch int) (*Indexer, error) {
	if emb == nil || db == nil {
		return nil, errors.New("postindex: embedder and vector DB are required")
	}
	if collection == "" {
		return nil, errors.New("postindex: empty collection name")
	}
	if batch <= 0 {
		batch = DefaultBatch
	}
	return &Indexer{emb: emb, db: db, collection: collection, batch: batch}, nil
}

// ensure opens the collection once with the dimension of the first vector; the error is returned on later calls too.
func (i *Indexer) ensure(ctx context.Context, dim int) error {
	i.once.Do(func() { i.ensured = i.db.EnsureCollection(ctx, i.collection, dim) })
	return i.ensured
}

// Index embeds and writes the posts with non-empty text in batches and returns the number of
// posts written. A post with empty text is skipped (there is no content to embed).
// If a batch fails, what was written so far is kept and the error is returned together with the count.
func (i *Indexer) Index(ctx context.Context, posts []Post) (int, error) {
	todo := make([]Post, 0, len(posts))
	for _, p := range posts {
		if p.ID != "" && strings.TrimSpace(p.Text) != "" {
			todo = append(todo, p)
		}
	}
	written := 0
	for start := 0; start < len(todo); start += i.batch {
		end := min(start+i.batch, len(todo))
		chunk := todo[start:end]
		texts := make([]string, len(chunk))
		for j, p := range chunk {
			texts[j] = p.Text
		}
		vecs, err := i.emb.Embed(ctx, texts, embedclient.Passage)
		if err != nil {
			return written, fmt.Errorf("postindex: embed: %w", err)
		}
		if len(vecs) != len(chunk) || len(vecs[0]) == 0 {
			return written, fmt.Errorf("postindex: got %d vectors for %d texts", len(vecs), len(chunk))
		}
		if err := i.ensure(ctx, len(vecs[0])); err != nil {
			return written, fmt.Errorf("postindex: collection: %w", err)
		}
		points := make([]vectordb.Point, len(chunk))
		for j, p := range chunk {
			points[j] = vectordb.Point{
				ID:      p.ID,
				Vector:  vecs[j],
				Payload: map[string]any{"author_id": p.AuthorID, "category": p.Category},
			}
		}
		if err := i.db.Upsert(ctx, i.collection, points); err != nil {
			return written, fmt.Errorf("postindex: write: %w", err)
		}
		written += len(chunk)
	}
	return written, nil
}

// SimilarRequest describes a search for posts similar to a text.
type SimilarRequest struct {
	Limit    int
	MinScore float64
	// If AuthorID is set, only that author's posts are searched.
	AuthorID string
	// If Category is set, only posts in that category are searched.
	Category string
}

// Similar embeds the text as a query and returns the most similar posts.
func (i *Indexer) Similar(ctx context.Context, text string, req SimilarRequest) ([]vectordb.Hit, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("postindex: empty query text")
	}
	vecs, err := i.emb.Embed(ctx, []string{text}, embedclient.Query)
	if err != nil {
		return nil, fmt.Errorf("postindex: embed: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil, errors.New("postindex: failed to get query vector")
	}
	match := map[string]any{}
	if req.AuthorID != "" {
		match["author_id"] = req.AuthorID
	}
	if req.Category != "" {
		match["category"] = req.Category
	}
	return i.db.Search(ctx, i.collection, vectordb.SearchRequest{
		Vector: vecs[0], Limit: req.Limit, MinScore: req.MinScore, Match: match,
	})
}

// VectorSource reads stored post vectors by ID. It satisfies curated/content.IPostVectorSource
// (vectors are converted to float64; the cosine computation runs in float64).
type VectorSource struct {
	db         vectordb.VectorDB
	collection string
}

// NewVectorSource builds the source that reads the collection collection.
func NewVectorSource(db vectordb.VectorDB, collection string) *VectorSource {
	return &VectorSource{db: db, collection: collection}
}

func (s *VectorSource) GetVectorsByPostIDs(ctx context.Context, postIDs []string) (map[string][]float64, error) {
	raw, err := s.db.GetVectors(ctx, s.collection, postIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]float64, len(raw))
	for id, v := range raw {
		f := make([]float64, len(v))
		for k, x := range v {
			f[k] = float64(x)
		}
		out[id] = f
	}
	return out, nil
}
