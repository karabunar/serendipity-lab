package vectordb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type recorded struct {
	method, path, apiKey string
	body                 map[string]any
}

// fakeQdrant is a fake server that records requests and returns the given response.
func fakeQdrant(t *testing.T, handler func(r recorded) (int, string)) (*Qdrant, *[]recorded) {
	t.Helper()
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := recorded{method: r.Method, path: r.URL.RequestURI(), apiKey: r.Header.Get("api-key")}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &rec.body)
		}
		got = append(got, rec)
		code, body := handler(rec)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewQdrant(srv.URL, "anahtar", time.Second), &got
}

func TestPointID_DeterministicUUIDv5(t *testing.T) {
	a, b := PointID("post-1"), PointID("post-1")
	if a != b {
		t.Fatalf("same ID produced different UUIDs: %s %s", a, b)
	}
	if PointID("post-2") == a {
		t.Fatal("different IDs collided")
	}
	if len(a) != 36 || a[14] != '5' {
		t.Fatalf("malformed UUIDv5: %s", a)
	}
}

func TestEnsureCollection_CreatesWhenMissing(t *testing.T) {
	q, got := fakeQdrant(t, func(r recorded) (int, string) {
		if r.method == http.MethodGet {
			return 404, `{}`
		}
		return 200, `{"result":true}`
	})
	if err := q.EnsureCollection(context.Background(), "posts", 384); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 2 || (*got)[1].method != http.MethodPut || (*got)[1].path != "/collections/posts" {
		t.Fatalf("beklenmeyen istekler: %+v", *got)
	}
	v := (*got)[1].body["vectors"].(map[string]any)
	if v["size"].(float64) != 384 || v["distance"] != "Cosine" {
		t.Fatalf("wrong collection definition: %v", v)
	}
	if (*got)[0].apiKey != "anahtar" {
		t.Fatal("api-key header was not sent")
	}
}

func TestEnsureCollection_ExistingSameDimIsNoop(t *testing.T) {
	q, got := fakeQdrant(t, func(recorded) (int, string) {
		return 200, `{"result":{"config":{"params":{"vectors":{"size":384,"distance":"Cosine"}}}}}`
	})
	if err := q.EnsureCollection(context.Background(), "posts", 384); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 {
		t.Fatalf("a write request was sent for an existing collection: %+v", *got)
	}
}

func TestEnsureCollection_DimMismatch(t *testing.T) {
	q, _ := fakeQdrant(t, func(recorded) (int, string) {
		return 200, `{"result":{"config":{"params":{"vectors":{"size":768}}}}}`
	})
	err := q.EnsureCollection(context.Background(), "posts", 384)
	if err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("expected a dimension mismatch error: %v", err)
	}
}

func TestEnsureCollection_InvalidDim(t *testing.T) {
	q, _ := fakeQdrant(t, func(recorded) (int, string) { return 200, `{}` })
	if err := q.EnsureCollection(context.Background(), "posts", 0); err == nil {
		t.Fatal("zero dimension was accepted")
	}
}

func TestUpsert_SendsUUIDAndKeepsOriginalID(t *testing.T) {
	q, got := fakeQdrant(t, func(recorded) (int, string) { return 200, `{"result":{}}` })
	err := q.Upsert(context.Background(), "posts", []Point{{ID: "p1", Vector: []float32{1, 0}, Payload: map[string]any{"author": "a"}}})
	if err != nil {
		t.Fatal(err)
	}
	r := (*got)[0]
	if r.method != http.MethodPut || r.path != "/collections/posts/points?wait=true" {
		t.Fatalf("wrong request: %s %s", r.method, r.path)
	}
	p := r.body["points"].([]any)[0].(map[string]any)
	if p["id"] != PointID("p1") {
		t.Fatalf("wrong UUID: %v", p["id"])
	}
	pl := p["payload"].(map[string]any)
	if pl["_id"] != "p1" || pl["author"] != "a" {
		t.Fatalf("wrong payload: %v", pl)
	}
}

func TestUpsert_EmptyIsNoopAndEmptyIDFails(t *testing.T) {
	q, got := fakeQdrant(t, func(recorded) (int, string) { return 200, `{}` })
	if err := q.Upsert(context.Background(), "posts", nil); err != nil || len(*got) != 0 {
		t.Fatalf("empty upsert must not send a request: %v %+v", err, *got)
	}
	if err := q.Upsert(context.Background(), "posts", []Point{{ID: "", Vector: []float32{1}}}); err == nil {
		t.Fatal("empty ID was accepted")
	}
}

func TestSearch_MapsHitsAndSendsFilter(t *testing.T) {
	q, got := fakeQdrant(t, func(recorded) (int, string) {
		return 200, `{"result":[{"id":"u1","score":0.9,"payload":{"_id":"p1","author":"a"}},{"id":"u2","score":0.5,"payload":{"author":"b"}}]}`
	})
	hits, err := q.Search(context.Background(), "posts", SearchRequest{
		Vector: []float32{1, 0}, Limit: 5, MinScore: 0.4, Match: map[string]any{"author": "a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].ID != "p1" || hits[0].Score != 0.9 || hits[0].Payload["author"] != "a" {
		t.Fatalf("wrong results: %+v", hits)
	}
	if _, leaked := hits[0].Payload["_id"]; leaked {
		t.Fatal("_id leaked into the payload")
	}
	if hits[1].ID != "u2" {
		t.Fatalf("Qdrant ID must be returned when _id is absent: %+v", hits[1])
	}
	b := (*got)[0].body
	if b["limit"].(float64) != 5 || b["score_threshold"].(float64) != 0.4 || b["with_payload"] != true {
		t.Fatalf("wrong search body: %v", b)
	}
	must := b["filter"].(map[string]any)["must"].([]any)[0].(map[string]any)
	if must["key"] != "author" || must["match"].(map[string]any)["value"] != "a" {
		t.Fatalf("wrong filter: %v", must)
	}
}

func TestSearch_EmptyVectorAndMissingCollection(t *testing.T) {
	q, _ := fakeQdrant(t, func(recorded) (int, string) { return 404, `{}` })
	if _, err := q.Search(context.Background(), "posts", SearchRequest{}); err == nil {
		t.Fatal("empty vector was accepted")
	}
	if _, err := q.Search(context.Background(), "yok", SearchRequest{Vector: []float32{1}}); err == nil ||
		!strings.Contains(err.Error(), "collection not found") {
		t.Fatalf("expected a missing collection error: %v", err)
	}
}

func TestSearch_ServerErrorIncludesStatus(t *testing.T) {
	q, _ := fakeQdrant(t, func(recorded) (int, string) { return 500, `boom` })
	_, err := q.Search(context.Background(), "posts", SearchRequest{Vector: []float32{1}})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("status code must be in the error: %v", err)
	}
}

func TestDelete_SendsUUIDs(t *testing.T) {
	q, got := fakeQdrant(t, func(recorded) (int, string) { return 200, `{}` })
	if err := q.Delete(context.Background(), "posts", []string{"p1", "p2"}); err != nil {
		t.Fatal(err)
	}
	pts := (*got)[0].body["points"].([]any)
	if len(pts) != 2 || pts[0] != PointID("p1") {
		t.Fatalf("wrong delete body: %v", pts)
	}
	if err := q.Delete(context.Background(), "posts", nil); err != nil || len(*got) != 1 {
		t.Fatal("empty delete must not send a request")
	}
}

func TestGetVectors_MapsByOriginalID(t *testing.T) {
	q, got := fakeQdrant(t, func(recorded) (int, string) {
		return 200, `{"result":[{"id":"u1","vector":[0.1,0.2],"payload":{"_id":"p1"}},{"id":"u2","vector":[],"payload":{"_id":"p2"}},{"id":"u3","vector":[1],"payload":{}}]}`
	})
	out, err := q.GetVectors(context.Background(), "posts", []string{"p1", "p2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || len(out["p1"]) != 2 {
		t.Fatalf("only points with a vector and _id must be returned: %v", out)
	}
	r := (*got)[0]
	if r.method != http.MethodPost || r.path != "/collections/posts/points" || r.body["with_vector"] != true {
		t.Fatalf("wrong request: %s %s %v", r.method, r.path, r.body)
	}
	if pts := r.body["ids"].([]any); pts[0] != PointID("p1") {
		t.Fatalf("UUID was not sent: %v", pts)
	}
}

func TestGetVectors_EmptyAndMissingCollection(t *testing.T) {
	q, got := fakeQdrant(t, func(recorded) (int, string) { return 404, `{}` })
	out, err := q.GetVectors(context.Background(), "posts", nil)
	if err != nil || len(out) != 0 || len(*got) != 0 {
		t.Fatalf("empty input must not send a request: %v %v", out, err)
	}
	if _, err := q.GetVectors(context.Background(), "yok", []string{"p1"}); err == nil {
		t.Fatal("missing collection must return an error")
	}
}
