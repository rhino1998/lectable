package speakerattr

import (
	"context"
	"fmt"
	"sort"
)

// widenContextRadii is AttributeWithWideningContext's schedule: how many
// paragraphs either side of a still-rejected target each successive retry
// round includes. The last radius (161 lines at most) still leaves an
// attribution reply well inside attributeMaxTokens - see
// maxWidenWindowChars for the input-side bound.
var widenContextRadii = []int{20, 40, 80}

// maxWidenWindowChars bounds one retry window's own paragraph text, so a
// chapter of unusually long paragraphs stops growing a window before it
// crowds out DefaultNCtx (roughly 4 chars/token puts this near 10k tokens
// of input, alongside systemPrompt and attributeMaxTokens of output).
const maxWidenWindowChars = 40000

// AttributeWithWideningContext re-attributes targets (paragraph Idx
// values, each present in all) whose earlier attribution came back as a
// name reject refuses - Auto Split's own follow-up for lines the model
// keeps handing straight back to the speaker being eliminated, or to a
// name already marked invalid (see httpapi.reattributeChapterSpeaker).
//
// AttributeChapter sees each line only inside whichever fixed
// maxBatchParagraphs batch it happened to land in, so a line near a batch
// edge can be missing exactly the turn-taking or narration tag that
// identifies its speaker. Each round here instead sends a window centred
// on every still-rejected target (widenContextRadii: 20, then 40, then 80
// paragraphs either side, clipped to the chapter and to
// maxWidenWindowChars) as one un-split batch, and keeps any target whose
// fresh answer reject accepts. Targets close together share one window per
// round rather than each paying for their own call. A target stops
// growing once its window can't get any bigger (it already spans the
// whole chapter, or hit the character budget) - there's no more context
// left to add.
//
// out holds only accepted results, already normalized and canonicalized
// the same way AttributeChapter's are; a target still rejected after the
// last round is simply absent, so the caller's own fallback (Unknown)
// applies. roleNames mirrors AttributeChapter's own. err is the first
// window's failure, if any - rounds stop there, returning whatever was
// already accepted.
func (c *Client) AttributeWithWideningContext(ctx context.Context, bookTitle, chapterTitle string, knownCharacters []string, knownDescriptions map[string]string, knownRoles map[string]bool, all []ParagraphInput, targets []int, reject func(speaker string) bool) (out map[int]string, roleNames map[string]bool, err error) {
	out = make(map[int]string, len(targets))
	roleNames = map[string]bool{}

	posByIdx := make(map[int]int, len(all))
	for i, p := range all {
		posByIdx[p.Idx] = i
	}
	pending := make([]int, 0, len(targets))
	for _, idx := range targets {
		if pos, ok := posByIdx[idx]; ok {
			pending = append(pending, pos)
		}
	}
	sort.Ints(pending)

	// lastSpan is each pending target's previous window, so a round that
	// can't widen it any further drops the target instead of repeating
	// the identical prompt.
	type span struct{ lo, hi int }
	lastSpan := map[int]span{}

	for _, radius := range widenContextRadii {
		if len(pending) == 0 || ctx.Err() != nil {
			break
		}
		var stillPending []int
		for i := 0; i < len(pending); {
			lo, hi := widenWindow(all, pending[i], radius)
			if prev, ok := lastSpan[pending[i]]; ok && prev.lo == lo && prev.hi == hi {
				i++
				continue
			}
			// Every pending target inside this window rides along on the
			// same call.
			var covered []int
			for ; i < len(pending) && pending[i] <= hi; i++ {
				covered = append(covered, pending[i])
			}

			window := all[lo : hi+1]
			attributions, batchErr := c.attributeBatch(ctx, bookTitle, chapterTitle, knownCharacters, knownDescriptions, knownRoles, window)
			if batchErr != nil {
				return out, roleNames, fmt.Errorf("widened paragraphs %d-%d: %w", window[0].Idx, window[len(window)-1].Idx, batchErr)
			}
			raw := make(map[int]string, len(covered))
			for _, pos := range covered {
				idx := all[pos].Idx
				a, ok := attributions[idx]
				if !ok {
					continue
				}
				speaker := normalizeBarePronoun(a.Speaker)
				raw[idx] = disallowNarratorForDialogue(all[pos].IsQuote, speaker)
			}
			canon := canonicalizeSpeakerNames(knownCharacters, raw)
			for _, pos := range covered {
				idx := all[pos].Idx
				name, ok := canon[idx]
				if !ok || name == "" || reject(name) {
					lastSpan[pos] = span{lo, hi}
					stillPending = append(stillPending, pos)
					continue
				}
				out[idx] = name
				if attributions[idx].Role && name != "Narrator" && name != "Unknown" {
					roleNames[name] = true
				}
			}
		}
		pending = stillPending
	}
	return out, roleNames, nil
}

// widenWindow returns the [lo, hi] positions (inclusive) of a window
// around center reaching up to radius paragraphs either side, grown one
// paragraph at a time alternating sides so a maxWidenWindowChars cutoff
// still leaves center roughly in the middle, not all context on one side.
func widenWindow(all []ParagraphInput, center, radius int) (lo, hi int) {
	lo, hi = center, center
	chars := len(all[center].Text)
	for step := 1; step <= radius; step++ {
		grew := false
		if l := center - step; l >= 0 {
			if chars+len(all[l].Text) > maxWidenWindowChars {
				break
			}
			chars += len(all[l].Text)
			lo = l
			grew = true
		}
		if h := center + step; h < len(all) {
			if chars+len(all[h].Text) > maxWidenWindowChars {
				break
			}
			chars += len(all[h].Text)
			hi = h
			grew = true
		}
		if !grew {
			break
		}
	}
	return lo, hi
}
