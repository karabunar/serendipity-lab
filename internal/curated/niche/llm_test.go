package niche

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

func llmServer(t *testing.T, status int, body string, rec *map[string]any, auth *string) *LLMCategorizer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if rec != nil {
			_ = json.Unmarshal(raw, rec)
		}
		if auth != nil {
			*auth = r.Header.Get("Authorization")
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("wrong path: %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := NewLLMCategorizer(srv.URL+"/", "model-x", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const okBody = `{"choices":[{"message":{"content":"{\"categories\":[\"Fenerbahçe\",\"yazılım\"]}"}}]}`

func TestCategorize_SendsGuidedJSONAndParses(t *testing.T) {
	var rec map[string]any
	c := llmServer(t, 200, okBody, &rec, nil)
	got, err := c.Categorize(context.Background(), []string{"bir", "iki"}, 3)
	if err != nil || len(got) != 2 || got[0] != "Fenerbahçe" {
		t.Fatalf("wrong categories: %v %v", got, err)
	}
	if rec["model"] != "model-x" || rec["temperature"].(float64) != 0 || rec["guided_json"] == nil {
		t.Fatalf("wrong request body: %v", rec)
	}
	user := rec["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "- bir\n- iki") {
		t.Fatalf("texts must be sent as bullet points: %q", user)
	}
}

func TestCategorize_ClampsToMaxAndDedups(t *testing.T) {
	body := `{"choices":[{"message":{"content":"{\"categories\":[\"a\",\"A\",\"b\",\"c\",\"d\"]}"}}]}`
	got, err := llmServer(t, 200, body, nil, nil).Categorize(context.Background(), []string{"x"}, 2)
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("must deduplicate and truncate to 2: %v %v", got, err)
	}
}

func TestCategorize_AuthHeaderOnlyWhenKeySet(t *testing.T) {
	var auth string
	c := llmServer(t, 200, okBody, nil, &auth)
	_, _ = c.Categorize(context.Background(), []string{"x"}, 3)
	if auth != "" {
		t.Fatalf("no Authorization header expected without a key: %q", auth)
	}
	c.APIKey = "  gizli "
	_, _ = c.Categorize(context.Background(), []string{"x"}, 3)
	if auth != "Bearer gizli" {
		t.Fatalf("Bearer must be sent with the trimmed key: %q", auth)
	}
}

func TestCategorize_Failures(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"http error":      {500, "boom", "http 500"},
		"empty choices":   {200, `{"choices":[]}`, "empty choices"},
		"missing content": {200, `{"choices":[{"message":{}}]}`, "content missing"},
		"non-json":        {200, `{"choices":[{"message":{"content":"merhaba"}}]}`, "not JSON"},
		"bad body":        {200, `<html>`, "decode"},
	}
	for name, tc := range cases {
		_, err := llmServer(t, tc.status, tc.body, nil, nil).Categorize(context.Background(), []string{"x"}, 3)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: expected an error containing %q, got %v", name, tc.want, err)
		}
	}
}

func TestCategorize_EmptyTextsNoCallAndConstructorValidation(t *testing.T) {
	c := llmServer(t, 500, "boom", nil, nil)
	if got, err := c.Categorize(context.Background(), nil, 3); err != nil || got != nil {
		t.Fatalf("a call without texts must not send a request: %v %v", got, err)
	}
	if _, err := NewLLMCategorizer("", "m", "", 0); err == nil {
		t.Fatal("empty base URL was accepted")
	}
	if _, err := NewLLMCategorizer("http://x", " ", "", 0); err == nil {
		t.Fatal("empty model was accepted")
	}
}

func contentBody(content string) string {
	b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": content}}}})
	return string(b)
}

// sequenceServer answers each call with the next body and records every request.
func sequenceServer(t *testing.T, bodies []string, recs *[]map[string]any) *LLMCategorizer {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var rec map[string]any
		_ = json.Unmarshal(raw, &rec)
		*recs = append(*recs, rec)
		body := bodies[min(n, len(bodies)-1)]
		n++
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := NewLLMCategorizer(srv.URL, "model-x", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCategorize_AcceptsJSONWrappedInProse(t *testing.T) {
	cases := map[string]string{
		"code fence": "```json\n{\"categories\": [\"Kubernetes\", \"Redis\"]}\n```",
		"prose":      "Elbette! {\"categories\": [\"Kubernetes\", \"Redis\"]} umarım yardımcı olur.",
		"bare array": "[\"Kubernetes\", \"Redis\"]",
	}
	for name, content := range cases {
		var recs []map[string]any
		c := sequenceServer(t, []string{contentBody(content)}, &recs)
		got, err := c.Categorize(context.Background(), []string{"x"}, 3)
		if err != nil || len(got) != 2 || got[0] != "Kubernetes" {
			t.Errorf("%s: want [Kubernetes Redis], got %v %v", name, got, err)
		}
		if len(recs) != 1 {
			t.Errorf("%s: a parseable answer must not be retried (%d calls)", name, len(recs))
		}
	}
}

func TestCategorize_RetriesOnceWhenTheFirstAnswerIsBroken(t *testing.T) {
	var recs []map[string]any
	c := sequenceServer(t, []string{contentBody(`{"categories": ["Fener`), okBody}, &recs)
	got, err := c.Categorize(context.Background(), []string{"x"}, 3)
	if err != nil || len(got) != 2 {
		t.Fatalf("the retry should succeed: %v %v", got, err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 calls, got %d", len(recs))
	}
	if recs[0]["temperature"].(float64) != 0 || recs[1]["temperature"].(float64) == 0 {
		t.Fatalf("first call deterministic, retry with some temperature: %v %v", recs[0]["temperature"], recs[1]["temperature"])
	}
	user := recs[1]["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "categories") {
		t.Fatalf("the retry must remind the model of the JSON shape: %q", user)
	}
}

func TestCategorize_RetriesWhenTheFirstAnswerHasNoCategories(t *testing.T) {
	var recs []map[string]any
	c := sequenceServer(t, []string{contentBody(`{"categories": []}`), okBody}, &recs)
	got, err := c.Categorize(context.Background(), []string{"x"}, 3)
	if err != nil || len(got) != 2 || len(recs) != 2 {
		t.Fatalf("want the second answer after one retry: %v %v (%d calls)", got, err, len(recs))
	}
}

func TestCategorize_GivesUpAfterTheRetryBudget(t *testing.T) {
	var recs []map[string]any
	c := sequenceServer(t, []string{contentBody("hmm")}, &recs)
	_, err := c.Categorize(context.Background(), []string{"x"}, 3)
	if err == nil || !strings.Contains(err.Error(), "not JSON") || len(recs) != 2 {
		t.Fatalf("want a 'not JSON' error after exactly 2 calls: %v (%d calls)", err, len(recs))
	}
}

func TestCategorize_HTTPErrorsAreNotRetried(t *testing.T) {
	var recs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recs = append(recs, nil)
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)
	c, _ := NewLLMCategorizer(srv.URL, "m", "", time.Second)
	if _, err := c.Categorize(context.Background(), []string{"x"}, 3); err == nil || len(recs) != 1 {
		t.Fatalf("an HTTP error is returned right away: %v (%d calls)", err, len(recs))
	}
}
