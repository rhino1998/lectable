package speakerattr

import (
	"context"
	"fmt"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/deliverytags"
)

// validInlineTags is the "inline" half of Higgs Audio v3 TTS's own
// delivery-tag vocabulary - see validSentenceTags' own doc comment for the
// full split rationale. Each of these marks an exact point within a line
// rather than coloring a whole span: an sfx tag sits immediately before an
// onomatopoeia/interjection word already present in the line (no space -
// "<|sfx:laughter|>Haha" - per PROMPTING.md's own sfx example), and
// prosody's pause/long_pause sit between two existing words/phrases
// (with a space on each side - "Hello there <|prosody:pause|> and
// welcome").
var validInlineTags = map[string]bool{
	"<|sfx:cough|>":          true,
	"<|sfx:laughter|>":       true,
	"<|sfx:crying|>":         true,
	"<|sfx:screaming|>":      true,
	"<|sfx:burping|>":        true,
	"<|sfx:humming|>":        true,
	"<|sfx:sigh|>":           true,
	"<|sfx:sniff|>":          true,
	"<|sfx:sneeze|>":         true,
	"<|prosody:pause|>":      true,
	"<|prosody:long_pause|>": true,
}

// sfxBatchParagraphs/sfxMaxTokens mirror directionBatchParagraphs/
// directionMaxTokens exactly - same "echoes full line text back, keep
// batches small" reasoning (see their own doc comments).
const sfxBatchParagraphs = 10
const sfxMaxTokens = 32768

// sfxSystemPrompt asks for a narrower, more literal-minded judgment than
// directionSystemPrompt's own "what's the dominant emotion here": sfx tags
// may only ever be attached to an onomatopoeia the line already spells
// out, never invented, and content pauses only where the narration itself
// describes a real beat of silence - both explicitly instructed against
// the model getting creative, since positional insertion has no natural
// "pick the closest match" fallback the way a sentence-level emotion
// judgment does. Breath pauses (added later - see their own bullet below)
// are the one deliberate exception to "only tag what the text already
// says": they're triggered by a sentence's own length/structure, not
// anything it describes, but are kept in this same prompt/tag family
// rather than split into their own pass - unlike the direction/sfx split
// (a genuinely different kind of judgment - emotional vs. positional),
// content pauses and breath pauses are the same judgment (where does a
// narrator's voice need a beat of silence) and the same tag
// (<|prosody:pause|>), just two different triggers for reaching for it.
//
// <|sfx:laughter|>, <|sfx:humming|>, <|sfx:sneeze|>, <|sfx:cough|>, and
// <|sfx:sigh|> are deliberately left off this prompt entirely, not merely
// discouraged - each has a small, closed set of real-world spellings (see
// heuristicSfxPatterns) that TagSfx's own addHeuristicSfxTags catches by
// regex after this call returns, for free and deterministically, so
// there's nothing left for the model to usefully add by also looking for
// them, and every token spent listing/explaining them would be pure
// bloat. Only <|sfx:crying|>, <|sfx:screaming|>, and <|sfx:burping|> stay
// in this prompt's own "Valid" list - the ones with no one fixed spelling
// a regex could reliably recognize. Content pauses similarly drop "an
// ellipsis" from their own example list below - also now regex-owned (see
// heuristicSfxPatterns) - while keeping the genuinely judgment-dependent
// examples ("she paused," "after a long moment") the model still needs to
// catch itself.
//
// Of the sfx tags - regex-caught or LLM-judged alike - only humming, sigh,
// laughter, cough, and sniff ever survive on a narration (non-quote) line;
// crying, screaming, burping, and sneeze are stripped from narration by
// restrictNonQuoteTags (direction.go) after this pass returns, regardless
// of source, since an actual person crying/screaming/burping/sneezing only
// makes sense as dialogue, not narration describing the action.
const sfxSystemPrompt = `You are a sound-effect and pacing director for an audiobook narrator. Given numbered lines from a novel, find lines that already contain a spoken interjection or onomatopoeia worth tagging as a sound effect, or a place where a pause clearly belongs - either a dramatic beat the narration itself describes, or a breath pause in an unusually long, unbroken sentence.

Reply with ONLY plain text lines, no JSON, no code fence, no other commentary: one line per tagged line, in exactly this format - <line number>: <the line's own text with 0 or more tags inserted> - include a line ONLY for a line that clearly qualifies; omit every other line entirely. This should be a very short list - most lines need none of this. If no line qualifies, reply with nothing at all.

CRITICAL RULE: the text after "<line number>: " must be the line's own original text, character for character, with ONLY tags inserted - never add, remove, reorder, or reword a single word of the actual line, and never invent a word that isn't already there. The only thing you are allowed to change is where tags are inserted.

Three independent things to look for - a line can get any combination, or none:
- Sound effects: insert the tag immediately before an interjection/onomatopoeia word that is ALREADY written out in the line, with NO space between the tag and that word (e.g. "<|sfx:screaming|>Aaaah, run!"). Only use one when the line already spells out the sound as a word - never insert one just because the narration says someone screamed/cried/burped without actually writing the sound itself, and never invent a new interjection that isn't already there.
  Valid: <|sfx:crying|>, <|sfx:screaming|>, <|sfx:burping|> - all three are dialogue-only (an actual person making the sound), never narration; any of them placed on narration is discarded automatically, so don't spend effort tagging narration with them.
- Content pauses: insert <|prosody:pause|> or <|prosody:long_pause|> between two existing words or phrases, with a space on each side, where the narration itself clearly calls for a real beat of silence or hesitation ("she paused," "after a long moment," a trailing-off line) - not for ordinary sentence punctuation or a normal comma.
- Breath pauses: separately, a single long sentence with no internal punctuation heavy enough to already force a natural break (no comma, semicolon, dash, or colon for a long stretch) can get one <|prosody:pause|> inserted at the clearest clause boundary - before a coordinating conjunction like "and"/"but"/"or", after a subordinate clause, wherever a narrator reading it aloud would naturally take a breath. This is about the sentence's own length and structure, not anything it describes - it applies just as well to plain, unemotional narration. Rough guide: only for a genuinely long, unbroken run of 30+ words with no comma/semicolon/dash giving the reader a natural break already; an ordinary sentence, even a moderately long one with normal commas, needs nothing. Never use <|prosody:long_pause|> for this - long_pause is reserved for content pauses; a breath pause is always the shorter <|prosody:pause|>. At most one breath pause per sentence.

Rules:
- Use all three sparingly - most lines in any chapter get no tag at all, and that's expected, not a sign you're being too conservative.
- Never tag a line just because a word could theoretically make a sound - only when the line's own words already spell it out or the narration explicitly describes a real pause.
- Don't confuse a long line made of several normally-punctuated sentences with one genuinely long, unbroken sentence - breath pauses are for the latter only.
- Each entry's "idx" must be copied exactly from the number printed right before that line in "Lines:" - never shift it to a neighboring line.`

func (c *Client) sfxBatch(ctx context.Context, bookTitle, chapterTitle string, batch []ParagraphInput) (map[int]string, error) {
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
		byIdx[p.Idx] = oneLine(p.Text) // see DirectChapter's own directionBatch for why oneLine, not p.Text
	}
	return generateAndParse(ctx, c, sfxSystemPrompt, user.String(), 0, sfxMaxTokens, func(content string) (map[int]string, error) {
		return parseTaggedLines(content, byIdx, validInlineTags)
	})
}

// TagSfx tags chapterTitle's paragraphs with inline Higgs delivery tags
// (sound effects and pauses - see validInlineTags), batching in groups of
// sfxBatchParagraphs. Returns a map from paragraph Idx to that line's own
// text with 0 or more tags inserted (see parseTaggedLines's own
// validation) - a paragraph absent from the result gets no annotation,
// the overwhelmingly common case. Persisted via
// Store.SetParagraphInlineTags, independently of DirectChapter's own
// SentenceText - see store.ParagraphDirection's own doc comment for how
// the two compose back together at generation time.
//
// Every paragraph actually reached (i.e. in a batch that got this far,
// pause or no LLM error notwithstanding) also runs through
// addHeuristicSfxTags - regex-detectable onomatopoeia (see
// heuristicSfxPatterns) that costs nothing to catch deterministically and
// doesn't need sfxSystemPrompt's help. addHeuristicSfxTags is a plain
// function with no *Client/LLM dependency specifically so it belongs to
// the delivery-tagging step as a whole, not to this one LLM call - nothing
// stops it running against paragraphs this call's own LLM pass had
// nothing to say about, or being reused from elsewhere in this step later.
//
// shouldPause/remaining/err follow AttributeChapter's own contract exactly
// (see its doc comment).
func (c *Client) TagSfx(ctx context.Context, bookTitle, chapterTitle string, paragraphs []ParagraphInput, shouldPause func() bool) (out map[int]string, remaining []ParagraphInput, err error) {
	out = make(map[int]string, len(paragraphs))

	for start := 0; start < len(paragraphs); start += sfxBatchParagraphs {
		if start > 0 && shouldPause != nil && shouldPause() {
			remaining = paragraphs[start:]
			break
		}
		end := start + sfxBatchParagraphs
		if end > len(paragraphs) {
			end = len(paragraphs)
		}
		batch := paragraphs[start:end]

		tagged, err := c.sfxBatch(ctx, bookTitle, chapterTitle, batch)
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
				// tagged came straight out of parseTaggedLines, which
				// already validated it against this same orig - a diff
				// error here would mean this package's own invariant
				// broke, not bad LLM output, so there's nothing useful to
				// do but treat it as "nothing to build on" rather than
				// fail the whole batch over it.
				existing, _ = deliverytags.ExtractInsertions(orig, text)
			}
			merged := addHeuristicSfxTags(orig, existing)
			if len(merged) != len(existing) {
				out[p.Idx] = deliverytags.Merge(orig, merged)
			}
		}
		restrictNonQuoteTags(batch, origByIdx, out)
	}
	return out, remaining, nil
}
