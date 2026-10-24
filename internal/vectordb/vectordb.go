// Package vectordb holds the small contract in front of the vector database and the Qdrant
// adapter. Readers do not know the database is Qdrant; another store can
// implement the same interface.
package vectordb

import "context"

// Point is a single record written to a collection. ID is the application's own (string) identifier;
// the store converts it to its own ID format.
type Point struct {
	ID      string
	Vector  []float32
	Payload map[string]any
}

// Hit is a single search result.
type Hit struct {
	ID      string
	Score   float64 // cosine similarity; larger = closer
	Payload map[string]any
}

// SearchRequest describes a similarity search.
type SearchRequest struct {
	Vector []float32
	Limit  int
	// MinScore: if greater than zero, results below it are dropped.
	MinScore float64
	// Match is the AND of payload field == value conditions (no filter if empty).
	Match map[string]any
}

// VectorDB is a vector store with cosine similarity.
type VectorDB interface {
	// EnsureCollection creates a cosine collection of dimension dim if it does not exist;
	// returns an error if it exists with a different dimension.
	EnsureCollection(ctx context.Context, name string, dim int) error
	// Upsert writes the points; a record with the same ID is overwritten.
	Upsert(ctx context.Context, collection string, points []Point) error
	// Search returns the most similar points in descending score order.
	Search(ctx context.Context, collection string, req SearchRequest) ([]Hit, error)
	// GetVectors reads the vectors of the IDs in bulk. IDs not in the collection are
	// omitted from the result (not an error).
	GetVectors(ctx context.Context, collection string, ids []string) (map[string][]float32, error)
	// Delete removes the IDs; a missing ID is not an error.
	Delete(ctx context.Context, collection string, ids []string) error
}
