package postentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"serendipity/internal/trfold"
)

// Matcher returns the canonical entity names occurring in the text. The interface is defined on the CONSUMER
// side; the concrete impl is canonicaldict.Cache and this package does NOT carry
// an import dependency on it.
//
// nil may be passed but is a no-longer-supported operating mode —
// when the extractor was made a gate over the matcher at startup, accepting nil
// was a tolerance to prevent a crash (it does no scanning,
// empty labels). Now that the error word is PRINTED at code startup and a nil
// matcher is rejected, this nil check is unnecessary — it remains only for
// backward compatibility and is never taken.
type Matcher interface {
	Match(text string) []string
}

// DefaultNERBatch is the number of posts put into a single gRPC call.
//
// WHY IT EXISTS (C2): per round POST_ENTITY_MAX_PER_RUN (default 500, and
// `0` means UNLIMITED) posts were put into a SINGLE ExtractBatch call. The measured
// NER cost is 17 s/1000 posts — 500 posts ≈ 8.5 s, while the
// ENTITY_NER_TIMEOUT default is 5 s. So with the DEFAULT configuration, the moment the flag is
// turned on every round would return fail-closed with DEADLINE_EXCEEDED and no post would be tagged,
// including today's WORKING dictionary-only extraction.
//
// The fix is NOT to enlarge the timeout but to chunk: no fixed timeout can save an unlimited
// MAX_PER_RUN. 100 posts ≈ 1.7 s — leaves ~3x margin in a
// 5 s budget.
const DefaultNERBatch = 100

// Config is the extraction behavior.
type Config struct {
	MaxEntities int // max entities to keep per post
	Workers     int // number of concurrent scans
	NERBatch    int // number of posts put into a single NER call (ENTITY_NER_BATCH)
}

// DefaultConfig is sensible defaults.
func DefaultConfig() Config {
	return Config{MaxEntities: 8, Workers: 4, NERBatch: DefaultNERBatch}
}

// INERSource abstracts the batch NER call. Concrete impl:
// *grpcclient.NERClient.
type INERSource interface {
	ExtractBatch(ctx context.Context, texts map[string]string) (map[string][]NERSpan, error)
}

// ExtractorOption is optional settings.
type ExtractorOption func(*Extractor)

// WithNER enables hybrid extraction. link may be nil (in Plan A rules 1/2
// are not applied, all NER surfaces pass raw).
func WithNER(src INERSource, link Linker) ExtractorOption {
	return func(e *Extractor) { e.ner = src; e.link = link }
}

// Extractor converts a group of post texts into entity JSON.
//
// NO LLM: scanning is pure CPU and memory work. So the error surface is also small —
// the only possible error is JSON production.
type Extractor struct {
	matcher Matcher
	cfg     Config
	logger  *slog.Logger
	ner     INERSource
	link    Linker
}

// NewExtractor fills zero/negative config fields with defaults.
func NewExtractor(m Matcher, cfg Config, logger *slog.Logger, opts ...ExtractorOption) *Extractor {
	d := DefaultConfig()
	if cfg.MaxEntities <= 0 {
		cfg.MaxEntities = d.MaxEntities
	}
	if cfg.Workers <= 0 {
		cfg.Workers = d.Workers
	}
	if cfg.NERBatch <= 0 {
		cfg.NERBatch = d.NERBatch
	}
	if logger == nil {
		logger = slog.Default()
	}
	e := &Extractor{matcher: m, cfg: cfg, logger: logger}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// ErrNERUnavailable reports that the WHOLE round was skipped without writing because the NER source
// returned an error (fail-closed). The caller counts it per round
// (post consumer: EntityScanSkipped{reason="ner_unavailable"}) — it is NOT WRITTEN to per-post
// ExtractTotal.
var ErrNERUnavailable = errors.New("postentity: ner-service unavailable")

// ExtractBatch processes a post_id → text map and returns a post_id → entities JSON
// map.
//
// A post with no match ENTERS the result as `{"entities":[],"topics":[]}`;
// the field existing ensures the post is not processed again.
//
// The only error is ErrNERUnavailable (wrapped); in that case the result is EMPTY.
func (e *Extractor) ExtractBatch(ctx context.Context, texts map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(texts))
	if len(texts) == 0 {
		return out, nil
	}

	// THE SCAN TEXT IS PREPARED IN ONE PLACE — both legs must see the SAME text.
	//
	// Previously the dictionary got PromptText(text) while NER got the RAW text. PromptText has
	// two jobs (prompt.go): it strips raw URLs and trims to MaxPromptRunes.
	// Both apply to NER EXACTLY as well:
	//   - URL: the rationale in prompt.go ("EVERY post that shares a link inflates the
	//     publisher into an entity") is even heavier for NER — spaCy tags domain
	//     names as ORG/PRODUCT, without even tripping the scanner's word-boundary
	//     condition.
	//   - Trimming: NER cost grows linearly with text length (the 17 s/1000
	//     posts measurement was taken with trimmed text), so this also fixes the cost
	//     side of C2.
	prepared := make(map[string]string, len(texts))
	for id, txt := range texts {
		prepared[id] = PromptText(txt)
	}

	var nerSpans map[string][]NERSpan
	if e.ner != nil {
		var err error
		nerSpans, err = e.extractNERChunked(ctx, prepared)
		if err != nil {
			// FAIL-CLOSED: an empty label cannot be taken back (the scanner only scans a post that does NOT have an
			// entities field). The counterpart of the empty_dict gate — counting
			// and logging is done by the caller (the scan round).
			return map[string]string{}, fmt.Errorf("%w: %w", ErrNERUnavailable, err)
		}
	}

	type job struct {
		postID string
		text   string
	}
	jobs := make(chan job)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for range e.cfg.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				payload, ok := e.extractOne(j.postID, j.text, nerSpans[j.postID])
				if !ok {
					continue
				}
				mu.Lock()
				out[j.postID] = payload
				mu.Unlock()
			}
		}()
	}

	for id, txt := range prepared {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return map[string]string{}, ctx.Err()
		case jobs <- job{postID: id, text: txt}:
		}
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return map[string]string{}, err
	}
	return out, nil
}

// extractOne scans a single text. If the second return value is false the post
// is left untagged (retried). spans carries this post's surfaces when NER is on
// (always empty if e.ner is nil).
//
// text HAS ALREADY gone through PromptText (in ExtractBatch, SHARED with the NER leg)
// — it is not applied a second time here.
func (e *Extractor) extractOne(postID, text string, spans []NERSpan) (string, bool) {
	tags := Tags{Entities: []string{}, Topics: []string{}}

	if e.matcher != nil {
		// The trimmed text is scanned: so that scan cost does not grow
		// unboundedly with post length.
		seen := make(map[string]struct{})
		for _, name := range e.matcher.Match(text) {
			key := trfold.Fold(name)
			if _, dup := seen[key]; dup {
				continue // the same entity appeared multiple times
			}
			if len(tags.Entities) >= e.cfg.MaxEntities {
				break
			}
			seen[key] = struct{}{}
			tags.Entities = append(tags.Entities, name)
		}
	}

	names := tags.Entities
	if e.ner != nil {
		names = MergeHybridAtText(text, names, spans, e.link)
	}
	// TRIMMING CUTS FROM THE TAIL. MergeHybrid deliberately gives its output in PRIORITY
	// ORDER (linked canonicals first, then raw NER
	// surfaces), so that in a post with 8 surfaces + 8 canonicals rule 4's
	// hashtag rescue survives. The rationale is in MergeHybrid's doc comment.
	if len(names) > e.cfg.MaxEntities {
		names = names[:e.cfg.MaxEntities]
	}
	tags.Entities = names

	payload, err := json.Marshal(tags)
	if err != nil {
		ExtractTotal.WithLabelValues("error").Inc()
		e.logger.Warn("could not produce entity JSON (post stays untagged)",
			slog.String("post_id", postID),
			slog.String("error", err.Error()))
		return "", false
	}

	EntitiesPerPost.Observe(float64(len(tags.Entities)))
	if len(tags.Entities) > 0 {
		ExtractTotal.WithLabelValues("tagged").Inc()
	} else {
		ExtractTotal.WithLabelValues("empty").Inc()
	}
	return string(payload), true
}

// extractNERChunked splits the NER call into FIXED-SIZE slices.
//
// Why slices (C2): the number of posts put into a single call grows with POST_ENTITY_MAX_PER_RUN
// and that value being `0` (UNLIMITED) is a documented configuration.
// With the measured cost of 17 s/1000 posts, a single call exceeded ENTITY_NER_TIMEOUT
// even at default settings. Slicing makes the timeout budget INDEPENDENT
// of the post count.
//
// EACH SLICE GETS ITS OWN TIMEOUT: the budget is applied inside grpcclient.NERClient,
// per call with context.WithTimeout — this function's ctx
// is only the RUN budget (entityRunBudget) and puts an upper bound on the total duration
// of the slices.
//
// A SINGLE SLICE ERROR DROPS THE WHOLE ROUND (fail-closed): writing a partial result would
// mark the posts in the failed slice with `{"entities":[]}` and make them NEVER
// scanned again (the scanner only scans a post that does NOT have an entities field).
// The existing contract is kept as is.
func (e *Extractor) extractNERChunked(ctx context.Context, texts map[string]string) (map[string][]NERSpan, error) {
	size := e.cfg.NERBatch
	if size <= 0 {
		size = DefaultNERBatch
	}

	out := make(map[string][]NERSpan, len(texts))
	chunk := make(map[string]string, size)
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		// Each RPC gets its own immutable input map. Even if INERSource runs synchronously,
		// reusing the map by clearing it afterwards would produce a data race in a future
		// implementation that keeps a reference (A5).
		request := chunk
		chunk = make(map[string]string, size)
		res, err := e.ner.ExtractBatch(ctx, request)
		if err != nil {
			return err
		}
		for id, spans := range res {
			out[id] = spans
		}
		return nil
	}

	for id, txt := range texts {
		chunk[id] = txt
		if len(chunk) >= size {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}
