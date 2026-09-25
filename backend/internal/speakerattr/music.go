package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode"
)

// musicBatchParagraphs bounds one pass-1 (boundary) music-scoring
// completion request - directionBatchParagraphs' own sibling, picked a bit
// larger since a tone-region judgment benefits from more surrounding
// context than a per-line delivery-tag judgment does (deciding "has the
// mood genuinely shifted" needs to see a real stretch of the scene, not
// just one line at a time), while still keeping one malformed response
// from losing more than one batch's worth of a long chapter. A region may
// span batches: every batch after the first also sees the end of the one
// before it (musicSeamContextLines), so the model decides whether the tone
// already playing carries on across the seam.
const musicBatchParagraphs = 20

// musicSeamContextLines is how many numbered lines from the end of the
// previous batch each later batch is shown ahead of its own. Only text -
// the previous batch's result isn't needed - so batches stay independent
// and can run concurrently (musicParallelBatches).
//
// Each batch used to be judged blind and its first region forced to "cut",
// since nothing could vouch that it continued the one before. Measured on
// Spire's Spite (194 scored chapters): 2701 of 3001 stored cuts sat exactly
// on a 20-paragraph batch seam, 220 opened a chapter, and only 80 were
// cuts the model itself chose - so the music restarted in a fresh style
// every ~20 paragraphs, and those seam cuts were never merged away
// (mergeShortRegions keeps cuts).
const musicSeamContextLines = 4

// musicParallelBatches is how many pass-1 batches run at once - the
// worker's 2 LLM slots, attributeParallelBatches' sibling.
const musicParallelBatches = 2

// musicMaxTokens bounds one pass-1 batch's generated JSON - a batch of
// musicBatchParagraphs paragraphs collapses into only a handful of
// {"startIdx","transition"} pairs now that mood/prompt-writing is deferred
// to pass 2 (see ScoreMusic's own doc comment for why), so this is far
// smaller than the single-pass version's own old budget - kept generous
// anyway for a "hybrid thinking" model's own <think>...</think> trace
// ahead of the JSON.
const musicMaxTokens = 2048

// musicSystemPrompt asks only for each region's own start plus its
// transition - never an end, and (since ScoreMusic split into two passes -
// see its own doc comment) never a mood or a music generation prompt
// either. Starts-only keeps a chapter's regions contiguous and
// non-overlapping by construction (each one implicitly runs until the next
// one's own start, or the chapter's last paragraph for the last region -
// see store.MusicRegion.EndIdx's own doc comment), rather than something
// parseMusicBoundaries has to separately validate against gaps/overlaps
// the way an explicit per-region end once required.
//
// "cut" is tied to scene changes (a jump in time/place, a POV switch, a
// shock), "continuation" to mood shifts within a scene. An earlier version
// reserved cut for "a genuine musical shock" and defaulted to continuation
// when unsure - fine while batch seams forced a cut every ~20 paragraphs
// anyway, but once seams stopped doing that (musicSeamContextLines) the
// model marked ~1 cut per book in 200 chapters, and continuation-chained
// descriptions then kept one palette and one ambience for whole chapters
// that crossed several scenes.
const musicSystemPrompt = `You are scoring a novel's chapter for background instrumental music. You will be given a numbered list of paragraphs from one chapter (narration and/or spoken dialogue). Identify where the chapter's emotional/narrative tone shifts meaningfully (e.g. calm -> tense, banter -> grief, mundane -> suspenseful) and mark the paragraph where each new tone region begins. A region implicitly runs until the next region's own start (or the end of the chapter, for the last region) - never state where a region ends.

A region should span a real, sustained tone, not flicker per paragraph - prefer far fewer, far longer regions over many short ones. A single tense paragraph inside an otherwise calm scene does not need its own region unless the tension actually carries forward. Be very conservative: for a list of around 20 paragraphs, 1-2 regions is typical and 3 should already be unusual - treat 4 or more as a sign something went wrong, not a reasonable outcome. An entire scene - a whole back-and-forth conversation, a whole action beat, a whole stretch of narration, a whole run of reminiscence or description - is normally ONE region for its full span, no matter how much individual lines within it rise and fall in intensity, or how much the subject matter itself moves around (excitement building through a conversation, a joke landing inside a tense exchange, a character's train of thought drifting from one memory or topic to the next, a description moving from one detail to another). What decides a region's boundary is the *emotional register the music itself would play under* - calm, tense, warm, melancholy, urgent - never the subject matter: a character recalling five completely different memories in a row is still ONE region the whole time if all five are being recalled in the same wistful, reflective mood; a conversation that wanders across several unrelated topics is still ONE region as long as the *feeling* in the room doesn't change. Only mark a new region where that underlying feeling itself genuinely changes - not where the topic, setting, or subject changes on its own. If you're tempted to start a new region because a line moved on to something different, but the mood a composer would score it with is the same as the line before, don't - that's not a boundary.

Each numbered line below is already one complete narrated beat: when the original text had a quote immediately followed by its own short narration tag ("she said") or more dialogue right after it, that continuation text is folded directly onto its paragraph's own numbered line rather than given a number of its own. So every number you see already marks the start of a real, whole beat - you never need to identify or avoid a mid-beat line yourself.

Also judge each region's own transition. Use "cut" whenever a new scene begins: the story jumps in time or to a different place, the point of view switches to other characters, a scene-break marker appears, or something shocking erupts out of calm (a twist, sudden violence). A cut restarts the music with a fresh palette, so it belongs where a film would change scenes. Use "continuation" for a mood shift within the same scene - calm drifting into unease, tension slowly building, warmth cooling into melancholy - which a film score would simply evolve through. Always use "cut" for the very first region in the list, which has nothing before it to continue.

Reply with a compact JSON array on a single line only, no other text, no markdown code fence, ordered by paragraph index:

[{"startIdx": <int>, "transition": "cut" | "continuation"}, ...]

Rules:
- startIdx is the paragraph index number exactly as printed before that line below - never shift it.
- The first region's startIdx must equal the first paragraph number shown; each following region's startIdx must be strictly greater than the one before it, and no greater than the last paragraph number shown.
- Only "cut" or "continuation" are valid transition values - copy one verbatim.`

// MusicRegionResult is one tone region ScoreMusic judged - see
// store.MusicRegionInput, which this maps directly onto (kept as a plain
// value here rather than importing store, matching this package's
// existing rule - see ParagraphInput). Carries no end of its own - see
// musicSystemPrompt's own doc comment for why only a start is ever asked
// for or stored.
type MusicRegionResult struct {
	StartIdx int
	Mood     string
	Prompt   string // Stable Audio music prompt - see buildMusicPrompt
	// Ambience is the Stable Audio prompt for this region's ambient
	// soundscape layer (see buildAmbiencePrompt), mixed under the music -
	// "" when the region has no clear physical setting to hear.
	Ambience   string
	Transition string // "cut" or "continuation" - see musicSystemPrompt
}

// musicBoundary is pass 1's own narrower judgment - a region's start and
// transition, before pass 2 (describeMusicRegion) has filled in its own
// Mood/Prompt from that region's real paragraph span. Never exported: only
// ScoreMusic's own final, combined MusicRegionResult is a public shape.
type musicBoundary struct {
	StartIdx   int
	Transition string
	// split marks a boundary splitLongRegions inserted into one tone
	// region purely for length - ScoreMusic reuses the region's own
	// description for it rather than asking the model again.
	split bool
}

type musicBoundaryRaw struct {
	StartIdx   int    `json:"startIdx"`
	Transition string `json:"transition"`
}

// musicBoundaryBatch judges batch's region starts. seam is the tail of the
// previous batch (empty for a chapter's first batch), shown numbered ahead
// of batch so the model can tell whether the tone already playing carries
// on - see musicSeamContextLines.
func (c *Client) musicBoundaryBatch(ctx context.Context, bookTitle, chapterTitle string, seam, batch []ParagraphInput) ([]musicBoundary, error) {
	shown := batch
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nChapter: %s\n", bookTitle, chapterTitle)
	if len(seam) > 0 {
		shown = append(append([]ParagraphInput(nil), seam...), batch...)
		fmt.Fprintf(&user, "\nParagraphs %d-%d below are the end of the previous section, already scored - they're shown only so you can hear what's already playing. Your first region starts at %d and stands for that music carrying on. Mark a region at %d or later only where the feeling genuinely changes from it; its transition is judged against those earlier paragraphs too. If nothing changes, reply with just the one region.\n",
			seam[0].Idx, batch[0].Idx-1, seam[0].Idx, batch[0].Idx)
	}
	user.WriteString("\nParagraphs:\n")
	for i, p := range shown {
		// An Inline paragraph (a split-out continuation of the paragraph
		// right before it - e.g. a quote's own "she said" tag) gets no
		// number of its own: its text is folded directly onto its base
		// paragraph's line instead, so the model is never shown a
		// mid-beat line to (mis)identify as a region start in the first
		// place - see musicSystemPrompt. batch's own first paragraph is
		// never Inline (ScoreMusic never lets a batch start mid-run), so
		// this always has a preceding non-Inline line to append to.
		if p.Inline {
			fmt.Fprintf(&user, " %s", oneLine(p.Text))
			continue
		}
		if i > 0 {
			user.WriteString("\n\n")
		}
		fmt.Fprintf(&user, "%d: %s", p.Idx, oneLine(p.Text))
	}
	user.WriteString("\n")
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}

	return generateAndParse(ctx, c, musicSystemPrompt, user.String(), 0, musicMaxTokens, func(content string) ([]musicBoundary, error) {
		return parseMusicBoundaries(content, shown, batch[0].Idx)
	})
}

// parseMusicBoundaries is parseAttributions' own sibling for this pass's
// JSON array shape - same fence-stripping/unmarshalRepairedArray
// tolerance - plus a hard ordering check (see musicSystemPrompt's own doc
// comment): every region after the first must strictly increase and stay
// within the batch, or the whole batch is treated as a parse failure and
// retried (generateAndParse/retryParse) rather than persisting a region
// set that could leave a gap or overlap once EndIdx is later derived from
// these starts (store.MusicRegion.EndIdx's own doc comment) - there's no
// endIdx left to validate directly any more, but a strictly-increasing,
// batch-bounded run of starts is exactly what guarantees that derivation
// can never produce one.
//
// shown is the seam context (see musicSeamContextLines) followed by the
// batch itself, which starts at batchStart. Only boundaries at or after
// batchStart are returned: the first region stands for whatever was
// already playing, so a batch whose tone carries straight on returns none,
// and a change the model placed inside the context moves to batchStart.
//
// The *first* region is corrected, not rejected, when it doesn't start
// exactly at shown's own first paragraph: a real, observed, 100%-
// reproducible failure (confirmed live: 28/28 attempts across many
// retries all failed the same way, permanently failing the task every
// single time - no amount of retrying a sampling-noise-only failure could
// ever fix it) for a batch whose own first paragraph(s) are trivial front
// matter (a bare chapter-number heading like "Chapter 93" or "93") that
// the model reasonably declines to anchor its own first tone judgment on,
// instead starting from the first paragraph with real content. Pulling
// the model's own first startIdx back to batchStart folds that skipped
// front matter into whatever region the model did choose - the correct
// outcome, since it still needs *some* region (there's no earlier one in
// this batch to fall back on), and a heading line's own background music
// is never going to be a meaningfully different judgment anyway.
//
// Also guards against a region ever starting on an Inline continuation
// line (see musicSystemPrompt) - a real, observed failure mode (confirmed
// live: a chapter fragmented into a new region every single
// quote+narration-tag pair, each only a couple of seconds of real
// narration) from a first version of this prompt/listing that numbered
// every paragraph individually and only asked the model not to start a
// region on a continuation. musicBoundaryBatch no longer shows an Inline
// paragraph its own number at all (its text is folded onto its base
// paragraph's line instead), so the model has no number to name for one
// in the first place; the inline map/drop below is now just a defensive
// backstop against a hallucinated startIdx that happens to land on one -
// silently dropping it rather than failing the batch, since an Inline
// paragraph by definition isn't a valid region boundary regardless of
// what the model reports. batch[i].Inline is the ground truth this checks
// against.
func parseMusicBoundaries(content string, shown []ParagraphInput, batchStart int) ([]musicBoundary, error) {
	shownStart := shown[0].Idx
	hasSeam := shownStart < batchStart
	inline := make(map[int]bool, len(shown))
	shownEnd := shownStart
	for _, p := range shown {
		if p.Inline {
			inline[p.Idx] = true
		} else {
			shownEnd = p.Idx
		}
	}

	start := strings.IndexByte(content, '[')
	end := strings.LastIndexByte(content, ']')
	if start == -1 || end == -1 || end < start {
		return nil, fmt.Errorf("no JSON array found in response: %s", truncate(content, 200))
	}
	var raw []musicBoundaryRaw
	if err := unmarshalRepairedArray(content[start:end+1], &raw); err != nil {
		return nil, fmt.Errorf("parse JSON array: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("no regions returned for paragraphs %d-%d", shownStart, shownEnd)
	}

	out := make([]musicBoundary, 0, len(raw))
	prevStart := -1
	// seamChange is a change the model placed inside the seam context
	// (after its first, carried-over region) - that tone is what's playing
	// when this batch starts, so it lands on batchStart instead.
	seamChange := ""
	for i, r := range raw {
		if i == 0 {
			// See this function's own doc comment - corrected, not
			// rejected, so a batch opening on trivial front matter (a
			// bare chapter-number heading) doesn't permanently fail no
			// matter how many times it's retried.
			r.StartIdx = shownStart
		} else if r.StartIdx <= prevStart {
			return nil, fmt.Errorf("region startIdx %d does not strictly increase from previous %d", r.StartIdx, prevStart)
		}
		if r.StartIdx > shownEnd {
			return nil, fmt.Errorf("region startIdx %d beyond paragraphs %d-%d", r.StartIdx, shownStart, shownEnd)
		}
		prevStart = r.StartIdx
		if inline[r.StartIdx] {
			continue
		}
		// Defaults to the now-preferred "continuation" for anything the
		// model didn't clearly mark "cut" - see musicSystemPrompt's own
		// doc comment for why cut, not continuation, is the one that
		// should require an unambiguous signal.
		transition := "continuation"
		if strings.ToLower(strings.TrimSpace(r.Transition)) == "cut" {
			transition = "cut"
		}
		if r.StartIdx < batchStart {
			if i > 0 {
				seamChange = transition
			}
			continue
		}
		if seamChange != "" && r.StartIdx > batchStart && len(out) == 0 {
			out = append(out, musicBoundary{StartIdx: batchStart, Transition: seamChange})
		}
		out = append(out, musicBoundary{StartIdx: r.StartIdx, Transition: transition})
	}
	if seamChange != "" && len(out) == 0 {
		out = append(out, musicBoundary{StartIdx: batchStart, Transition: seamChange})
	}
	if len(out) == 0 && !hasSeam {
		return nil, fmt.Errorf("every region for paragraphs %d-%d started on an inline continuation line", shownStart, shownEnd)
	}
	return out, nil
}

// scoreMusicBoundaries is pass 1: batches chapterTitle's paragraphs into
// tone-region boundaries (see musicBoundary), up to musicParallelBatches
// at a time - batches share no state (each sees the previous one's tail
// as plain text, musicSeamContextLines), so they can run concurrently.
// shouldPause (nil-safe) is checked before launching each batch but the
// first, so a single dispatch always makes some real progress; on a pause
// or a genuine failure, out reflects every batch before the first one not
// completed, while remaining holds everything from that one on - the same
// contract AttributeChapter documents in full.
//
// Only the chapter's first region is a forced "cut" (musicSystemPrompt's
// own "always cut the very first region" rule); a later batch's model call
// judges its seam against the context it was shown.
func (c *Client) scoreMusicBoundaries(ctx context.Context, bookTitle, chapterTitle string, paragraphs []ParagraphInput, shouldPause func() bool) (out []musicBoundary, remaining []ParagraphInput, err error) {
	type span struct{ start, end int }
	var spans []span
	for start := 0; start < len(paragraphs); {
		end := min(start+musicBatchParagraphs, len(paragraphs))
		// Never let a batch boundary fall inside an inline-continuation
		// run - a batch's own first paragraph must be a real, numbered
		// line (see musicBoundaryBatch's Inline folding).
		for end < len(paragraphs) && paragraphs[end].Inline {
			end++
		}
		spans = append(spans, span{start, end})
		start = end
	}

	results := make([][]musicBoundary, len(spans))
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		stopAt   = len(spans)
		firstErr error
	)
	sem := make(chan struct{}, musicParallelBatches)
	for bi, sp := range spans {
		if bi > 0 && shouldPause != nil && shouldPause() {
			mu.Lock()
			stopAt = min(stopAt, bi)
			mu.Unlock()
			break
		}
		sem <- struct{}{}
		mu.Lock()
		failed := firstErr != nil
		mu.Unlock()
		if failed {
			<-sem
			break
		}
		batch := paragraphs[sp.start:sp.end]
		seam := musicSeamContext(paragraphs[:sp.start])
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			regions, berr := c.musicBoundaryBatch(ctx, bookTitle, chapterTitle, seam, batch)
			mu.Lock()
			defer mu.Unlock()
			if berr != nil {
				if bi < stopAt {
					stopAt = bi
					firstErr = fmt.Errorf("paragraphs %d-%d: %w", batch[0].Idx, batch[len(batch)-1].Idx, berr)
				}
				return
			}
			results[bi] = regions
		}()
	}
	wg.Wait()

	for _, r := range results[:stopAt] {
		out = append(out, r...)
	}
	covered := paragraphs
	if stopAt < len(spans) {
		remaining = paragraphs[spans[stopAt].start:]
		covered = paragraphs[:spans[stopAt].start]
	}
	if len(out) > 0 {
		out[0].Transition = "cut"
	}
	return splitLongRegions(mergeShortRegions(out), covered), remaining, firstErr
}

// musicSeamContext returns the last musicSeamContextLines numbered lines of
// before (each with its Inline continuations), for the next batch to be
// shown as context - see musicSeamContextLines.
func musicSeamContext(before []ParagraphInput) []ParagraphInput {
	lines := 0
	for i := len(before) - 1; i >= 0; i-- {
		if before[i].Inline {
			continue
		}
		lines++
		if lines == musicSeamContextLines {
			return before[i:]
		}
	}
	return before
}

// minRegionParagraphs is mergeShortRegions' own floor on a region's own
// paragraph span - see its doc comment for why this exists as a
// deterministic backstop rather than something asked of the model via the
// prompt alone. Picked as a clear, easy-to-recognize-as-too-short lower
// bound (a 1-3 paragraph region is almost always a single line or two of
// dialogue, not a real scene) while still well under
// musicBatchParagraphs's own 20 - leaving plenty of room for several
// genuinely distinct regions in an ordinary batch, not collapsing
// everything into one.
const minRegionParagraphs = 5

// mergeShortRegions drops any region shorter than minRegionParagraphs
// paragraphs from out, folding its span into whichever region precedes
// it (out's own regions carry no explicit end - so simply removing a
// too-short entry is exactly a merge: the region before it implicitly
// grows to cover the gap once EndIdx is later derived from these starts,
// store.AppendMusicRegions's own job). Runs on musicBoundary, before pass 2
// ever writes a Mood/Prompt for anything - a region merged away here never
// costs a wasted describeMusicRegion call.
//
// A real, deterministic backstop, not another prompt tweak: musicSystemPrompt
// went through several escalating rounds asking the model directly for
// fewer, longer regions - explicit target counts, explicit worked
// examples of what NOT to do, even a separate dedicated LLM pass whose
// only job was grouping already-tagged paragraphs into runs - and none of
// it reliably worked. The failure mode oscillated between two extremes
// depending on exact wording (a new region on nearly every paragraph, or
// every paragraph collapsed into one region) with no stable middle ground
// found through prompting alone, single- or multi-pass. This is what
// actually guarantees a floor regardless of what the model does.
//
// Never merges away a "cut" region, no matter how short: a "cut" is a
// deliberate, sharp musical transition (see musicSystemPrompt's own doc
// comment) that the model specifically chose to
// mark distinct; merging it away would silently defeat the entire reason
// it was marked "cut" rather than "continuation" in the first place. The
// first and last regions in regions are also always kept regardless of
// span: the first has nothing before it to merge into, and the last has
// no reliably-known span from inside this package alone (its own true end
// is the chapter's own last paragraph, tracked by the caller - see
// AppendMusicRegions - not regions itself).
func mergeShortRegions(regions []musicBoundary) []musicBoundary {
	if len(regions) <= 1 {
		return regions
	}
	out := make([]musicBoundary, 0, len(regions))
	out = append(out, regions[0])
	for i := 1; i < len(regions)-1; i++ {
		r := regions[i]
		span := regions[i+1].StartIdx - r.StartIdx
		if span < minRegionParagraphs && r.Transition != "cut" {
			continue // merge into whichever region precedes it
		}
		out = append(out, r)
	}
	out = append(out, regions[len(regions)-1])
	return out
}

// maxRegionParagraphs caps how many logical paragraphs (a non-Inline
// paragraph plus every Inline continuation folded onto it - the one
// "complete narrated beat" the model is shown as a single numbered line,
// see musicSystemPrompt's own doc comment) a single region may span before
// splitLongRegions deterministically splits it, regardless of what the
// model itself judged. mergeShortRegions' own minRegionParagraphs is the
// floor this is the ceiling for - musicSystemPrompt's own "prefer far
// fewer, far longer regions" guidance has no upper limit of its own, and a
// single long, tonally-uniform stretch (one unbroken scene, a long stretch
// of narration) can legitimately produce one very long region under it. A
// long region costs more than just a long piece of music: pass 2's own
// describeMusicRegion call has to read that whole span in one go, and - a
// real, observed problem this constant exists to bound - a chapter whose
// own regions run long enough takes long enough to fully score that a
// higher-priority poolLLM task showing up mid-run (HasHigherPriorityWork)
// pauses it before AppendMusicRegions ever persists anything past whatever
// region was in progress, and (see scoreChapterMusic's own doc comment)
// every retry starts over from paragraph 0 - so a chapter stuck with a few
// very long regions can end up making close to zero net progress, retry
// after retry, on a busy box. Smaller, more numerous regions checkpoint
// (AppendMusicRegions persists per pass-1 batch) more often, so a pause
// loses less work regardless of retry behavior.
const maxRegionParagraphs = 10

// splitLongRegions deterministically inserts a "continuation" boundary
// after every maxRegionParagraphs-th logical paragraph within any region
// that would otherwise run longer than that - a hard ceiling layered on
// top of whatever scoreMusicBoundaries/mergeShortRegions already decided,
// counted in the same logical-paragraph units musicSystemPrompt's own
// numbered listing uses (an Inline paragraph is never itself a candidate
// split point - see musicSystemPrompt's own "never start a region on an
// Inline line" rule, the same reason parseMusicBoundaries already keeps a
// hallucinated startIdx off one). Runs after mergeShortRegions, not
// before: splitting first and merging second would risk mergeShortRegions
// immediately folding a deterministically-split remainder back into its
// own predecessor if that remainder came out shorter than
// minRegionParagraphs, silently undoing the split it was never supposed
// to touch. A split is always "continuation", never "cut" - a length cap
// isn't a real musical event, and (see musicSystemPrompt's own doc
// comment on why "cut" is deliberately rare) forcing one here would be
// exactly the kind of choppy, unwarranted hard cross-fade this whole
// package's transition bias exists to avoid.
func splitLongRegions(regions []musicBoundary, paragraphs []ParagraphInput) []musicBoundary {
	if len(regions) == 0 || len(paragraphs) == 0 {
		return regions
	}
	logicalStarts := make([]int, 0, len(paragraphs))
	for _, p := range paragraphs {
		if !p.Inline {
			logicalStarts = append(logicalStarts, p.Idx)
		}
	}
	lastIdx := paragraphs[len(paragraphs)-1].Idx

	out := make([]musicBoundary, 0, len(regions))
	for i, r := range regions {
		regionEnd := lastIdx
		if i+1 < len(regions) {
			regionEnd = regions[i+1].StartIdx - 1
		}
		out = append(out, r)
		count := 0
		for _, idx := range logicalStarts {
			if idx < r.StartIdx || idx > regionEnd {
				continue
			}
			count++
			if count > 1 && (count-1)%maxRegionParagraphs == 0 {
				out = append(out, musicBoundary{StartIdx: idx, Transition: "continuation", split: true})
			}
		}
	}
	return out
}

// musicDescribeMaxTokens bounds pass 2's generated JSON - a mood label,
// a handful of short music fields and one ambience description per call,
// comfortably smaller than pass 1's own batch-of-several-regions budget,
// since this is one region's own single description.
const musicDescribeMaxTokens = 1024

// musicDescribeSystemPrompt is pass 2: given the real text of one region
// scoreMusicBoundaries already identified (rather than a numbered listing
// spanning several regions at once, the way the old single-pass prompt
// worked), describe that region's own background bed - a music cue and,
// where the scene has a clear physical setting, an ambience layer mixed
// under it (see musicgen.MixAmbience). Splitting this out from the
// boundary judgment is the same lesson DescribeChapter was already split
// from AttributeChapter over (backend/CLAUDE.md) - asking one call to both
// place a boundary and justify a full composer brief for it in the same
// breath left the single-pass version measurably worse on real chapters
// (ad hoc comparison against two real book chapters: the combined pass
// produced 15 regions for a 63-paragraph chapter - its own prompt calls 4+
// "a sign something went wrong" - with all but one marked "cut",
// apparently because justifying a differently-worded brief made every tone
// change look like a hard cut; splitting the judgment out produced 8
// regions with far fewer spurious cuts on the same chapter, and also
// sidestepped a batch-seam parse failure that had permanently lost the
// back half of another chapter under the combined prompt).
//
// The model fills structured fields (genres, instruments, bpm, a short
// description) rather than writing the final Stable Audio prompt itself:
// buildMusicPrompt assembles them into Stable Audio 3's own documented
// AudioSparx tag shape ("TrackType: Music, VocalType: Instrumental,
// Genre: ..., Instruments: ..., <description>, <n> BPM" - see the
// stable-audio-3 prompting guide), so the tag syntax can never drift or be
// half-remembered by a 4B model. The previous region's own description is
// passed back in (musicDescribeContext) so a "continuation" keeps its
// palette and a setting that hasn't changed keeps byte-identical ambience,
// which is what lets the ambience layer be continuation-seeded across
// regions (jobs.Manager.musicRegionSeeds).
const musicDescribeSystemPrompt = `You are the sound designer for an audiobook. Behind the narrator's voice plays a quiet background bed made of two layers: an instrumental music underscore, and (when the scene has a clear physical setting) an ambient soundscape of that place - ocean waves, a busy tavern, city streets, rain on a roof, a forest at night. You are designing the bed for one contiguous stretch of a novel chapter, which an earlier pass has already identified as a single tone region. You will be given the full text of every paragraph within that region, in order, and possibly a description of the region that plays just before it.

SCENE - decide this first, when a previous region is given:
- "new" when this region starts a different scene from the previous one: the story jumps forward in time or to a different place, switches to other characters, or something shocking erupts out of calm (a twist, sudden violence).
- "same" when it carries on the same scene - the same people in the same place and moment, even if the mood has shifted.
- With no previous region, or when told an earlier pass already marked a new scene, answer "new".

MUSIC - an underscore that sits under spoken narration, never competing with it:
- Match the region's emotional register, and pick a palette that fits the book's world (historical or fantasy settings suit acoustic, folk or orchestral instruments; futuristic settings suit synths and electronic textures; contemporary settings can go either way).
- Keep it sparse and steady: sustained pads, soft textures, low or mid-register instruments, gentle pulses. Avoid a prominent lead melody, bright high-pitched solo instruments, heavy drums, sudden drops or big builds - anything that would fight the voice for attention. Even tense or action scenes stay restrained: tension comes from rhythm, low drones and dissonance, not loudness.
- Instrumental only - never vocals, choirs singing words, or lyrics.
- For the "same" scene, keep the previous region's genre and core instruments, evolving the mood and energy rather than switching style. For a "new" scene, choose a fresh palette that fits it, rather than reusing the previous one out of habit.

AMBIENCE - the continuous sound of the place where this region physically happens:
- Describe it the way a field recording is labeled: the sound sources, how busy or sparse they are, the space and perspective (e.g. "ocean waves breaking on a rocky shore, distant gulls, steady sea wind, wide open outdoor perspective"; "busy medieval tavern interior, low indistinct crowd murmur, clinking mugs, crackling hearth fire, wooden room"; "night city street, distant traffic hum, occasional far-off car horn, light rain on pavement").
- Only continuous, loopable background sound - never a one-off event (no single door slam, gunshot, scream, explosion or line of speech), and never music. Crowds are fine as indistinct murmur, never as intelligible speech.
- Use "" (empty) when there is no clear, sustained physical setting to hear: a quiet interior with nothing notable audible, a dream or abstract space, a stretch of reflection or summary, or a setting you would only be guessing at.
- Decide the setting from this region's own text first. If the previous region's ambience is given and this text clearly still takes place in that same place, copy that ambience string exactly, character for character. If the characters have moved somewhere else, or the text no longer fits that place, write a new one (or "") - never carry over a place the text has left.

Never mention plot events, character names or story specifics in any field - describe only sound, as if briefing someone who has never read the book.

The "music" field is read by a text-to-music model, not a person: write it as a short run of comma-separated sound descriptors (mood, energy, dynamics, texture), at most 15 words - e.g. "slow, brooding, low sustained drones, faint uneasy pulse, sparse". Don't repeat the instruments, don't explain what the music reflects or mirrors, and never refer to the narration or the story.

Reply with a compact JSON object on a single line only, no other text, no markdown code fence:

{"scene": "same" | "new", "mood": "<2-4 word emotional label, e.g. tense confrontation, quiet melancholy, warm banter>", "setting": "<2-6 word physical setting, or empty>", "genres": ["<1-2 genres, e.g. Ambient, Cinematic, Folk, Orchestral, Electronic>"], "instruments": ["<2-4 instruments or textures>"], "bpm": <integer tempo, 50-120>, "music": "<at most 15 words of comma-separated sound descriptors>", "ambience": "<field-recording style description, or empty>"}`

// musicDescription is pass 2's own finished judgment for one region - the
// assembled Stable Audio prompts (see buildMusicPrompt/
// buildAmbiencePrompt), plus the raw fields the next region's own call is
// shown as context (see musicDescribeContext).
type musicDescription struct {
	Mood     string
	Setting  string
	Prompt   string // assembled music prompt
	Ambience string // assembled ambience prompt, "" for none
	// music/ambience are the model's own raw sentences, before assembly -
	// what the next region's own call sees as its predecessor, so it can
	// copy an unchanged ambience back verbatim.
	music       string
	rawAmbience string
	genres      []string
	instruments []string
	// newScene is the model's "scene" judgment - see ScoreMusic, which
	// turns it into a "cut".
	newScene bool
}

type musicDescriptionRaw struct {
	Scene       string   `json:"scene"`
	Mood        string   `json:"mood"`
	Setting     string   `json:"setting"`
	Genres      []string `json:"genres"`
	Instruments []string `json:"instruments"`
	BPM         float64  `json:"bpm"`
	Music       string   `json:"music"`
	Ambience    string   `json:"ambience"`
	// Prompt is the old single-string shape - accepted as a fallback for
	// Music so a model that ignores the new schema still yields a usable
	// cue instead of a failed parse.
	Prompt string `json:"prompt"`
}

// musicDescribeContext is the previous region's own description, as shown
// to the next region's describe call - nil for a chapter's (or resumed
// run's) first region.
type musicDescribeContext struct {
	prev       *musicDescription
	transition string
}

func (c *Client) describeMusicRegion(ctx context.Context, bookTitle, chapterTitle string, regionParagraphs []ParagraphInput, mctx musicDescribeContext) (musicDescription, error) {
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nChapter: %s\n", bookTitle, chapterTitle)
	switch {
	case mctx.prev == nil:
		user.WriteString("This is the chapter's opening region - its scene is \"new\".\n")
	case mctx.transition == "cut":
		user.WriteString("An earlier pass already marked this region as a new scene - its scene is \"new\".\n")
	}
	if p := mctx.prev; p != nil {
		user.WriteString("\nPrevious region:\n")
		fmt.Fprintf(&user, "- mood: %s\n", p.Mood)
		fmt.Fprintf(&user, "- setting: %s\n", orNone(p.Setting))
		fmt.Fprintf(&user, "- genres: %s\n", strings.Join(p.genres, ", "))
		fmt.Fprintf(&user, "- instruments: %s\n", strings.Join(p.instruments, ", "))
		fmt.Fprintf(&user, "- music: %s\n", p.music)
		fmt.Fprintf(&user, "- ambience: %q\n", p.rawAmbience)
	}
	user.WriteString("\nRegion text:\n")
	for i, p := range regionParagraphs {
		if p.Inline {
			fmt.Fprintf(&user, " %s", oneLine(p.Text))
			continue
		}
		if i > 0 {
			user.WriteString("\n\n")
		}
		user.WriteString(oneLine(p.Text))
	}
	user.WriteString("\n")
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}

	raw, err := generateAndParse(ctx, c, musicDescribeSystemPrompt, user.String(), 0, musicDescribeMaxTokens, parseMusicRegionDescription)
	if err != nil {
		return musicDescription{}, err
	}
	d := musicDescription{
		Mood:        raw.Mood,
		Setting:     raw.Setting,
		Prompt:      buildMusicPrompt(raw),
		music:       raw.Music,
		rawAmbience: raw.Ambience,
		genres:      raw.Genres,
		instruments: raw.Instruments,
		newScene:    strings.EqualFold(strings.TrimSpace(raw.Scene), "new"),
	}
	// An unchanged setting must produce a byte-identical ambience prompt
	// (see musicDescribeSystemPrompt) - the model is asked to copy it, but
	// a small model's copy can drift by a character or two, which would
	// silently lose the seeded continuation. Snap a near-copy back.
	if p := mctx.prev; p != nil && p.rawAmbience != "" && sameAmbience(raw.Ambience, p.rawAmbience) {
		d.rawAmbience = p.rawAmbience
	}
	d.Ambience = buildAmbiencePrompt(d.rawAmbience)
	return d, nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// sameAmbience reports whether a is the previous region's ambience b,
// copied back with only case/whitespace/punctuation drift.
func sameAmbience(a, b string) bool {
	norm := func(s string) string {
		var sb strings.Builder
		for _, r := range strings.ToLower(s) {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				sb.WriteRune(r)
			}
		}
		return sb.String()
	}
	return norm(a) != "" && norm(a) == norm(b)
}

func parseMusicRegionDescription(content string) (musicDescriptionRaw, error) {
	start := strings.IndexByte(content, '{')
	end := strings.LastIndexByte(content, '}')
	if start == -1 || end == -1 || end < start {
		return musicDescriptionRaw{}, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
	}
	var raw musicDescriptionRaw
	if err := json.Unmarshal([]byte(content[start:end+1]), &raw); err != nil {
		return musicDescriptionRaw{}, fmt.Errorf("parse JSON object: %w", err)
	}
	raw.Mood = strings.TrimSpace(raw.Mood)
	raw.Setting = strings.TrimSpace(raw.Setting)
	raw.Music = strings.TrimSpace(raw.Music)
	if raw.Music == "" {
		raw.Music = strings.TrimSpace(raw.Prompt)
	}
	raw.Ambience = cleanAmbience(raw.Ambience)
	raw.Genres = cleanTagList(raw.Genres, 2)
	raw.Instruments = cleanTagList(raw.Instruments, 4)
	if raw.Music == "" && len(raw.Instruments) == 0 {
		return musicDescriptionRaw{}, fmt.Errorf("empty music description")
	}
	return raw, nil
}

// cleanAmbience normalizes the model's "no ambience" spellings ("none",
// "n/a", "silence") to "" - an ambience layer is an extra Stable Audio
// render, so a placeholder must never get sent as a real prompt.
func cleanAmbience(s string) string {
	s = strings.TrimSpace(s)
	switch strings.Trim(strings.ToLower(s), ".()[] ") {
	case "", "none", "n/a", "na", "null", "empty", "silence", "no ambience", "nothing":
		return ""
	}
	return s
}

// cleanTagList trims, drops empties/duplicates and anything containing the
// ", " / ":" separators buildMusicPrompt's own tag syntax relies on, and
// caps the list at max entries.
func cleanTagList(in []string, max int) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(strings.ReplaceAll(s, ":", ""))
		if s == "" || strings.Contains(s, ",") || seen[strings.ToLower(s)] {
			continue
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
		if len(out) == max {
			break
		}
	}
	return out
}

// musicMinBPM/musicMaxBPM clamp the model's own tempo - anything outside
// is either a hallucination or far too busy to sit under narration.
const (
	musicMinBPM = 40
	musicMaxBPM = 140
)

// buildMusicPrompt assembles raw into Stable Audio 3's own tag shape - see
// musicDescribeSystemPrompt's own doc comment. "VocalType: Instrumental" is
// the guide's own documented way to keep vocals out (the default
// guidance_scale of 1.0 makes a negative prompt a no-op, so this has to
// live in the positive prompt), and "underscore, background music" steer
// toward the stock/library-music end of its training data.
func buildMusicPrompt(raw musicDescriptionRaw) string {
	parts := []string{"TrackType: Music", "VocalType: Instrumental"}
	for _, g := range raw.Genres {
		parts = append(parts, "Genre: "+g)
	}
	if len(raw.Instruments) > 0 {
		parts = append(parts, "Instruments: "+strings.Join(raw.Instruments, ", "))
	}
	if raw.Music != "" {
		parts = append(parts, strings.TrimRight(raw.Music, ". "))
	}
	parts = append(parts, "subtle background underscore")
	if bpm := int(raw.BPM); bpm > 0 {
		bpm = max(musicMinBPM, min(musicMaxBPM, bpm))
		parts = append(parts, fmt.Sprintf("%d BPM", bpm))
	}
	return strings.Join(parts, ", ")
}

// buildAmbiencePrompt wraps a raw ambience description in the guide's own
// "TrackType: SFX" tag (sound rather than music) plus Freesound-style
// field-recording descriptors - "" stays "" (no ambience layer).
func buildAmbiencePrompt(raw string) string {
	if raw == "" {
		return ""
	}
	return "TrackType: SFX, ambience field recording, " + strings.TrimRight(raw, ". ") + ", continuous seamless background atmosphere, no music"
}

// musicDescribeGroupRegions is how many regions one describeMusicRegion
// call may cover: a tone region plus the splitLongRegions chunks right
// after it, which are the same tone by pass 1's own judgment and so share
// its description instead of costing a call each. Measured before this on
// Spire's Spite: describe calls were ~70% of scoring time, and most
// regions were such chunks. Capped rather than unlimited so a very long
// single-tone stretch still gets re-described now and then (and one
// describe prompt stays a bounded ~30 paragraphs).
const musicDescribeGroupRegions = 3

// ScoreMusic tone-scores chapterTitle's paragraphs into background-music
// regions (see MusicRegionResult) in two passes: scoreMusicBoundaries
// (pass 1) decides only where each region starts and whether it's a "cut"
// or "continuation", batching in groups of musicBatchParagraphs; then, for
// each resulting region, describeMusicRegion (pass 2) writes that region's
// own mood label, music prompt and ambience prompt from its own actual
// paragraph span - one call per region (a length-only split shares its
// region's call - musicDescribeGroupRegions), once its real boundaries are
// known, rather than asking the model to write a description while
// simultaneously still deciding where the region even begins/ends. See
// musicDescribeSystemPrompt's own doc comment for the ad hoc comparison
// that motivated this split. Pass 2 runs strictly in region order, each
// call shown the previous region's own description (musicDescribeContext)
// so palettes carry through continuations and an unchanged setting keeps
// an identical ambience prompt.
//
// shouldPause (nil-safe: nil never pauses) is threaded through pass 1's
// own batches (see scoreMusicBoundaries) and, separately, checked again
// between pass 2's own per-region calls (never before the first region,
// matching every other batched pass in this package) - a long chapter
// with many regions can otherwise run pass 2 to completion even after a
// more urgent poolLLM task has shown up. out only ever contains regions
// pass 2 actually finished describing; remaining is whichever paragraphs
// (from pass 1's own leftover batches, or from wherever pass 2 stopped,
// whichever is later) weren't turned into a finished region - the same
// "out reflects real, finished work" contract AttributeChapter documents
// in full. httpapi.scoreChapterMusic never auto-resumes a paused run (see
// its own doc comment) - remaining is informational there, not something
// this call itself needs to get perfectly minimal.
func (c *Client) ScoreMusic(ctx context.Context, bookTitle, chapterTitle string, paragraphs []ParagraphInput, shouldPause func() bool) (out []MusicRegionResult, remaining []ParagraphInput, err error) {
	if len(paragraphs) == 0 {
		return nil, nil, nil
	}
	boundaries, remaining, err := c.scoreMusicBoundaries(ctx, bookTitle, chapterTitle, paragraphs, shouldPause)
	if err != nil {
		return nil, remaining, err
	}
	if len(boundaries) == 0 {
		return nil, remaining, nil
	}

	lastIdx := paragraphs[len(paragraphs)-1].Idx
	if len(remaining) > 0 {
		lastIdx = remaining[0].Idx - 1
	}
	regionEnd := func(i int) int {
		if i+1 < len(boundaries) {
			return boundaries[i+1].StartIdx - 1
		}
		return lastIdx
	}
	out = make([]MusicRegionResult, 0, len(boundaries))
	var prev *musicDescription
	for i := 0; i < len(boundaries); {
		b := boundaries[i]
		if i > 0 && shouldPause != nil && shouldPause() {
			remaining = paragraphsFromIdx(paragraphs, b.StartIdx)
			return out, remaining, nil
		}
		// One description covers this region plus the length-only splits
		// right after it (see musicDescribeGroupRegions).
		n := 1
		for i+n < len(boundaries) && boundaries[i+n].split && n < musicDescribeGroupRegions {
			n++
		}
		span := paragraphsInIdxRange(paragraphs, b.StartIdx, regionEnd(i+n-1))
		if len(span) == 0 {
			i += n
			continue
		}
		d, derr := c.describeMusicRegion(ctx, bookTitle, chapterTitle, span, musicDescribeContext{prev: prev, transition: b.Transition})
		if derr != nil {
			return out, paragraphsFromIdx(paragraphs, b.StartIdx), fmt.Errorf("describing region starting at %d: %w", b.StartIdx, derr)
		}
		// Pass 1 sees only numbered lines and almost never calls a cut
		// itself; the describe call, reading the region's full text against
		// the previous region's setting, is what spots a scene change.
		transition := b.Transition
		if prev != nil && d.newScene {
			transition = "cut"
		}
		prev = &d
		for k, g := range boundaries[i : i+n] {
			if k == 0 {
				g.Transition = transition
			}
			out = append(out, MusicRegionResult{StartIdx: g.StartIdx, Mood: d.Mood, Prompt: d.Prompt, Ambience: d.Ambience, Transition: g.Transition})
		}
		i += n
	}
	return out, remaining, nil
}

func paragraphsFromIdx(paragraphs []ParagraphInput, startIdx int) []ParagraphInput {
	for i, p := range paragraphs {
		if p.Idx >= startIdx {
			return paragraphs[i:]
		}
	}
	return nil
}

func paragraphsInIdxRange(paragraphs []ParagraphInput, start, end int) []ParagraphInput {
	var out []ParagraphInput
	for _, p := range paragraphs {
		if p.Idx >= start && p.Idx <= end {
			out = append(out, p)
		}
	}
	return out
}
