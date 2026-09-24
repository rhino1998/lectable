package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// musicBatchParagraphs bounds one pass-1 (boundary) music-scoring
// completion request - directionBatchParagraphs' own sibling, picked a bit
// larger since a tone-region judgment benefits from more surrounding
// context than a per-line delivery-tag judgment does (deciding "has the
// mood genuinely shifted" needs to see a real stretch of the scene, not
// just one line at a time), while still keeping one malformed response
// from losing more than one batch's worth of a long chapter. A batch
// boundary can still fall mid-scene (a real, known imperfection - a region
// is never allowed to span two batches, since each batch is scored
// independently with no carried-over context), so a chapter with several
// unrelated tonal stretches inside one batch may get a slightly choppier
// region split than an unbounded single call would produce; revisit this
// size if that turns out to matter in practice.
const musicBatchParagraphs = 20

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
// Deliberately biases toward "continuation" over "cut" (see the transition
// paragraph below): an earlier version left the choice more neutral, but
// generated background music switching abruptly (a fresh, unrelated-
// sounding clip, cross-faded) every time the model called a merely
// different mood "cut" made for a much choppier listen than letting most
// mood shifts flow into each other the way real film/game scores usually
// do - "cut" reads as a genuine musical event (a sting, a hard scene
// break) and should be rare, not the default outcome of "the tone changed
// at all."
const musicSystemPrompt = `You are scoring a novel's chapter for background instrumental music. You will be given a numbered list of paragraphs from one chapter (narration and/or spoken dialogue). Identify where the chapter's emotional/narrative tone shifts meaningfully (e.g. calm -> tense, banter -> grief, mundane -> suspenseful) and mark the paragraph where each new tone region begins. A region implicitly runs until the next region's own start (or the end of the chapter, for the last region) - never state where a region ends.

A region should span a real, sustained tone, not flicker per paragraph - prefer far fewer, far longer regions over many short ones. A single tense paragraph inside an otherwise calm scene does not need its own region unless the tension actually carries forward. Be very conservative: for a list of around 20 paragraphs, 1-2 regions is typical and 3 should already be unusual - treat 4 or more as a sign something went wrong, not a reasonable outcome. An entire scene - a whole back-and-forth conversation, a whole action beat, a whole stretch of narration, a whole run of reminiscence or description - is normally ONE region for its full span, no matter how much individual lines within it rise and fall in intensity, or how much the subject matter itself moves around (excitement building through a conversation, a joke landing inside a tense exchange, a character's train of thought drifting from one memory or topic to the next, a description moving from one detail to another). What decides a region's boundary is the *emotional register the music itself would play under* - calm, tense, warm, melancholy, urgent - never the subject matter: a character recalling five completely different memories in a row is still ONE region the whole time if all five are being recalled in the same wistful, reflective mood; a conversation that wanders across several unrelated topics is still ONE region as long as the *feeling* in the room doesn't change. Only mark a new region where that underlying feeling itself genuinely changes - not where the topic, setting, or subject changes on its own. If you're tempted to start a new region because a line moved on to something different, but the mood a composer would score it with is the same as the line before, don't - that's not a boundary.

Each numbered line below is already one complete narrated beat: when the original text had a quote immediately followed by its own short narration tag ("she said") or more dialogue right after it, that continuation text is folded directly onto its paragraph's own numbered line rather than given a number of its own. So every number you see already marks the start of a real, whole beat - you never need to identify or avoid a mid-beat line yourself.

Also judge this region's own transition, defaulting to "continuation" unless the shift is genuinely jarring: use "continuation" for the ordinary case, including a real mood shift, as long as it's the kind of change a film score would simply evolve through (calm drifting into unease, tension slowly building, warmth cooling into melancholy) - most regions after the first should be "continuation". Reserve "cut" for an actual musical shock: a sudden scene change, a twist, violence erupting out of calm, a chapter's own opening - moments where an abrupt, surprising break in the music itself is exactly right. Always use "cut" for the very first region in the list, which has nothing before it to continue. When genuinely unsure between the two, choose "continuation".

Reply with a JSON array only, no other text, no markdown code fence, ordered by paragraph index:

[
  {"startIdx": <int>, "transition": "cut" | "continuation"},
  ...
]

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
	StartIdx   int
	Mood       string
	Prompt     string
	Transition string // "cut" or "continuation" - see musicSystemPrompt
}

// musicBoundary is pass 1's own narrower judgment - a region's start and
// transition, before pass 2 (describeMusicRegion) has filled in its own
// Mood/Prompt from that region's real paragraph span. Never exported: only
// ScoreMusic's own final, combined MusicRegionResult is a public shape.
type musicBoundary struct {
	StartIdx   int
	Transition string
}

type musicBoundaryRaw struct {
	StartIdx   int    `json:"startIdx"`
	Transition string `json:"transition"`
}

func (c *Client) musicBoundaryBatch(ctx context.Context, bookTitle, chapterTitle string, batch []ParagraphInput) ([]musicBoundary, error) {
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nChapter: %s\n", bookTitle, chapterTitle)
	user.WriteString("\nParagraphs:\n")
	for i, p := range batch {
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
		return parseMusicBoundaries(content, batch)
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
// The *first* region is corrected, not rejected, when it doesn't start
// exactly at batch's own first paragraph: a real, observed, 100%-
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
func parseMusicBoundaries(content string, batch []ParagraphInput) ([]musicBoundary, error) {
	batchStart := batch[0].Idx
	inline := make(map[int]bool, len(batch))
	shownEnd := batchStart
	for _, p := range batch {
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
		return nil, fmt.Errorf("no regions returned for paragraphs %d-%d", batchStart, shownEnd)
	}

	out := make([]musicBoundary, 0, len(raw))
	prevStart := -1
	for i, r := range raw {
		if i == 0 {
			// See this function's own doc comment - corrected, not
			// rejected, so a batch opening on trivial front matter (a
			// bare chapter-number heading) doesn't permanently fail no
			// matter how many times it's retried.
			r.StartIdx = batchStart
		} else if r.StartIdx <= prevStart {
			return nil, fmt.Errorf("region startIdx %d does not strictly increase from previous %d", r.StartIdx, prevStart)
		}
		if r.StartIdx > shownEnd {
			return nil, fmt.Errorf("region startIdx %d beyond paragraphs %d-%d", r.StartIdx, batchStart, shownEnd)
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
		out = append(out, musicBoundary{StartIdx: r.StartIdx, Transition: transition})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("every region for paragraphs %d-%d started on an inline continuation line", batchStart, shownEnd)
	}
	return out, nil
}

// scoreMusicBoundaries is pass 1: batches chapterTitle's paragraphs into
// tone-region boundaries (see musicBoundary), the same batching/pause/
// forced-batch-seam-cut shape ScoreMusic's own single-pass predecessor
// used - shouldPause (nil-safe) is checked between batches, never before
// the first, so a single dispatch always makes at least some real
// progress; on a pause or a genuine failure, out reflects every batch
// actually completed while remaining holds whatever wasn't reached, the
// same contract AttributeChapter documents in full.
//
// There's no cross-batch state to carry (no running "known regions" list -
// each batch's own paragraph range is judged independently, with each
// batch's own first region required to start exactly at that batch's own
// first paragraph - see parseMusicBoundaries) - see musicBatchParagraphs'
// own doc comment for the tradeoff that allows.
func (c *Client) scoreMusicBoundaries(ctx context.Context, bookTitle, chapterTitle string, paragraphs []ParagraphInput, shouldPause func() bool) (out []musicBoundary, remaining []ParagraphInput, err error) {
	for start := 0; start < len(paragraphs); {
		if start > 0 && shouldPause != nil && shouldPause() {
			remaining = paragraphs[start:]
			break
		}
		end := start + musicBatchParagraphs
		if end > len(paragraphs) {
			end = len(paragraphs)
		}
		// Never let a batch boundary fall inside an inline-continuation
		// run - musicSystemPrompt's own "never start a region on an Inline
		// line" rule (parseMusicBoundaries' own enforcement of it) would
		// make the *next* batch's own required first region (its first
		// paragraph, by parseMusicBoundaries' "must start at batchStart"
		// rule) impossible to satisfy legitimately if that first paragraph
		// were itself an Inline continuation of this batch's own last one.
		for end < len(paragraphs) && paragraphs[end].Inline {
			end++
		}
		batch := paragraphs[start:end]

		regions, berr := c.musicBoundaryBatch(ctx, bookTitle, chapterTitle, batch)
		if berr != nil {
			return out, paragraphs[start:], fmt.Errorf("paragraphs %d-%d: %w", batch[0].Idx, batch[len(batch)-1].Idx, berr)
		}
		if start > 0 && len(regions) > 0 {
			// Every batch is scored independently, with no visibility into
			// how the previous batch's own last region actually ended (see
			// musicBatchParagraphs' own doc comment) - so only the chapter's
			// true first batch (start == 0) can ever make a genuine
			// "continuation" judgment for its own first region. Force every
			// later batch's own first region to "cut" regardless of what the
			// model guessed, the same "don't just ask nicely, enforce it"
			// pattern used throughout this package: a false "cut" at
			// a batch seam just costs one short frontend crossfade, while a
			// false "continuation" there would seed generation from audio
			// the model never actually compared against.
			regions[0].Transition = "cut"
		}
		out = append(out, regions...)
		start = end
	}
	return splitLongRegions(mergeShortRegions(out), paragraphs), remaining, nil
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
// comment) that the model - or, at a batch seam, this package itself, see
// scoreMusicBoundaries' own forced-cut comment - specifically chose to
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
				out = append(out, musicBoundary{StartIdx: idx, Transition: "continuation"})
			}
		}
	}
	return out
}

// musicDescribeMaxTokens bounds pass 2's generated JSON - a mood label
// plus one composer-brief paragraph per call, comfortably smaller than
// pass 1's own batch-of-several-regions budget used to need, since this is
// one region's own single description.
const musicDescribeMaxTokens = 1024

// musicDescribeSystemPrompt is pass 2: given the real text of one region
// scoreMusicBoundaries already identified (rather than a numbered listing
// spanning several regions at once, the way the old single-pass prompt
// worked), write that region's own mood label and music generation prompt.
// Splitting this out from the boundary judgment is the same lesson
// DescribeChapter was already split from AttributeChapter over
// (backend/CLAUDE.md) - asking one call to both place a
// boundary and justify a full composer brief for it in the same breath
// left the single-pass version measurably worse on real chapters (ad hoc
// comparison against two real book chapters: the combined pass produced
// 15 regions for a 63-paragraph chapter - its own prompt calls 4+ "a sign
// something went wrong" - with all but one marked "cut", apparently
// because justifying a differently-worded brief made every tone change
// look like a hard cut; splitting the judgment out produced 8 regions
// with far fewer spurious cuts on the same chapter, and also sidestepped a
// batch-seam parse failure that had permanently lost the back half of
// another chapter under the combined prompt).
const musicDescribeSystemPrompt = `You are scoring one background instrumental music cue for a single contiguous stretch of a novel chapter - a tone region that has already been identified for you by an earlier pass. You will be given the full text of every paragraph within that region, in order.

Write a music generation prompt describing instrumental background music matching this region's own tone - genre, instrumentation, tempo, mood/atmosphere descriptors, dynamics. Never mention plot events, character names, or lyrics - describe only the music itself, as if briefing a composer who has never read the book, and never ask for vocals/lyrics (this is instrumental-only music).

Also give a short mood label (2-4 words, e.g. "tense confrontation", "quiet melancholy", "warm banter") summarizing this region's own emotional register.

Reply with a JSON object only, no other text, no markdown code fence:

{"mood": "<short label>", "prompt": "<music generation prompt>"}`

type musicDescriptionRaw struct {
	Mood   string `json:"mood"`
	Prompt string `json:"prompt"`
}

func (c *Client) describeMusicRegion(ctx context.Context, bookTitle, chapterTitle string, regionParagraphs []ParagraphInput) (mood, prompt string, err error) {
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nChapter: %s\n", bookTitle, chapterTitle)
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
		return "", "", err
	}
	return raw.Mood, raw.Prompt, nil
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
	raw.Prompt = strings.TrimSpace(raw.Prompt)
	if raw.Prompt == "" {
		return musicDescriptionRaw{}, fmt.Errorf("empty music generation prompt")
	}
	return raw, nil
}

// ScoreMusic tone-scores chapterTitle's paragraphs into background-music
// regions (see MusicRegionResult) in two passes: scoreMusicBoundaries
// (pass 1) decides only where each region starts and whether it's a "cut"
// or "continuation", batching in groups of musicBatchParagraphs; then, for
// each resulting region, describeMusicRegion (pass 2) writes that region's
// own mood label and music generation prompt from its own actual
// paragraph span - one call per region, once its real boundaries are
// known, rather than asking the model to write a description while
// simultaneously still deciding where the region even begins/ends. See
// musicDescribeSystemPrompt's own doc comment for the ad hoc comparison
// that motivated this split.
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
	out = make([]MusicRegionResult, 0, len(boundaries))
	for i, b := range boundaries {
		if i > 0 && shouldPause != nil && shouldPause() {
			remaining = paragraphsFromIdx(paragraphs, b.StartIdx)
			return out, remaining, nil
		}
		regionEnd := lastIdx
		if i+1 < len(boundaries) {
			regionEnd = boundaries[i+1].StartIdx - 1
		}
		span := paragraphsInIdxRange(paragraphs, b.StartIdx, regionEnd)
		if len(span) == 0 {
			continue
		}
		mood, prompt, derr := c.describeMusicRegion(ctx, bookTitle, chapterTitle, span)
		if derr != nil {
			return out, paragraphsFromIdx(paragraphs, b.StartIdx), fmt.Errorf("describing region starting at %d: %w", b.StartIdx, derr)
		}
		out = append(out, MusicRegionResult{StartIdx: b.StartIdx, Mood: mood, Prompt: prompt, Transition: b.Transition})
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
