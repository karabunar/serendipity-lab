package entitylink

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	goredis "github.com/redis/go-redis/v9"
)

// BuilderConfig holds the corpus linker's safety and cost limits.
type BuilderConfig struct {
	MinCanonicalCount int
	MaxMappings       int
}

// DefaultBuilderConfig does not canonicalize singleton and weak corpus surfaces.
func DefaultBuilderConfig() BuilderConfig {
	return BuilderConfig{MinCanonicalCount: 2}
}

// BuildMappings produces a deterministic alias map from post:data.entities frequencies.
// There are two deliberate rules:
//  1. The more frequent whole word/phrase absorbs the longer phrase that contains it.
//  2. A long stem with a Turkish suffix of at most three letters merges with the more frequent stem.
func BuildMappings(counts map[string]int, cfg BuilderConfig) map[string]string {
	if cfg.MinCanonicalCount <= 0 {
		cfg.MinCanonicalCount = 2
	}
	type termInfo struct {
		key, display string
		count        int
		tokens       []string
	}
	terms := make(map[string]*termInfo)
	for raw, count := range counts {
		key := normalize(raw)
		if key == "" || count <= 0 || utf8.RuneCountInString(key) > maxAliasRunes {
			continue
		}
		if old := terms[key]; old != nil {
			old.count += count
			if displayLess(raw, old.display) {
				old.display = strings.TrimSpace(raw)
			}
			continue
		}
		terms[key] = &termInfo{key: key, display: strings.TrimSpace(raw), count: count, tokens: strings.Fields(key)}
	}

	direct := make(map[string]string)
	for _, source := range terms {
		best := ""
		bestCount := 0
		bestTokens := 0
		// Inclusion: only WHOLE token sequences inside the source are candidates.
		for i := range source.tokens {
			for j := i + 1; j <= len(source.tokens); j++ {
				if j-i == len(source.tokens) {
					continue
				}
				candidate := terms[strings.Join(source.tokens[i:j], " ")]
				if candidate == nil || candidate.count < cfg.MinCanonicalCount || candidate.count <= source.count {
					continue
				}
				if betterCandidate(candidate.key, candidate.count, j-i, best, bestCount, bestTokens) {
					best, bestCount, bestTokens = candidate.key, candidate.count, j-i
				}
			}
		}
		// Suffix: filistinli -> filistin. Single token only and stem >=5 runes.
		if len(source.tokens) == 1 {
			runes := []rune(source.key)
			for extra := 1; extra <= 3 && len(runes)-extra >= minSuffixRunes; extra++ {
				baseKey := string(runes[:len(runes)-extra])
				candidate := terms[baseKey]
				if candidate == nil || candidate.count < cfg.MinCanonicalCount || candidate.count <= source.count {
					continue
				}
				if betterCandidate(candidate.key, candidate.count, 1, best, bestCount, bestTokens) {
					best, bestCount, bestTokens = candidate.key, candidate.count, 1
				}
			}
		}
		if best != "" && best != source.key {
			direct[source.key] = best
		}
	}

	// Collapse chains to the root; if there is a cycle, give up on that candidate.
	resolved := make(map[string]string, len(direct))
	for source := range direct {
		seen := map[string]struct{}{source: {}}
		current := direct[source]
		for {
			next, ok := direct[current]
			if !ok {
				break
			}
			if _, cycle := seen[next]; cycle {
				current = ""
				break
			}
			seen[next] = struct{}{}
			current = next
		}
		if current == "" || current == source {
			continue
		}
		if target := terms[current]; target != nil {
			resolved[source] = target.display
		}
	}

	if cfg.MaxMappings > 0 && len(resolved) > cfg.MaxMappings {
		type ranked struct {
			key, value string
			count      int
		}
		items := make([]ranked, 0, len(resolved))
		for key, value := range resolved {
			items = append(items, ranked{key: key, value: value, count: terms[key].count})
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].count != items[j].count {
				return items[i].count > items[j].count
			}
			return items[i].key < items[j].key
		})
		resolved = make(map[string]string, cfg.MaxMappings)
		for _, item := range items[:cfg.MaxMappings] {
			resolved[item.key] = item.value
		}
	}
	return resolved
}

func betterCandidate(key string, count, tokenCount int, best string, bestCount, bestTokens int) bool {
	if count != bestCount {
		return count > bestCount
	}
	if tokenCount != bestTokens {
		return tokenCount > bestTokens
	}
	return best == "" || key < best
}

func displayLess(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if b == "" {
		return true
	}
	return a < b
}

// RedisWriter atomically refreshes the linker map via a temporary HASH + RENAME.
// A reader sees either the old map or the new one, never a half-written HASH.
type RedisWriter struct {
	rdb    goredis.Cmdable
	logger *slog.Logger
	seq    atomic.Uint64
}

func NewRedisWriter(rdb goredis.Cmdable, logger *slog.Logger) *RedisWriter {
	if logger == nil {
		logger = slog.Default()
	}
	return &RedisWriter{rdb: rdb, logger: logger}
}

// Replace atomically swaps the given map into the target HASH.
func (w *RedisWriter) Replace(ctx context.Context, mappings map[string]string) error {
	clean := make(map[string]string, len(mappings))
	for alias, canonical := range mappings {
		alias = normalize(alias)
		canonical = strings.TrimSpace(canonical)
		if alias == "" || canonical == "" || alias == normalize(canonical) {
			continue
		}
		clean[alias] = canonical
	}
	if len(clean) == 0 {
		if err := w.rdb.Del(ctx, Key).Err(); err != nil {
			return fmt.Errorf("entitylink clear: %w", err)
		}
		MappingCount.Set(0)
		BuildRuns.WithLabelValues("empty").Inc()
		return nil
	}

	tmp := fmt.Sprintf("%s:tmp:%d:%d", Key, time.Now().UnixNano(), w.seq.Add(1))
	args := make([]any, 0, len(clean)*2)
	for alias, canonical := range clean {
		args = append(args, alias, canonical)
	}
	if err := w.rdb.HSet(ctx, tmp, args...).Err(); err != nil {
		BuildRuns.WithLabelValues("error").Inc()
		return fmt.Errorf("entitylink temp hash: %w", err)
	}
	if err := w.rdb.Rename(ctx, tmp, Key).Err(); err != nil {
		_ = w.rdb.Del(ctx, tmp).Err()
		BuildRuns.WithLabelValues("error").Inc()
		return fmt.Errorf("entitylink atomic rename: %w", err)
	}
	meta, _ := json.Marshal(map[string]any{"built_at": time.Now().UTC().Format(time.RFC3339), "mapping_count": len(clean)})
	if err := w.rdb.Set(ctx, MetaKey, meta, 0).Err(); err != nil {
		w.logger.Warn("could not write entity linker metadata", slog.Any("error", err))
	}
	MappingCount.Set(float64(len(clean)))
	BuildRuns.WithLabelValues("success").Inc()
	w.logger.Info("entity linker map refreshed atomically", slog.Int("mapping_count", len(clean)))
	return nil
}
