package speakerattr

import (
	"context"
	"log"
	"maps"
	"sync"
)

// confidenceSink collects the confidence of each line a chapter's decide
// batches choose (the chosen candidate's share of the candidates'
// probability), across batches running at once. Nil-safe: a nil sink
// records nothing.
type confidenceSink struct {
	mu sync.Mutex
	p  map[int]float64
}

func (s *confidenceSink) record(idx int, p float64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.p[idx] = p
}

func (s *confidenceSink) snapshot() map[int]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.p)
}

// attributeRedecided is AttributeChapterWith for AttributeOptions.
// RedecideBelow: the chapter is decided once, then again with every line
// decided at or above the threshold settled, so the lines left are each
// chosen with confident answers on both sides of them - within one decide
// reply a line only sees the answers before it. Up to RedecideRounds
// rounds (default 1), each settling the lines the last one decided
// confidently, stopping early when a round changes nothing. Measured
// before this existed (seed + decide, eval set): lines below 0.9 were 20%
// of the decided ones and held 64% of decide's errors, 35% of them wrong.
// A round that fails or pauses keeps the answers so far.
func (c *Client) attributeRedecided(ctx context.Context, bookTitle, chapterTitle string, roster Roster, paragraphs []ParagraphInput, shouldPause func() bool, opts AttributeOptions) (map[int]string, map[int]int, map[string]bool, map[string]string, []ParagraphInput, error) {
	first := opts
	first.RedecideBelow, first.Decide = 0, true
	sink := &confidenceSink{p: map[int]float64{}}
	firstRoster := roster
	firstRoster.confidence = sink
	out, indexes, roles, pronouns, remaining, err := c.AttributeChapterWith(ctx, bookTitle, chapterTitle, firstRoster, paragraphs, shouldPause, first)
	if err != nil || len(remaining) > 0 {
		return out, indexes, roles, pronouns, remaining, err
	}
	// The lines the rules settle are settled in every round; later rounds
	// don't seed again. Under seed-in-reply a settled line - seeded, or
	// decided confidently - stays asked with its answer written into the
	// reply (ParagraphInput.Forced), as the first pass had it; otherwise
	// it is shown settled (Known).
	inReply := opts.SeedInReply || c.cfg.Seed && !opts.Seed
	switch {
	case inReply:
		paragraphs = forcedFromSeeds(seedFromTags(paragraphs, roster))
	case opts.Seed:
		paragraphs = seedFromTags(paragraphs, roster)
	}
	merged, mergedIndexes, mergedRoles := maps.Clone(out), maps.Clone(indexes), maps.Clone(roles)
	if mergedIndexes == nil {
		mergedIndexes = map[int]int{}
	}
	if mergedRoles == nil {
		mergedRoles = map[string]bool{}
	}
	conf := sink.snapshot()
	again := first
	again.Seed, again.SeedInReply = false, false
	rounds := max(opts.RedecideRounds, 1)
	for round := 1; round <= rounds; round++ {
		settled := append([]ParagraphInput(nil), paragraphs...)
		unsure := 0
		for i, p := range settled {
			if !askedLine(p) || p.Forced != "" {
				continue
			}
			// A line decide sent back to generation (its mass check) has
			// no confidence and is asked again too, as is an Unknown
			// however confident: settled, it would be shown as a known
			// speaker of "Unknown".
			if pc, ok := conf[p.Idx]; ok && pc >= opts.RedecideBelow && !isSentinel(merged[p.Idx]) {
				if inReply {
					settled[i].Forced = merged[p.Idx]
				} else {
					settled[i].Known, settled[i].KnownIndex = merged[p.Idx], mergedIndexes[p.Idx]
				}
				continue
			}
			unsure++
		}
		if unsure == 0 {
			break
		}
		roundSink := &confidenceSink{p: map[int]float64{}}
		roundRoster := roster
		roundRoster.confidence = roundSink
		got, gotIndexes, gotRoles, _, rem, err := c.AttributeChapterWith(ctx, bookTitle, chapterTitle, roundRoster, settled, shouldPause, again)
		if err != nil || len(rem) > 0 {
			log.Printf("speakerattr: redecide: round %d of %q didn't finish (%v), keeping the answers so far", round, chapterTitle, err)
			break
		}
		changed := 0
		for _, p := range settled {
			if !askedLine(p) {
				continue
			}
			name, ok := got[p.Idx]
			if !ok {
				continue
			}
			if name != merged[p.Idx] {
				changed++
			}
			merged[p.Idx] = name
			delete(mergedIndexes, p.Idx)
			if n := gotIndexes[p.Idx]; n > 0 {
				mergedIndexes[p.Idx] = n
			}
			if gotRoles[name] {
				mergedRoles[name] = true
			}
		}
		maps.Copy(conf, roundSink.snapshot())
		log.Printf("speakerattr: redecide: %q round %d: %d lines below %.2f decided again, %d changed", chapterTitle, round, unsure, opts.RedecideBelow, changed)
		if changed == 0 {
			break
		}
	}
	return merged, mergedIndexes, mergedRoles, pronouns, nil, nil
}
