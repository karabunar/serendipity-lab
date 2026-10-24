package postmeta

import (
	"context"
	"strconv"
	"testing"

	"serendipity/internal/core"
)

func TestGetUntagged_CountsCoverageCorrectly(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.SaveBatch(ctx, []core.RawPost{
		{PostID: "p1", Text: "metin bir"},
		{PostID: "p2", Text: "metin iki"},
		{PostID: "p3", Text: ""}, // textless — can never be tagged
	}); err != nil {
		t.Fatal(err)
	}
	// p2 is already tagged.
	if err := s.SetEntitiesBatch(ctx, map[string]string{"p2": `{"entities":["X"],"topics":[]}`}); err != nil {
		t.Fatal(err)
	}

	// p4 does not exist at all (HASH not created) — counts toward Total, counted as NoText.
	untagged, cov, err := s.GetUntagged(ctx, []string{"p1", "p2", "p3", "p4"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(untagged) != 1 || untagged["p1"] != "metin bir" {
		t.Fatalf("untagged = %v", untagged)
	}
	if cov.Total != 4 || cov.Tagged != 1 || cov.Untagged != 1 || cov.NoText != 2 {
		t.Fatalf("coverage = %+v", cov)
	}
}

func TestGetTextBatch_ReadsTaggedAndUntaggedPosts(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if err := s.SaveBatch(ctx, []core.RawPost{
		{PostID: "p1", Text: "eski metin"},
		{PostID: "p2", Text: "yeni metin"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEntitiesBatch(ctx, map[string]string{"p1": `{"entities":["Eski"],"topics":[]}`}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTextBatch(ctx, []string{"p1", "p2", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if got["p1"] != "eski metin" || got["p2"] != "yeni metin" {
		t.Fatalf("texts = %#v", got)
	}
	if _, ok := got["missing"]; ok {
		t.Fatal("missing post should not be returned")
	}
}

func TestSetEntitiesBatch_DoesNotCreateMissingHash(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()

	// A post that does not exist at all — a thin hash must not be created (same EXISTS gate
	// as the other batch setters; for posts that do not exist in the store).
	if err := s.SetEntitiesBatch(ctx, map[string]string{"yok": `{"entities":[],"topics":[]}`}); err != nil {
		t.Fatal(err)
	}
	if mr.Exists("serendipity:post:data:yok") {
		t.Fatal("HASH created for a nonexistent post")
	}
}

func TestSetEntitiesBatch_DoesNotTouchOtherFields(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()

	if err := s.SaveBatch(ctx, []core.RawPost{{PostID: "p1", AuthorID: "a1", Text: "t"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCategoryBatch(ctx, map[string]string{"p1": "spor"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEntitiesBatch(ctx, map[string]string{"p1": `{"entities":["Fenerbahçe"],"topics":[]}`}); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet("serendipity:post:data:p1", "category"); got != "spor" {
		t.Fatalf("category overwritten: %q", got)
	}
	if got := mr.HGet("serendipity:post:data:p1", "text"); got != "t" {
		t.Fatalf("text overwritten: %q", got)
	}
	if got := mr.HGet("serendipity:post:data:p1", "entities"); got != `{"entities":["Fenerbahçe"],"topics":[]}` {
		t.Fatalf("entities = %q", got)
	}
}

func TestSetEntitiesBatch_EmptyValueSkipped(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	if err := s.SaveBatch(ctx, []core.RawPost{{PostID: "p1", Text: "t"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEntitiesBatch(ctx, map[string]string{"p1": "", "": "x"}); err != nil {
		t.Fatal(err)
	}
	// (This miniredis version has no HExists; the same check is done with the HKeys membership
	// pattern in save_test.go.)
	fields, err := mr.HKeys("serendipity:post:data:p1")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fields {
		if f == "entities" {
			t.Fatal("empty value should not have been written")
		}
	}
}

func TestGetUntagged_EmptyListNoError(t *testing.T) {
	s, _ := newTestStore(t)
	untagged, cov, err := s.GetUntagged(context.Background(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(untagged) != 0 || cov.Total != 0 {
		t.Fatalf("untagged=%v cov=%+v", untagged, cov)
	}
}

func TestGetUntagged_WorksAboveChunkBoundary(t *testing.T) {
	// Queueing the whole keyspace into a single pipeline was unbounded on a large
	// keyspace; it must also give the correct result above the chunking limit.
	s, _ := newTestStore(t)
	ctx := context.Background()

	const n = untaggedChunk*2 + 37
	posts := make([]core.RawPost, 0, n)
	ids := make([]string, 0, n)
	for i := range n {
		id := "p" + strconv.Itoa(i)
		ids = append(ids, id)
		posts = append(posts, core.RawPost{PostID: id, Text: "metin " + id})
	}
	if err := s.SaveBatch(ctx, posts); err != nil {
		t.Fatal(err)
	}

	untagged, cov, err := s.GetUntagged(ctx, ids, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(untagged) != n {
		t.Fatalf("untagged = %d, want %d", len(untagged), n)
	}
	if cov.Total != n || cov.Untagged != n || cov.Tagged != 0 || cov.NoText != 0 {
		t.Fatalf("coverage = %+v", cov)
	}
}

func TestGetUntagged_CeilingDoesNotTrimCoverage(t *testing.T) {
	// The ceiling limits only the RETURNED texts. The coverage counts must see ALL
	// posts; otherwise the EntityPendingPosts gauge gets stuck at the ceiling
	// and the question "is coverage approaching zero" cannot be measured.
	s, _ := newTestStore(t)
	ctx := context.Background()

	const n = untaggedChunk + 500 // so the ceiling fills in the middle of a chunk
	posts := make([]core.RawPost, 0, n+1)
	ids := make([]string, 0, n+1)
	for i := range n {
		id := "p" + strconv.Itoa(i)
		ids = append(ids, id)
		posts = append(posts, core.RawPost{PostID: id, Text: "metin"})
	}
	// Textless post: the pass AFTER the ceiling is filled must count it as NoText, not Untagged
	// (otherwise the pending gauge plateaus forever).
	posts = append(posts, core.RawPost{PostID: "bosmetin", Text: ""})
	ids = append(ids, "bosmetin")
	if err := s.SaveBatch(ctx, posts); err != nil {
		t.Fatal(err)
	}

	untagged, cov, err := s.GetUntagged(ctx, ids, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(untagged) != 10 {
		t.Fatalf("returned text count = %d, want 10", len(untagged))
	}
	if cov.Total != n+1 || cov.Untagged != n || cov.NoText != 1 {
		t.Fatalf("coverage = %+v (cap truncated the coverage)", cov)
	}
}

func TestGetUntagged_TaggedPostTextNotFetched(t *testing.T) {
	// The first pass reads only `entities`; the body of a tagged post is NEVER
	// carried. Since miniredis gives no command counts, indirect proof:
	// we delete the `text` field and leave the HASH tagged — the old single-pass
	// code would still count it as "tagged", but would try to read the text.
	// The real lock is that the second pass runs ONLY over the candidate list:
	// below, since the tagged post never enters the candidate list its text
	// is not asked about and the coverage still comes out right.
	s, mr := newTestStore(t)
	ctx := context.Background()

	if err := s.SaveBatch(ctx, []core.RawPost{
		{PostID: "tagged", Text: "cok uzun bir govde"},
		{PostID: "fresh", Text: "kisa"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEntitiesBatch(ctx, map[string]string{"tagged": `{"entities":["X"],"topics":[]}`}); err != nil {
		t.Fatal(err)
	}
	// We corrupt the tagged post's text: if the second pass had read it, this
	// value would leak into the result.
	mr.HSet("serendipity:post:data:tagged", "text", "SIZINTI")

	untagged, cov, err := s.GetUntagged(ctx, []string{"tagged", "fresh"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := untagged["tagged"]; leaked {
		t.Fatal("text of a tagged post was fetched")
	}
	if len(untagged) != 1 || untagged["fresh"] != "kisa" {
		t.Fatalf("untagged = %v", untagged)
	}
	if cov.Total != 2 || cov.Tagged != 1 || cov.Untagged != 1 {
		t.Fatalf("coverage = %+v", cov)
	}
}

func TestGetEntitiesBatch_ReturnsCanonicalNames(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.SaveBatch(ctx, []core.RawPost{{PostID: "p1", Text: "t"}, {PostID: "p2", Text: "t"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEntitiesBatch(ctx, map[string]string{
		"p1": `{"entities":["Fenerbahçe","transfer"],"topics":["futbol"]}`,
		"p2": `{"entities":[],"topics":[]}`,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetEntitiesBatch(ctx, []string{"p1", "p2", "yok"})
	if err != nil {
		t.Fatal(err)
	}
	// Only `entities` is read; `topics` is NOT INCLUDED — the Fenerbahce/Besiktas
	// distinction is carried by proper names, a topic label falls to the "football" resolution.
	if len(got["p1"]) != 2 || got["p1"][0] != "Fenerbahçe" {
		t.Fatalf("p1 = %v", got["p1"])
	}
	// An empty entity list and a post that does not exist DO NOT APPEAR IN THE RESULT: so the caller
	// does not have to distinguish "this post has no entities" from "this post is unknown";
	// in both the contribution is zero.
	if _, ok := got["p2"]; ok {
		t.Fatalf("empty entity list must not appear in the result: %v", got)
	}
	if _, ok := got["yok"]; ok {
		t.Fatalf("nonexistent post must not appear in the result: %v", got)
	}
}

func TestGetEntitiesBatch_CorruptJSONSkipped(t *testing.T) {
	// A manually corrupted field or one left over from an old version must not
	// drop the whole batch (fail-soft).
	s, mr := newTestStore(t)
	ctx := context.Background()
	if err := s.SaveBatch(ctx, []core.RawPost{{PostID: "p1", Text: "t"}, {PostID: "p2", Text: "t"}}); err != nil {
		t.Fatal(err)
	}
	mr.HSet("serendipity:post:data:p1", "entities", "{bozuk")
	if err := s.SetEntitiesBatch(ctx, map[string]string{"p2": `{"entities":["A"],"topics":[]}`}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetEntitiesBatch(ctx, []string{"p1", "p2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["p1"]; ok {
		t.Fatalf("corrupt record appeared in the result: %v", got)
	}
	if len(got["p2"]) != 1 {
		t.Fatalf("healthy record was affected: %v", got)
	}
}

func TestGetEntitiesBatch_EmptyListAndChunking(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	got, err := s.GetEntitiesBatch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %v", got)
	}

	// Input exceeding the chunking limit (untaggedChunk = 1000).
	ids := make([]string, 0, untaggedChunk+5)
	posts := make([]core.RawPost, 0, untaggedChunk+5)
	payload := map[string]string{}
	for i := range untaggedChunk + 5 {
		id := "p" + strconv.Itoa(i)
		ids = append(ids, id)
		posts = append(posts, core.RawPost{PostID: id, Text: "t"})
		payload[id] = `{"entities":["A"],"topics":[]}`
	}
	if err := s.SaveBatch(ctx, posts); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEntitiesBatch(ctx, payload); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetEntitiesBatch(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ids) {
		t.Fatalf("chunking lost data: %d/%d", len(got), len(ids))
	}
}

func TestGetEntityMeta_ReturnsAuthorAndEntityTogether(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()

	mr.HSet("serendipity:post:data:p1", "author_id", "u9")
	mr.HSet("serendipity:post:data:p1", "entities", `{"entities":["Fenerbahçe"],"topics":[]}`)
	mr.HSet("serendipity:post:data:p2", "author_id", "u7")
	mr.HSet("serendipity:post:data:p2", "entities", `{"entities":[],"topics":[]}`)

	got, err := s.GetEntityMeta(ctx, []string{"p1", "p2", "yok"})
	if err != nil {
		t.Fatalf("GetEntityMeta: %v", err)
	}
	if got["p1"].AuthorID != "u9" || len(got["p1"].Entities) != 1 {
		t.Fatalf("p1 = %+v", got["p1"])
	}
	// A post without entities STILL RETURNS: author_id is needed for the author quota and self-exclusion;
	// an empty entity list is harmless for the veto.
	if got["p2"].AuthorID != "u7" || len(got["p2"].Entities) != 0 {
		t.Fatalf("p2 = %+v", got["p2"])
	}
	if _, ok := got["yok"]; ok {
		t.Fatal("nonexistent post appeared in the result")
	}
}

func TestGetEntityMeta_CorruptJSONDoesNotDropPost(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()

	mr.HSet("serendipity:post:data:p1", "author_id", "u9")
	mr.HSet("serendipity:post:data:p1", "entities", "{bozuk")

	got, err := s.GetEntityMeta(ctx, []string{"p1"})
	if err != nil {
		t.Fatalf("GetEntityMeta: %v", err)
	}
	// A corrupt entities field empties the entity list but does not
	// lose author_id — the candidate must still be able to pass the author quota and self-exclusion
	// (fail-soft).
	if got["p1"].AuthorID != "u9" || len(got["p1"].Entities) != 0 {
		t.Fatalf("p1 = %+v", got["p1"])
	}
}

func TestGetEntityMeta_EmptyInput(t *testing.T) {
	s, _ := newTestStore(t)
	got, err := s.GetEntityMeta(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetEntityMeta: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %v", got)
	}
}
