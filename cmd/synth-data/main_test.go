package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"serendipity/internal/interactions"
)

func TestWriteJSONL_RoundTripsThroughSeedFormat(t *testing.T) {
	ts := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	err := writeJSONL(&buf, []interactions.RawInteraction{
		{UserID: "u1", PostID: "p1", AuthorID: "a1", InteractionType: "like", Timestamp: ts},
		{UserID: "u1", PostID: "p2", InteractionType: "skip", DwellMs: 250, Timestamp: ts},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"timestamp":"2025-06-15T12:00:00Z"`) || !strings.Contains(lines[1], `"dwell_ms":250`) {
		t.Fatalf("wrong JSONL format: %q", buf.String())
	}
	if strings.Contains(lines[1], "author_id") {
		t.Fatalf("empty author_id must not be written: %s", lines[1])
	}
}
