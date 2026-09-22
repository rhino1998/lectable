package speakerattr

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/deliverytags"
)

// validSentenceTags is the "sentence-level" half of Higgs Audio v3 TTS's
// own inline delivery-tag vocabulary (verified directly against the local
// Higgs-Audio-v3-TTS-4B-GGUF checkout's own embedded PROMPTING.md and
// tokenizer.json: 12 emotion (of the model's own full 21 -
// <|emotion:longing|>, <|emotion:arousal|>, <|emotion:affection|>,
// <|emotion:fear|>, <|emotion:contentment|>, <|emotion:confusion|>,
// <|emotion:sadness|>, <|emotion:enthusiasm|>, and <|emotion:elation|> are
// deliberately excluded from this app's own valid set: longing first,
// then the other eight after live use found them unreliable enough in
// practice - inconsistent, sometimes actively worse-sounding delivery for
// a given emotion tag - that asking for them was doing more harm than
// good; see the "Emotion:" line in directionSystemPrompt below) + 1 style
// (of the model's own 3 - <|style:whispering|> and <|style:singing|> are
// deliberately excluded too, the same "unreliable enough in practice"
// reasoning) + 8 prosody combined across both passes (6 of this file's own
// 8 sentence-level ones - <|prosody:pitch_low|> and <|prosody:pitch_high|>
// deliberately excluded, same reasoning - plus validInlineTags' own 2
// pause/long_pause, unaffected) + 9 sfx = 30 tags this app actually asks
// for or accepts, plus 2 undocumented
// <|env:...|> tokens also present in its vocab) - emotion,
// style, and prosody's speed/expressive family, all of which color a
// whole sentence/clause from wherever they're inserted, per the model's
// own PROMPTING.md.
// validInlineTags (sfx.go) is the other half - prosody's pause/long_pause
// and every sfx tag, which mark an exact point within a line instead. Kept
// as two separate sets (used by two separate LLM passes -
// Client.DirectChapter/Client.TagSfx) rather than one, the same "isolate a
// different kind of per-line judgment into its own smaller call" lesson
// DescribeChapter was split from AttributeChapter over (see
// describeSystemPrompt's own doc comment) - a sentence-level judgment
// ("what's the dominant emotion here") and a positional one ("is there an
// existing onomatopoeia word to tag, or a natural pause point") are
// different enough in kind that asking one model call to do both risks the
// same precision loss.
//
// Each string here must match the model's own tokenizer vocabulary exactly
// (every one of these is a dedicated "special": true added_tokens entry in
// Higgs-Audio-v3-TTS-4B-GGUF's tokenizer.json, guaranteeing atomic
// tokenization regardless of BPE merge rules) - any other spelling is not
// a control token at all, just literal text audio.cpp's tokenizer will
// happily encode and the model will happily (mis)pronounce.
var validSentenceTags = map[string]bool{
	"<|emotion:amusement|>":       true,
	"<|emotion:anger|>":           true,
	"<|emotion:awe|>":             true,
	"<|emotion:bitterness|>":      true,
	"<|emotion:contemplation|>":   true,
	"<|emotion:determination|>":   true,
	"<|emotion:disgust|>":         true,
	"<|emotion:helplessness|>":    true,
	"<|emotion:pride|>":           true,
	"<|emotion:relief|>":          true,
	"<|emotion:shame|>":           true,
	"<|emotion:surprise|>":        true,
	"<|style:shouting|>":          true,
	"<|prosody:speed_very_slow|>": true,
	"<|prosody:speed_slow|>":      true,
	"<|prosody:speed_fast|>":      true,
	"<|prosody:speed_very_fast|>": true,
	"<|prosody:expressive_high|>": true,
	"<|prosody:expressive_low|>":  true,
}

// ValidDeliveryTag reports whether tag is currently a valid Higgs delivery
// tag this app asks for or accepts - the union of validSentenceTags (this
// file) and validInlineTags (sfx.go). Exported for
// store.Paragraph.ResolveGenerationText, which filters a paragraph's own
// already-persisted tag insertions (SentenceText/InlineText, written by a
// possibly much earlier DirectChapter/TagSfx run) against this same set at
// read time - so a tag dropped from either set after a paragraph was
// already tagged with it (see validSentenceTags' own doc comment for the
// emotion/style tags already dropped this way) never actually reaches TTS
// for that paragraph, without needing a full chapter re-tag to take
// effect. Only ever reports whether tag is valid *right now* - a tag that
// was valid when a paragraph was tagged but has since been dropped reports
// false here, exactly the point.
func ValidDeliveryTag(tag string) bool {
	return validSentenceTags[tag] || validInlineTags[tag]
}

// directionBatchParagraphs bounds one delivery-tagging completion request,
// the same way describeBatchParagraphs bounds DescribeChapter's own -
// conservative (well under AttributeChapter's 40) since a batch's own
// generated response now has to echo back each tagged line's *entire*
// text (not just a short tag - see parseTaggedLines), not only because this is a
// similarly sparse, easy-to-over-tag judgment (see describeSystemPrompt's
// own "streak" bias history) that hasn't yet been benchmarked against real
// chapters the way describeBatchParagraphs was tuned down from 40 to 5 -
// revisit with the same kind of precision/response-size measurement if
// tagging turns out too trigger-happy, or batches too large, at this size.
const directionBatchParagraphs = 10

// directionMaxTokens bounds one batch's generated response - unlike the
// old single-tag-per-line design, a tagged line's own full text (up to
// ~4000 characters - see internal/epub's own comment on the longest
// paragraphs this app actually sends) is echoed back verbatim after its
// own "idx: " prefix (see parseTaggedLines), so this needs real headroom
// for a batch where several long lines all
// get tagged, plus a "hybrid thinking" model's own <think>...</think>
// trace (see stripThinking) - mirrors attributeMaxTokens' own reasoning
// but larger, for the same reason directionBatchParagraphs is smaller.
// Raised to genuinely exceed attributeMaxTokens now (both had drifted to
// the same 12288 despite this comment's own "but larger" - long chapters
// with several long, heavily-tagged lines in one batch were running out
// of room) - see DefaultNCtx's own doc comment for the context-window
// increase that goes with this.
const directionMaxTokens = 32768

// directionTemp is the first attempt's sampling temperature for both
// delivery-tagging passes (DirectChapter here and TagSfx) - just off
// greedy, unlike the temp 0 every JSON-parsed pass starts at. At temp 0 a
// batch the model degenerates on (a real, observed case: one sfx batch
// looping until directionMaxTokens, which outlasts ttsworker's 5-minute
// stuck-job watchdog) decodes the identical runaway on every top-level
// task retry too, since those re-run the same prompt through the same
// greedy path - a small temperature gives each retry an actual chance at
// a different token sequence. Kept below retryTempBump so a parse retry
// still escalates past it (see generateAndParse).
const directionTemp = 0.1

// taggedReplyLineAllowance is how many characters beyond a line's own text
// one echoed-back reply line may legitimately add: its "idx: " prefix,
// several inserted tags, and a little whitespace slack.
const taggedReplyLineAllowance = 256

// taggedReplyHeadroomTokens is the fixed token headroom taggedReplyBudget
// adds on top of the reply itself - room for a hybrid-thinking model's
// own <think> trace (see stripThinking) before its actual reply.
const taggedReplyHeadroomTokens = 2048

// taggedReplyBudget sizes one direction/sfx batch's generation to what a
// reply could actually need, instead of the flat directionMaxTokens/
// sfxMaxTokens ceiling. Both passes only ever echo back a subset of the
// batch's own lines with tags spliced in (see parseTaggedLines), so a real
// reply can never be much longer than the batch itself - maxChars is that
// bound. A reply longer than maxChars is a runaway (see
// taggedReplyParser). maxTokens converts it at a deliberately generous
// 2 chars/token (English prose runs nearer 4) plus
// taggedReplyHeadroomTokens, capped at ceiling. This matters because a
// degenerate generation (a real, observed case: one sfx batch never
// stopping) would otherwise run all the way to ceiling - 32768 tokens,
// longer than ttsworker's 5-minute stuck-job watchdog allows, so the
// worker got killed mid-request on every retry instead of the batch
// failing fast.
func taggedReplyBudget(byIdx map[int]string, ceiling int) (maxChars, maxTokens int) {
	for _, text := range byIdx {
		maxChars += len(text) + taggedReplyLineAllowance
	}
	return maxChars, min(ceiling, maxChars/2+taggedReplyHeadroomTokens)
}

// taggedReplyParser wraps parseTaggedLines for generateAndParse, rejecting
// a reply longer than maxChars (see taggedReplyBudget) as a runaway so a
// fresh attempt at a higher temperature gets a chance (see
// retryTempBump). The runaway's own lines are still validated and kept on
// the final attempt rather than failing the whole batch - parseTaggedLines
// already drops anything that isn't a faithful, validly-tagged copy of a
// real line, so whatever survives from a runaway is as trustworthy as any
// other reply's.
func taggedReplyParser(origByIdx map[int]string, validTags map[string]bool, maxChars int, pass string) func(string) (map[int]string, error) {
	attempt := 0
	return func(content string) (map[int]string, error) {
		attempt++
		if len(content) > maxChars {
			if attempt <= maxGenerateRetries {
				return nil, fmt.Errorf("%s reply is %d chars, longer than any valid reply to this batch (%d) - likely a runaway generation", pass, len(content), maxChars)
			}
			log.Printf("speakerattr: %s reply still a runaway (%d chars > %d) on the final attempt - keeping whichever lines validate", pass, len(content), maxChars)
		}
		return parseTaggedLines(content, origByIdx, validTags)
	}
}

// directionSystemPrompt is deliberately conservative in the same direction
// describeSystemPrompt's own precision-tuning history pushed it: asking
// for a high bar and an expected-short list up front, rather than tagging
// by default and dialing back later - see describeBatchParagraphs' own
// doc comment for the "streak" bias this heads off from the start rather
// than discovering the same way.
//
// Non-narrative front/back-matter (copyright pages, dedications,
// table-of-contents/index entries, bare epigraph attributions) gets
// <|prosody:expressive_low|> here rather than through a separate
// classify-and-skip pass: epub parsing (internal/epub) turns that content
// into ordinary paragraphs like any other, so it already flows through
// this same per-chapter call, and expressive_low already exists in
// validSentenceTags with exactly the right meaning (flatter, less
// expressive delivery) - no new tag, pass, or endpoint needed. This is a
// second, content-type trigger for a tag the prompt otherwise only reaches
// for on emotional grounds, the same "same tag, two different triggers"
// shape sfxSystemPrompt's own breath-pause addition uses for
// <|prosody:pause|> - see its doc comment.
const directionSystemPrompt = `You are a voice director for an audiobook narrator. Given numbered lines from a novel (narration and/or spoken dialogue), decide which lines deserve inline delivery tags marking a specific emotional or vocal-style/pacing shift, so a listener actually hears that shift in the narrator's voice - or, separately, a non-narrative line that calls for a flatter, more administrative delivery regardless of its tone.

Reply with ONLY plain text lines, no JSON, no code fence, no other commentary: one line per tagged line, made of that line's own number, a colon and a space, then that line's own text with its tag(s) inserted. For example, if "Lines:" contained

12: “Get out of my house!” she screamed.

then tagging it would be this one reply line:

12: <|emotion:anger|><|style:shouting|>“Get out of my house!” she screamed.

Include a line ONLY for a line where at least one tag is warranted; omit every other line entirely. Most lines are neutral and should be omitted - expect this list to be short, not one entry per line. If no line needs a tag, reply with nothing at all.

CRITICAL RULE: the text after the line number and colon must be the line's own original text, character for character, with ONLY tags inserted - never add, remove, reorder, or reword a single word of the actual line. The only thing you are allowed to change is where tags are inserted.

A tag is inserted immediately before the sentence, clause, or quoted phrase whose delivery it colors. Most lines need at most one tag, placed at the very start of the line. A longer line whose tone genuinely shifts partway through - dialogue that turns from calm to angry mid-sentence, a quote that starts warm and pivots to fear - may have a second tag inserted right at the point where the shift happens, still leaving every word exactly where it was.

A single position may get more than one tag, back-to-back with nothing between them (e.g. "<|emotion:anger|><|style:shouting|>He slammed the door"), when that moment genuinely has more than one independent quality at once - an emotion together with a vocal style ("<|emotion:awe|><|style:shouting|>"), an emotion together with a pacing cue ("<|emotion:anger|><|prosody:speed_fast|>"), or a style together with a pacing cue. Only stack tags from genuinely different categories (Emotion, Style, Pacing) this way; never stack two tags from the same category at one position (e.g. two emotions) - pick the single best one for that category instead. When stacking, always order them Emotion, then Style, then Pacing, regardless of the order those qualities occur to you.

Only these exact tag strings are valid - copy one verbatim, never invent a new one, never combine two into one, never change the wording inside the pipes:
Emotion: <|emotion:amusement|>, <|emotion:anger|>, <|emotion:awe|>, <|emotion:bitterness|>, <|emotion:contemplation|>, <|emotion:determination|>, <|emotion:disgust|>, <|emotion:helplessness|>, <|emotion:pride|>, <|emotion:relief|>, <|emotion:shame|>, <|emotion:surprise|>
Style: <|style:shouting|>
Pacing: <|prosody:speed_very_slow|>, <|prosody:speed_slow|>, <|prosody:speed_fast|>, <|prosody:speed_very_fast|>, <|prosody:expressive_high|>, <|prosody:expressive_low|>

Rules:
- For dialogue, base the tag on how the line is actually spoken - the words themselves, any dialogue tag right after it ("she whispered," "he shouted," "Maya said, fighting back tears"), and the surrounding scene. A dialogue tag naming a manner of speaking is strong evidence: "she screamed" -> <|style:shouting|>.
- Emotion and Style tags are dialogue-only - never apply either to narration, even a genuinely emotional passage or one describing a shout/whisper/song (any emotion or style tag placed on narration is discarded automatically, so don't spend effort on it). Of the Pacing tags, narration may ONLY ever get <|prosody:expressive_low|> or <|prosody:expressive_high|> (a controlled shift in the narrator's own overall vocal expressiveness) - speed_* tags alter the narrator's consistent voice too much and are likewise discarded automatically if placed on narration, so don't spend effort on those there either. Reserve expressive_low/high for a span whose own delivery genuinely calls for it (e.g. a tense passage read with more expression, a flat/somber passage read with less) - plain descriptive or plot-advancing narration gets no tag at all, even if the scene it's part of is emotional overall.
- Non-narrative content: a line that isn't really part of the story - a copyright notice, publisher's legal boilerplate, an ISBN, a dedication line ("For my mother"), a table-of-contents entry, an index entry, or a bare epigraph attribution ("-Robert Frost") - gets <|prosody:expressive_low|> at its very start, so it reads in a flatter, more administrative voice than ordinary narration. This is a content-type judgment, not an emotional one: apply it even to a line with no feeling in it at all. But only to genuinely non-narrative material - never to ordinary expository or descriptive prose just because it happens to be calm, and never to the quoted work of an epigraph itself (the poem or line being epigraphed is still narrated normally; only a bare trailing attribution line gets flattened).
- The bar is high: only tag a span where the emotion/style/pacing/non-narrative judgment is clear and strong, not merely plausible. An ordinary, calm line of dialogue or narration should get no tag - most lines in any chapter get none, and that's expected, not a sign you're being too conservative.
- Never tag a line just because a character's name or a strong word appears in it - judge the line's own actual delivery, not its topic.
- Each entry's "idx" must be copied exactly from the number printed right before that line in "Lines:" - never shift it to a neighboring line.`

func (c *Client) directionBatch(ctx context.Context, bookTitle, chapterTitle string, batch []ParagraphInput) (map[int]string, error) {
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nChapter: %s\n", bookTitle, chapterTitle)
	user.WriteString("\nLines:\n")
	for i, p := range batch {
		if i > 0 && !p.Inline {
			user.WriteString("\n")
		}
		fmt.Fprintf(&user, "%d: %s\n", p.Idx, oneLine(p.Text))
	}
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}

	byIdx := make(map[int]string, len(batch))
	for _, p := range batch {
		// oneLine(p.Text), not p.Text itself - that's the actual text the
		// model was shown in "Lines:" above (see the loop that built user),
		// so it's the real common ancestor to diff the model's own echoed-
		// back "text" against, not whatever whitespace the paragraph's raw
		// Text happens to contain.
		byIdx[p.Idx] = oneLine(p.Text)
	}
	maxChars, maxTokens := taggedReplyBudget(byIdx, directionMaxTokens)
	return generateAndParse(ctx, c, directionSystemPrompt, user.String(), directionTemp, maxTokens, taggedReplyParser(byIdx, validSentenceTags, maxChars, "direction"))
}

// allowedNonQuoteTags is the full set a paragraph that isn't actual quoted
// dialogue (ParagraphInput.IsQuote false) is ever allowed to keep, from
// either delivery-tagging pass (direction.go's sentence tags or sfx.go's
// inline tags) and regardless of source (an LLM call's own judgment, or
// either pass's own regex heuristic backstop - addHeuristicSentenceTags/
// addHeuristicSfxTags) - see restrictNonQuoteTags, which enforces this.
// Every other tag - every <|emotion:...|>, every <|style:...|>, the
// speed_* half of prosody, and the crying/screaming/burping/sneeze
// sfx tags - colors a line's delivery a step beyond what the narrator's
// own consistent voice can take without sounding jarring or "in character"
// when it isn't; a quiet vocalization (a hum, a sigh, a soft laugh, a
// cough, a sniff), a pause, or a controlled shift in overall vocal
// expressiveness (expressive_low/high) is as far as narration ever goes.
var allowedNonQuoteTags = map[string]bool{
	"<|sfx:humming|>":             true,
	"<|sfx:sigh|>":                true,
	"<|sfx:laughter|>":            true,
	"<|sfx:cough|>":               true,
	"<|sfx:sniff|>":               true,
	"<|prosody:pause|>":           true,
	"<|prosody:long_pause|>":      true,
	"<|prosody:expressive_low|>":  true,
	"<|prosody:expressive_high|>": true,
}

// restrictNonQuoteTags drops any insertion not in allowedNonQuoteTags from
// every paragraph in batch that isn't actual quoted dialogue
// (ParagraphInput.IsQuote false), regardless of which pass or source
// (LLM or heuristic) placed it - enforced here in Go rather than relying
// on either system prompt's own instructions alone, the same "don't just
// ask nicely, enforce it" pattern this package already uses elsewhere
// (e.g. parseTaggedLines's own trailing-tag filter). Called once per batch
// by both DirectChapter and TagSfx, after that pass's own heuristic
// backstop has already run - neither the LLM call nor either heuristic
// function checks IsQuote on its own, so this is the single place both
// converge before a tag is ever persisted. Only ever touches tagged's
// entries for paragraphs actually in batch, so it's safe to pass a whole
// chapter's accumulated result map without disturbing other batches'
// already-finalized entries. Mutates tagged in place; a paragraph left
// with no insertions after stripping is removed from the map entirely,
// the same "no tag" state as one no source ever touched at all.
func restrictNonQuoteTags(batch []ParagraphInput, origByIdx map[int]string, tagged map[int]string) {
	for _, p := range batch {
		if p.IsQuote {
			continue
		}
		text, ok := tagged[p.Idx]
		if !ok {
			continue
		}
		orig := origByIdx[p.Idx]
		insertions, err := deliverytags.ExtractInsertions(orig, text)
		if err != nil {
			continue // shouldn't happen - parseTaggedLines/addHeuristic* already validated this
		}
		kept := make([]deliverytags.Insertion, 0, len(insertions))
		changed := false
		for _, ins := range insertions {
			if allowedNonQuoteTags[ins.Tag] {
				kept = append(kept, ins)
				continue
			}
			changed = true
		}
		if !changed {
			continue
		}
		if len(kept) == 0 {
			delete(tagged, p.Idx)
			continue
		}
		tagged[p.Idx] = deliverytags.Merge(orig, kept)
	}
}

// taggedLineRe matches one line of the model's own answer format -
// "<idx>: <text>" - see parseTaggedLines' own doc comment for why this
// replaced a JSON array. Leading whitespace is trimmed by the caller
// before matching, so this only has to anchor the digits at the line's own
// start; the space after the colon is optional since some responses omit
// it.
var taggedLineRe = regexp.MustCompile(`^(\d+):\s?(.*)$`)

// parseTaggedLines scans content line by line for the model's own
// "<idx>: <text>" answer format (see directionSystemPrompt/
// sfxSystemPrompt's own instruction) - replacing this package's original
// JSON-array design (a `[{"idx":..., "text":...}]` shape parsed via
// unmarshalRepairedArray, same as parseAttributions still uses) after that
// design kept failing in production: asking the model to reproduce a
// paragraph's own real text verbatim inside a JSON string value means
// every literal quote character already IN that text (curly ” and ASCII "
// alike - both extremely common in narrated dialogue) has to be perfectly
// escaped or avoided, and it wasn't, in a new way every few chapters -
// dropping the JSON string's own closing quote
// (repairDroppedClosingQuoteInArray), using its own curly quotes as if
// they were the JSON delimiter (repairMissingOpeningQuoteInArray), and
// (the case that motivated this rewrite - a real, reproducible failure on
// chapter 66 of "Jake's Magical Market 3", paragraph 115, `“Not too bad,”`)
// silently substituting a bare ASCII " for its own curly quote, which
// still terminates the JSON string early and leaves the inserted tag
// dangling outside it as invalid syntax. Two targeted regex patches
// already existed for the first two variants (jsonrepair.go) and neither
// covered the third - and because JSON requires the *entire* response to
// parse as one well-formed document, any one of these on any single line
// discarded that whole batch's taggings, not just the bad line's.
//
// This format needs no quoting or escaping at all - a line is copied back
// verbatim with 0+ tags spliced in, nothing to mis-escape - and parses one
// line at a time, so a bad line only ever costs that one line: every other
// line in the same response is still scanned, validated, and kept
// independently. Deliberately never returns an error: a response with no
// matching lines at all is the ordinary, overwhelmingly common "nothing
// here needs a tag" outcome both system prompts explicitly ask for
// ("expect this list to be short"/"most lines need none of this"), not a
// failure to retry a whole fresh generation over - unlike the old
// parseTaggedLines, which had to treat "no JSON array found" as an error
// since the model was asked to always emit at least `[]`. A genuine
// transport/API failure (a dead connection, a timeout) still surfaces as a
// Go error from c.generate before this function is ever called.
//
// Each candidate line still goes through the exact same validation
// parseTaggedLines always ran on every JSON array entry: idx must be one
// this batch actually sent (origByIdx), the text after "idx: " must be a
// pure tag-insertion variant of that line's own real text
// (deliverytags.ExtractInsertions - a changed, added, or removed word
// means the model rewrote content, not just tagged it), and every inserted
// tag must be in validTags. A line failing any of these is dropped, same
// as before - a hallucinated or malformed result is worse than no tag at
// all (see validSentenceTags/validInlineTags' own doc comments). An
// insertion landing at Offset == len(orig) - the very end of the line,
// with nothing left to color - is likewise dropped rather than kept, same
// reasoning as before: audio.cpp would render it as a stray, uncolored
// artifact.
func parseTaggedLines(content string, origByIdx map[int]string, validTags map[string]bool) (map[int]string, error) {
	out := make(map[int]string, len(origByIdx))
	for _, line := range strings.Split(content, "\n") {
		m := taggedLineRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		idx, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		orig, ok := origByIdx[idx]
		if !ok {
			continue
		}
		text := m[2]
		insertions, err := deliverytags.ExtractInsertions(orig, text)
		if err != nil || len(insertions) == 0 {
			continue
		}
		valid := true
		for _, ins := range insertions {
			if !validTags[ins.Tag] {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		var kept []deliverytags.Insertion
		for _, ins := range insertions {
			if ins.Offset == len(orig) {
				continue
			}
			kept = append(kept, ins)
		}
		if len(kept) == 0 {
			continue
		}
		if len(kept) != len(insertions) {
			text = deliverytags.Merge(orig, kept)
		}
		out[idx] = text
	}
	return out, nil
}

// DirectChapter tags chapterTitle's paragraphs with sentence-level Higgs
// delivery tags (see validSentenceTags), batching in groups of
// directionBatchParagraphs. Returns a map from paragraph Idx to that
// line's own text with 0 or more tags inserted (see parseTaggedLines's own
// validation) - a paragraph absent from the result gets no annotation,
// the overwhelmingly common case (see directionSystemPrompt's own "expect
// this list to be short" instruction). Persisted via
// Store.SetParagraphSentenceTags.
//
// Unlike AttributeChapter/DescribeChapter, this has no knownCharacters/
// canonicalization step - a delivery tag is a per-line judgment with
// nothing to stay consistent with across batches or chapters, unlike a
// character's own name.
//
// Every paragraph actually reached also runs through
// addHeuristicSentenceTags - the sentence-level analog of TagSfx's own
// addHeuristicSfxTags (see its doc comment for the general shape/
// reasoning): purely typographic cues (see heuristicSentencePatterns)
// caught by regex rather than spent on the LLM.
//
// shouldPause/remaining/err follow AttributeChapter's own contract exactly
// (see its doc comment): nil-safe shouldPause, checked between batches
// only; remaining holds whatever wasn't reached (by a pause or a genuine
// failure) so the caller can persist real progress and requeue just the
// rest.
func (c *Client) DirectChapter(ctx context.Context, bookTitle, chapterTitle string, paragraphs []ParagraphInput, shouldPause func() bool) (out map[int]string, remaining []ParagraphInput, err error) {
	out = make(map[int]string, len(paragraphs))

	for start := 0; start < len(paragraphs); start += directionBatchParagraphs {
		if start > 0 && shouldPause != nil && shouldPause() {
			remaining = paragraphs[start:]
			break
		}
		end := start + directionBatchParagraphs
		if end > len(paragraphs) {
			end = len(paragraphs)
		}
		batch := paragraphs[start:end]

		tagged, err := c.directionBatch(ctx, bookTitle, chapterTitle, batch)
		if err != nil {
			return out, paragraphs[start:], fmt.Errorf("paragraphs %d-%d: %w", batch[0].Idx, batch[len(batch)-1].Idx, err)
		}
		for idx, text := range tagged {
			out[idx] = text
		}

		origByIdx := make(map[int]string, len(batch))
		for _, p := range batch {
			orig := oneLine(p.Text)
			origByIdx[p.Idx] = orig
			var existing []deliverytags.Insertion
			if text, ok := out[p.Idx]; ok {
				// see TagSfx's own identical comment: a diff error here
				// would mean this package's own invariant broke, not bad
				// LLM output - treat as "nothing to build on".
				existing, _ = deliverytags.ExtractInsertions(orig, text)
			}
			merged := addHeuristicSentenceTags(orig, existing)
			if len(merged) != len(existing) {
				out[p.Idx] = deliverytags.Merge(orig, merged)
			}
		}
		restrictNonQuoteTags(batch, origByIdx, out)
	}
	return out, remaining, nil
}
