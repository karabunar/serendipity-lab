package niche

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const systemInstruction = `Sen bir sosyal medya platformu için kullanıcı ilgi alanı analistisin. ` +
	`Sana bir kullanıcının son etkileşimde bulunduğu gönderilerin metinleri verilecek. ` +
	`Bu kullanıcının en fazla 3 niş ilgi alanını (2-4 kelimelik Türkçe kategoriler, ` +
	`örn. "Fenerbahçe", "yazılım", "kültür sanat") belirle.`

// responseSchema is the schema given to vLLM guided decoding: the output is always valid JSON in this shape.
var responseSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"categories": map[string]any{
			"type":     "array",
			"items":    map[string]any{"type": "string"},
			"maxItems": MaxCategories,
		},
	},
	"required": []string{"categories"},
}

// LLMCategorizer sends a request that forces schema-conforming JSON to an OpenAI-compatible chat-completion endpoint
// (local vLLM). If you do not want to go out to an external provider, point BaseURL at a local server.
type LLMCategorizer struct {
	BaseURL string
	Model   string
	APIKey  string // optional; if empty the Authorization header is never written
	Client  *http.Client
}

var _ Categorizer = (*LLMCategorizer)(nil)

// NewLLMCategorizer requires baseURL and model.
func NewLLMCategorizer(baseURL, model, apiKey string, timeout time.Duration) (*LLMCategorizer, error) {
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(model) == "" {
		return nil, errors.New("niche: LLM base URL and model are required")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &LLMCategorizer{BaseURL: baseURL, Model: model, APIKey: apiKey, Client: &http.Client{Timeout: timeout}}, nil
}

// maxAttempts is how many times a malformed or empty answer is retried. Only the answer is
// retried (transport and HTTP errors are returned as they are): a small model occasionally
// breaks the JSON shape, and a second try with a nudge usually fixes it.
const maxAttempts = 2

func (c *LLMCategorizer) Categorize(ctx context.Context, texts []string, max int) ([]string, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		content, err := c.request(ctx, texts, attempt)
		if err != nil {
			return nil, err
		}
		cats, perr := parseCategories(content)
		if perr != nil {
			// If it is not JSON, the server is running without guided decoding; it is not swallowed silently.
			lastErr = perr
			continue
		}
		cleaned := cleanCategories(cats, max)
		if len(cleaned) > 0 || attempt == maxAttempts-1 {
			return cleaned, nil
		}
	}
	return nil, fmt.Errorf("local llm: content is not JSON (guided decoding may be disabled): %w", lastErr)
}

// request sends one chat-completion call and returns the assistant message content. The first
// attempt is deterministic; a retry adds a reminder and a little temperature so it does not
// reproduce the same broken answer.
func (c *LLMCategorizer) request(ctx context.Context, texts []string, attempt int) (string, error) {
	var b strings.Builder
	for _, t := range texts {
		b.WriteString("- ")
		b.WriteString(t)
		b.WriteString("\n")
	}
	temperature := 0.0
	if attempt > 0 {
		b.WriteString("\nSadece {\"categories\": [...]} biçiminde JSON döndür.")
		temperature = 0.3
	}
	body := map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemInstruction},
			{"role": "user", "content": b.String()},
		},
		"temperature": temperature,
		"max_tokens":  128,
		"guided_json": responseSchema,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(c.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("local llm http %d: %s", resp.StatusCode, truncate(string(payload), 200))
	}
	var parsed struct {
		Choices []struct {
			Message *struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return "", fmt.Errorf("local llm: failed to decode response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("local llm: empty choices")
	}
	msg := parsed.Choices[0].Message
	if msg == nil || msg.Content == nil {
		return "", errors.New("local llm: choices[0].message.content missing")
	}
	return *msg.Content, nil
}

// parseCategories reads the categories from the model's answer. The strict shape is
// {"categories": [...]}; because a model without guided decoding often wraps it in prose or a
// code fence, the first JSON object (or, failing that, the first JSON array of strings) found in
// the text is accepted too.
func parseCategories(content string) ([]string, error) {
	var strict struct {
		Categories []string `json:"categories"`
	}
	err := json.Unmarshal([]byte(content), &strict)
	if err == nil {
		return strict.Categories, nil
	}
	if i, j := strings.Index(content, "{"), strings.LastIndex(content, "}"); i >= 0 && j > i {
		if json.Unmarshal([]byte(content[i:j+1]), &strict) == nil {
			return strict.Categories, nil
		}
	}
	if i, j := strings.Index(content, "["), strings.LastIndex(content, "]"); i >= 0 && j > i {
		var arr []string
		if json.Unmarshal([]byte(content[i:j+1]), &arr) == nil {
			return arr, nil
		}
	}
	return nil, err
}
