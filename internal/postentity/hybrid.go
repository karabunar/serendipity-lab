package postentity

import (
	"strings"

	"serendipity/internal/entitylink"
	"serendipity/internal/trfold"
)

// CompositeLinker combines the primary dictionary (canonicaldict) with the corpus linker.
// The primary linker wins on an exact match; the secondary linker only completes surfaces
// not found in the primary dictionary. If the second snapshot is empty,
// behavior stays identical to the dictionary-only Plan A.
type CompositeLinker struct {
	Primary   Linker
	Secondary Linker
}

func (c CompositeLinker) Canonical(term string) (string, bool) {
	if c.Primary != nil {
		if value, ok := c.Primary.Canonical(term); ok {
			return value, true
		}
	}
	if c.Secondary != nil {
		return c.Secondary.Canonical(term)
	}
	return "", false
}

func (c CompositeLinker) Match(text string) []string {
	var out []string
	seen := make(map[string]struct{})
	appendUnique := func(values []string) {
		for _, value := range values {
			key := normalizeKey(value)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, value)
		}
	}
	if c.Primary != nil {
		appendUnique(c.Primary.Match(text))
	}
	if c.Secondary != nil {
		appendUnique(c.Secondary.Match(text))
	}
	return out
}

// MatchLocated merges the located matches of both linkers.
// Matches of a linker without the located interface are skipped here; in that case MergeHybrid
// uses the backward-compatible approximate suppression path.
func (c CompositeLinker) MatchLocated(text string) []entitylink.LocatedMatch {
	var out []entitylink.LocatedMatch
	type locatedKey struct {
		canonical string
		start     int
		end       int
	}
	seen := make(map[locatedKey]struct{})
	appendUnique := func(values []entitylink.LocatedMatch) {
		for _, value := range values {
			key := normalizeKey(value.Canonical)
			if key == "" {
				continue
			}
			locationKey := locatedKey{canonical: key, start: value.Start, end: value.End}
			if _, ok := seen[locationKey]; ok {
				continue
			}
			seen[locationKey] = struct{}{}
			out = append(out, value)
		}
	}
	if located, ok := c.Primary.(entitylink.LocatedMatcher); ok {
		appendUnique(located.MatchLocated(text))
	}
	if located, ok := c.Secondary.(entitylink.LocatedMatcher); ok {
		appendUnique(located.MatchLocated(text))
	}
	return out
}

// NERSpan is a single surface returned by ner-service.
type NERSpan struct {
	Text  string
	Label string
	Score float64
	Start int // [Start, End), Unicode rune offset in the prepared text
	End   int
}

// Linker looks up the dictionary counterpart of a surface.
//
// Why an interface: postentity does NOT WANT to import canonicaldict — that package
// would drag in go-redis too. *canonicaldict.Cache already satisfies this interface.
type Linker interface {
	Canonical(term string) (string, bool) // is the WHOLE surface an alias
	Match(text string) []string           // is there an alias INSIDE the surface
}

// normalizeKey reduces a surface to the key space.
func normalizeKey(s string) string {
	if i := strings.IndexAny(s, "'’"); i > 0 {
		s = s[:i]
	}
	return strings.TrimSpace(trfold.Fold(s))
}

// MergeHybrid merges dictionary matches with NER surfaces by the FOUR rules
// in the design:
//
//  1. The NER span matches an alias EXACTLY        → the dictionary's canonical
//  2. The NER span is attached to an alias INSIDE it → the NER's surface
//  3. The NER span has no dictionary counterpart    → the NER's surface
//  4. The dictionary found it, NER never saw it     → the dictionary's canonical
//
// Rule 2 CUTS IDENTITY CONFUSION: in measurement 11.3% of linked spans
// collapsed onto an alias inside the span instead of the whole span ("Osmanli
// Imparatorlugu" → Fatih Terim, nickname "Imparator"). If the span is longer
// than the alias, the span wins.
//
// ⚠️ RULE 2 PRODUCES A SUPPRESSION SET. The span winning ALONE was not enough:
// the dictionary scans THE SAME TEXT independently and returns the same inner alias with its own canonical,
// and that canonical was added unconditionally in the rule 4 loop. The result would be
// the wrong canonical NEXT TO the right surface — i.e. the identity confusion that was supposed to be cut
// was not cut:
//
//	dictionary: ["Recep Tayyip Erdoğan"]   NER: ["Bilal Erdoğan"]
//	  BEFORE: ["bilal erdogan", "Recep Tayyip Erdoğan"]   ← confusion remains
//	  NOW:    ["bilal erdogan"]
//
// Therefore the result of link.Match(surface) is now written not only to the metric but also to the suppression
// set: rule 4 does NOT add those canonicals.
//
// SUPPRESSION IS APPROXIMATE — a deliberate trade-off. The Span message in ner.proto
// HAS NO CHARACTER OFFSET IN OLD IMPLEMENTATIONS (the new proto has start/end), so
// the distinction "was the dictionary's match INSIDE this span, or somewhere ELSE
// in the text" could not be made with old clients. A new client uses the span
// range; when working with an old client the same approximate suppression is
// kept. If the same canonical legitimately appeared elsewhere in the text it may be
// lost on the old path; the new path limits this loss by position.
// The new proto contract has start/end offsets; the production path uses them.
// If an old NER implementation does not return offsets, the backward-compatible
// approximate suppression is kept.
//
// Rule 4 IS THE HASHTAG RESCUE: NER cannot see Gazze inside "#GazzeyiUnutma",
// the dictionary can (372 matches/1000 posts in measurement).
//
// OUTPUT ORDER IS BY PRIORITY, not pass order: first LINKED CANONICALS
// (rule 1 + rule 4), then RAW NER SURFACES (rule 2 + rule 3). The reason is
// downstream: the caller trims MaxEntities from the TAIL. With pass order
// (spans first, then dictionary), in a post with 8 surfaces + 8 canonicals
// ALL of the dictionary canonicals were dropped — i.e. the hashtag rescue that rule 4
// is specifically justified by systematically disappeared
// in trimming. A linked canonical is worth more than a raw surface: in the inverted index it is
// a SHARED key, while a raw surface stays singleton in measurement at a rate of 89%
// and Slot4 cannot produce candidates from a singleton key.
//
// The output is DEDUPED. If link is nil, 1 and 2 cannot be applied and all NER surfaces pass
// raw (the situation that arises when the dictionary is empty).
func MergeHybrid(dictNames []string, spans []NERSpan, link Linker) []string {
	return mergeHybrid("", dictNames, spans, link)
}

// MergeHybridAtText computes rule 2 suppression by whether the dictionary match was actually
// inside the NER span. text must be the same as the prepared text the NER service
// took its offsets from.
func MergeHybridAtText(text string, dictNames []string, spans []NERSpan, link Linker) []string {
	return mergeHybrid(text, dictNames, spans, link)
}

func mergeHybrid(text string, dictNames []string, spans []NERSpan, link Linker) []string {
	// Two passes (A7, spec §10): the span loop only collects DECISIONS, it does not write to the
	// `seen` dedup. Writing is done in priority order — canonicals first,
	// then raw surfaces — so a raw NER surface that folds onto the same key cannot
	// push the dictionary canonical into the low-priority bucket. In the single-pass version
	// the span loop ran first, so `MergeHybrid(["TÜGVA","Gazze"],
	// [{Text:"TÜGVA"}])` → returned ["Gazze","tugva"].
	var linked, rawSurfaces []string // rule 1 · rule 2 + 3
	// suppressed is the dictionary canonicals suppressed by rule 2 (normalized key).
	var suppressed map[string]struct{}

	for _, sp := range spans {
		surface := normalizeKey(sp.Text)
		if surface == "" {
			continue
		}
		if link == nil {
			rawSurfaces = append(rawSurfaces, surface) // rule 3 (no dictionary)
			continue
		}
		if canonical, ok := link.Canonical(surface); ok {
			MergeRule.WithLabelValues("rule1_linked").Inc()
			linked = append(linked, canonical) // rule 1
			continue
		}
		if inner := link.Match(surface); len(inner) > 0 {
			// Rule 2: the span wins AND the canonical(s) the dictionary would
			// return for the same inner alias are suppressed.
			MergeRule.WithLabelValues("rule2_span_wins").Inc()
			if suppressed == nil {
				suppressed = make(map[string]struct{}, len(inner))
			}
			for _, c := range suppressedCanonicals(text, sp, inner, link) {
				if k := normalizeKey(c); k != "" {
					suppressed[k] = struct{}{}
				}
			}
		} else {
			MergeRule.WithLabelValues("rule3_unknown_surface").Inc()
		}
		rawSurfaces = append(rawSurfaces, surface)
	}

	seen := make(map[string]struct{}, len(spans)+len(dictNames))
	// Two buckets: since trimming cuts from the tail, the output order determines priority.
	canonicals := make([]string, 0, len(dictNames)+len(linked)) // rule 1 + 4
	surfaces := make([]string, 0, len(rawSurfaces))             // rule 2 + 3

	add := func(bucket *[]string, name string) bool {
		k := normalizeKey(name)
		if k == "" {
			return false
		}
		if _, dup := seen[k]; dup {
			return false
		}
		seen[k] = struct{}{}
		*bucket = append(*bucket, name)
		return true
	}

	// nerKeys is the keys NER SAW in this post (rule 1 canonicals +
	// raw surfaces). Rule 4 is only the dictionary addition OUTSIDE these
	// — "the dictionary found it, NER never saw it" (hashtag rescue).
	nerKeys := make(map[string]struct{}, len(linked)+len(rawSurfaces))
	for _, n := range linked {
		nerKeys[normalizeKey(n)] = struct{}{}
	}
	for _, n := range rawSurfaces {
		nerKeys[n] = struct{}{} // already normalized
	}

	for _, name := range linked {
		_ = add(&canonicals, name)
	}
	for _, name := range dictNames { // rule 4
		k := normalizeKey(name)
		if _, blocked := suppressed[k]; blocked {
			// The dictionary may have extracted this canonical from INSIDE the span; rule 2
			// already wrote the span. Writing both is identity confusion.
			MergeRule.WithLabelValues("rule2_dict_suppressed").Inc()
			continue
		}
		if add(&canonicals, name) {
			if _, nerSaw := nerKeys[k]; !nerSaw {
				MergeRule.WithLabelValues("rule4_dict_only").Inc()
			}
		}
	}
	for _, name := range rawSurfaces {
		_ = add(&surfaces, name)
	}
	return append(canonicals, surfaces...)
}

func suppressedCanonicals(text string, span NERSpan, inner []string, link Linker) []string {
	located, ok := link.(entitylink.LocatedMatcher)
	if !ok || text == "" || span.Start < 0 || span.End <= span.Start {
		return inner
	}
	if span.End > len([]rune(text)) {
		return inner
	}
	wanted := make(map[string]struct{}, len(inner))
	for _, canonical := range inner {
		if key := normalizeKey(canonical); key != "" {
			wanted[key] = struct{}{}
		}
	}
	inside := make(map[string]bool, len(wanted))
	outside := make(map[string]bool, len(wanted))
	for _, match := range located.MatchLocated(text) {
		key := normalizeKey(match.Canonical)
		if _, ok := wanted[key]; !ok || match.End <= match.Start {
			continue
		}
		if match.Start >= span.Start && match.End <= span.End {
			inside[key] = true
		} else {
			outside[key] = true
		}
	}
	var out []string
	for _, canonical := range inner {
		key := normalizeKey(canonical)
		if inside[key] && !outside[key] {
			out = append(out, canonical)
		}
	}
	return out
}
