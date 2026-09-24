package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ScareQuoteChapter is DescribeChapter's own sibling in shape: a separate,
// single-purpose pass over a chapter's already-quote-split paragraphs
// (internal/epub.splitQuoteSegments/store.Paragraph.IsQuote), asking
// nothing about who's speaking or what's described - only whether a
// quoted span (internal/epub splits out every balanced quote-mark pair as
// IsQuote, with no judgment of its own) is actually genuine spoken
// dialogue at all, or a "scare quote": quotation marks used for
// sarcasm/skepticism, mentioning a term/title/label rather than uttering
// it, or quoting a written source (a sign, a book, an inscription) rather
// than speech in the scene. This is the only place that distinction is
// made - telling `the "downward" stairway` or "the so-called 'chosen one,'
// he thought derisively" apart from real dialogue needs semantic judgment
// a structural parser can't do, the reason this is an LLM pass at all.
//
// Isolated into its own call, at DescribeChapter's own small batch size,
// for the same reason DescribeChapter itself was split out of
// AttributeChapter (see that package doc comment's own "Describe" section
// and describeBatchParagraphs' doc comment): folding a second judgment
// into a large per-line batch measurably hurt precision there, and a scare
// quote is exactly the same shape of rare, easy-to-over-tag judgment.
//
// A flagged paragraph is never re-split or merged by this package itself -
// see httpapi/jobs' own generation-time handling, which walks
// ParagraphInput.Inline both directions from a flagged paragraph to
// recover the full run of segments split out of one original source
// paragraph (store.Paragraph.Inline's own doc comment) and generates them
// as one TTS call, later sliced back into each paragraph's own audio file
// by forced alignment - paragraph rows/indices themselves never change.
const scareQuoteSystemPrompt = `You are a literary analysis assistant. Given numbered lines from a novel (a mix of narration and quoted spans), find the very rare quoted lines that are NOT actually spoken aloud by a character - a "scare quote" - as opposed to real spoken dialogue.
Reply with ONLY a JSON array, no other text: [{"idx": <line number>}, ...] - include an entry ONLY for a line that clears the bar below; omit every other line entirely, including every narration line. Expect this list to almost always be completely empty - the overwhelming majority of chapters contain zero scare quotes. Only flag a line you are confident about.
Rules:
- Only a line already given to you as a quoted span (its own text starts and ends with quotation marks) can ever belong in this list. Never include a plain narration line.
- The default assumption for EVERY quoted line is that it is real spoken dialogue. A quoted line followed or preceded by an ordinary narration line naming who said it - any speech/reaction verb at all (said, asked, replied, muttered, ordered, sputtered, grunted, hissed, groused, called, whispered, yelled, and so on) - is real dialogue, full stop, never a scare quote, no matter how that narration is worded. This is the single most common pattern in the whole book and must never be flagged. This still applies inside one long back-and-forth monologue built from several quote+tag beats strung together in one source paragraph (quote, tag, quote, tag, quote...) - every quote in that chain is still real dialogue, not a scare quote, no matter how short any one beat is.
- A quoted line with NO narration tag at all - a bare reply in the middle of a back-and-forth exchange, a one-word answer, an exclamation - is still real dialogue by default, not a scare quote, exactly the same as a tagged one. Untagged is the normal way a fast exchange is written; it is never on its own a reason to flag a line.
- A "scare quote" is something categorically different from ordinary dialogue: quotation marks wrapped around a single word or short phrase that is grammatically embedded INSIDE a narration sentence which has its own separate subject and main verb having nothing to do with speaking - the quoted words are functioning as a noun or modifier in that sentence, not standing on their own as an utterance. Concrete examples: their so-called "expert" had no idea what he was doing - "expert" is one word embedded in a sentence about what the expert lacked, not something anyone said aloud right then. What they called the "Long Winter" finally broke - "Long Winter" is a label, embedded in a sentence about the season ending. He'd learned to spot a "friendly" wager from a mile away - "friendly" is a skeptical aside inside a sentence about his own skill at spotting something.
- A scare quote is always a fragment - a word or short phrase - never a full sentence, question, or exclamation on its own, and it never has its own dialogue-shaped punctuation (a standalone line ending in "." "?" or "!" with nothing else in the sentence around it is dialogue, not a scare quote). If the quoted span, with the marks removed, could stand alone as a complete thing someone might say out loud, it is dialogue, even if it also happens to sit inside a longer narration sentence.
- Quoting a written source - a sign, book, inscription, or letter someone is reading rather than speaking - also counts as a scare quote, but only when the surrounding narration makes clear it's being read from a page/surface, not spoken.
- When genuinely torn, leave it out - an empty list is very often the fully correct answer, and defaulting to "real dialogue" is always the safer error.
- Each entry's "idx" must be copied exactly from the number printed right before that line in "Lines:" - never shift it to a neighboring line.`

// scareQuoteBatchParagraphs mirrors describeBatchParagraphs' own choice and
// reasoning exactly (see its doc comment) - a scare quote is the same
// shape of rare, binary, easy-to-streak judgment DescribeChapter's own
// benchmarking already found needs a small batch to keep precision high.
const scareQuoteBatchParagraphs = 5

// ScareQuoteChapter tags chapterTitle's quoted paragraphs that are actually
// scare quotes rather than real spoken dialogue, batching in groups of
// scareQuoteBatchParagraphs. Returns the set of paragraph Idx values
// flagged as scare quotes - a paragraph with no entry is real dialogue
// (the common case).
//
// shouldPause/remaining/err follow AttributeChapter/DescribeChapter's own
// contract exactly (see their doc comments) - nil-safe shouldPause,
// checked between batches only, remaining holds whatever wasn't reached
// (by a pause or a genuine failure) so the caller can persist real
// progress and requeue just the rest.
func (c *Client) ScareQuoteChapter(ctx context.Context, bookTitle, chapterTitle string, paragraphs []ParagraphInput, shouldPause func() bool) (out map[int]bool, remaining []ParagraphInput, err error) {
	out = make(map[int]bool, len(paragraphs))
	isQuoteByIdx := make(map[int]bool, len(paragraphs))
	for _, p := range paragraphs {
		isQuoteByIdx[p.Idx] = p.IsQuote
	}

	for start := 0; start < len(paragraphs); start += scareQuoteBatchParagraphs {
		if start > 0 && shouldPause != nil && shouldPause() {
			remaining = paragraphs[start:]
			break
		}
		end := start + scareQuoteBatchParagraphs
		if end > len(paragraphs) {
			end = len(paragraphs)
		}
		batch := paragraphs[start:end]

		raw, err := c.scareQuoteBatch(ctx, bookTitle, chapterTitle, batch)
		if err != nil {
			return out, paragraphs[start:], fmt.Errorf("paragraphs %d-%d: %w", batch[0].Idx, batch[len(batch)-1].Idx, err)
		}
		// Only an actual quoted-dialogue span can ever be a scare quote -
		// enforced here deterministically rather than trusted to the
		// model's own prompt adherence (systemPrompt's own rule 1), the
		// same belt-and-suspenders shape httpapi.attributeChapter applies
		// to AttributeChapter's own Speaker result.
		for idx := range raw {
			if isQuoteByIdx[idx] {
				out[idx] = true
			}
		}
	}
	return out, remaining, nil
}

func (c *Client) scareQuoteBatch(ctx context.Context, bookTitle, chapterTitle string, batch []ParagraphInput) (map[int]bool, error) {
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

	return generateAndParse(ctx, c, scareQuoteSystemPrompt, user.String(), 0, jsonPassMaxTokens, parseScareQuoteItems)
}

type scareQuoteItem struct {
	Idx int `json:"idx"`
}

// parseScareQuoteItems extracts the JSON array from content, tolerating
// models that wrap it in prose or a markdown code fence despite the system
// prompt asking for JSON only - the same tolerance parseDescribeItems
// applies for DescribeChapter's own response. Also tolerates a model
// simplifying [{"idx": N}, ...] down to a flat [N, ...] array of bare
// indices - a real, observed response shape for this particular schema
// (its only field is "idx", an easy one to collapse to just the value)
// that would otherwise cost a full retry every time it happens.
func parseScareQuoteItems(content string) (map[int]bool, error) {
	start := strings.IndexByte(content, '[')
	end := strings.LastIndexByte(content, ']')
	if start == -1 || end == -1 || end < start {
		return nil, fmt.Errorf("no JSON array found in response: %s", truncate(content, 200))
	}
	raw := content[start : end+1]

	out := make(map[int]bool)
	var items []scareQuoteItem
	if err := json.Unmarshal([]byte(raw), &items); err == nil {
		for _, it := range items {
			out[it.Idx] = true
		}
		return out, nil
	}

	var bare []int
	if err := json.Unmarshal([]byte(raw), &bare); err != nil {
		return nil, fmt.Errorf("parse JSON array: %w", err)
	}
	for _, idx := range bare {
		out[idx] = true
	}
	return out, nil
}
