package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
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

// scareQuoteBatchParagraphs is how many paragraphs one scare-quote call
// judges; a batch with no candidate (scareQuoteCandidates) makes no call.
// It was 5 on Qwen3-4B, which lapsed into tagging real dialogue in
// anything larger. Measured on Qwen3.6 over 32 chapters (22 holding the
// library's 30 stored scare quotes, 10 others), against Qwen3-4B at 5 one
// call at a time (47s, 893 calls, 12 found): unfiltered, batch 20 took
// 2m19s for 53 and batch 40 started flagging spoken dialogue ("“Why?”
// Ingrid asked"); with the candidate filter, which rules that out, batch
// 20 took 48s/84 calls for 58 and batch 40 49s/61 calls for 92, sampled
// flags all genuine (terms, labels, titles, sarcasm). A bigger batch
// finds more - the prompt's "almost always empty" framing suppresses
// flags in a small one.
var scareQuoteBatchParagraphs = 40

// scareQuoteParallelBatches is how many batches run at once - the
// worker's 2 LLM slots, attributeParallelBatches' sibling. Batches are
// independent (each sees only its own lines), so order doesn't matter.
const scareQuoteParallelBatches = 2

// ScareQuoteChapter tags chapterTitle's quoted paragraphs that are actually
// scare quotes rather than real spoken dialogue, batching in groups of
// scareQuoteBatchParagraphs, scareQuoteParallelBatches at a time. A batch
// with no quoted line at all makes no call. Returns the set of paragraph
// Idx values flagged as scare quotes - a paragraph with no entry is real
// dialogue (the common case).
//
// shouldPause/remaining/err follow AttributeChapter/DescribeChapter's own
// contract (see their doc comments) - nil-safe shouldPause, checked
// between batches only; remaining holds everything from the first batch
// not completed (by a pause or a genuine failure) so the caller can
// persist real progress and requeue just the rest.
func (c *Client) ScareQuoteChapter(ctx context.Context, bookTitle, chapterTitle string, paragraphs []ParagraphInput, shouldPause func() bool) (out map[int]bool, remaining []ParagraphInput, err error) {
	out = make(map[int]bool, len(paragraphs))
	candidate := scareQuoteCandidates(paragraphs)

	var starts []int
	for start := 0; start < len(paragraphs); start += scareQuoteBatchParagraphs {
		starts = append(starts, start)
	}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		stopAt   = len(starts)
		firstErr error
	)
	sem := make(chan struct{}, scareQuoteParallelBatches)
	for bi, start := range starts {
		if bi > 0 && shouldPause != nil && shouldPause() {
			mu.Lock()
			stopAt = min(stopAt, bi)
			mu.Unlock()
			break
		}
		batch := paragraphs[start:min(start+scareQuoteBatchParagraphs, len(paragraphs))]
		hasCandidate := false
		for _, p := range batch {
			hasCandidate = hasCandidate || candidate[p.Idx]
		}
		if !hasCandidate {
			continue // nothing shaped like a scare quote - no call
		}
		sem <- struct{}{}
		mu.Lock()
		failed := firstErr != nil
		mu.Unlock()
		if failed {
			<-sem
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			raw, berr := c.scareQuoteBatch(ctx, bookTitle, chapterTitle, batch)
			mu.Lock()
			defer mu.Unlock()
			if berr != nil {
				if bi < stopAt {
					stopAt = bi
					firstErr = fmt.Errorf("paragraphs %d-%d: %w", batch[0].Idx, batch[len(batch)-1].Idx, berr)
				}
				return
			}
			// Only a candidate (see scareQuoteCandidates) can be a scare
			// quote, whatever the model answered - a flag on an ordinary
			// line of dialogue is exactly the error to rule out.
			for idx := range raw {
				if candidate[idx] {
					out[idx] = true
				}
			}
		}()
	}
	wg.Wait()
	if stopAt < len(starts) {
		remaining = paragraphs[starts[stopAt]:]
		// A batch after the stop point may have finished first; its flags
		// are dropped with it, so a requeue redoes them consistently.
		for _, p := range remaining {
			delete(out, p.Idx)
		}
	}
	return out, remaining, firstErr
}

// scareQuoteCandidates is which quoted paragraphs could be scare quotes at
// all, by their place in the text - a scare quote sits inside a narration
// sentence, so the epub splitter always leaves it as an Inline segment of
// a longer source paragraph:
//   - mid-sentence: the segment before it (same source paragraph) ends on a
//     word, not punctuation - "my “friends” were", "the “noobs,” meaning";
//   - or a short quote (scareQuoteMaxWords) with no closing punctuation of
//     its own, followed by more of the same sentence - "“Toy yachts” lined
//     the harbor".
//
// Spoken dialogue opens its paragraph or follows a sentence end, comma or
// colon, so it's never asked about. Measured over the library's 39,086
// quoted spans: 1.6% are mid-sentence, and they held all 30 stored flags,
// 41 of Qwen3.6's 42 new batch-20 flags (the other was a spoken line with
// a nested ‘scare quote’ inside it) and none of the dialogue lines batch 40
// wrongly flagged ("“Why?” Ingrid asked").
func scareQuoteCandidates(paragraphs []ParagraphInput) map[int]bool {
	out := map[int]bool{}
	for i, p := range paragraphs {
		if !p.IsQuote {
			continue
		}
		prevMid := false
		if p.Inline && i > 0 && paragraphs[i-1].Idx == p.Idx-1 {
			prev := strings.TrimRight(paragraphs[i-1].Text, " \t")
			if r, _ := utf8.DecodeLastRuneInString(prev); unicode.IsLetter(r) || unicode.IsDigit(r) || r == '’' || r == '\'' || r == '-' {
				prevMid = true
			}
		}
		shortBare := false
		if i+1 < len(paragraphs) && paragraphs[i+1].Inline && paragraphs[i+1].Idx == p.Idx+1 {
			inner := strings.TrimFunc(p.Text, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`"“”'‘’`, r) })
			r, _ := utf8.DecodeLastRuneInString(inner)
			shortBare = len(strings.Fields(inner)) <= scareQuoteMaxWords && (unicode.IsLetter(r) || unicode.IsDigit(r))
		}
		if prevMid || shortBare {
			out[p.Idx] = true
		}
	}
	return out
}

// scareQuoteMaxWords bounds scareQuoteCandidates' short-quote rule.
const scareQuoteMaxWords = 6

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
	value, found := firstJSONValue(content, '[')
	if !found {
		return nil, fmt.Errorf("no JSON array found in response: %s", truncate(content, 200))
	}
	raw := value

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
