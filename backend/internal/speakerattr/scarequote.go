package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// ScareQuoteChapter is a separate, single-purpose pass over a chapter's already-quote-split paragraphs
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
// Isolated into its own call rather than folded into AttributeChapter:
// a second judgment in a large per-line batch measurably hurt precision
// when description tagging (since retired) was folded in that way, and a
// scare quote is the same shape of rare, easy-to-over-tag judgment.
//
// A flagged paragraph is never re-split or merged by this package itself -
// see httpapi/jobs' own generation-time handling, which walks
// ParagraphInput.Inline both directions from a flagged paragraph to
// recover the full run of segments split out of one original source
// paragraph (store.Paragraph.Inline's own doc comment) and generates them
// as one TTS call, later sliced back into each paragraph's own audio file
// by forced alignment - paragraph rows/indices themselves never change.
const scareQuoteSystemPrompt = `You are a literary analysis assistant preparing a novel for an audiobook. Given numbered lines from a novel (narration and quoted spans), decide for each line marked "(?)" whether its quoted words are spoken aloud by a character at that moment in the scene - or whether the quotation marks are the narrator's own, a "scare quote" the narrator should read as part of the sentence.
Reply with ONLY plain text lines, no commentary: one per "(?)" line, its number, a colon and a space, then "spoken" or "not spoken". For example:

12: spoken
15: not spoken

Every "(?)" line gets exactly one reply line; the other lines are context only. Copy each number exactly as printed.

The "(?)" lines all sit inside narration sentences, so most of them are the narrator's own quotation marks. Answer "spoken" only when the sentence itself shows a character saying those words aloud at that moment (said, asked, called, barked, gasped, getting a hurried "..." from someone) - never on a guess that someone might have said them.

"not spoken" - nobody says these words aloud right then:
- a term, name, nickname, label, title, sign, or screen/system text, or a word being talked about: the "custodian" class, a big "no smoking" sign, the one marked as "Leo", the local word for "hello," phrases like "field genomics" were tossed about
- a choice picked from a menu, list or prompt: selecting "high" and "fast."
- sarcasm or skepticism: a "friendly" wager, the government's "temporary" fix
- words thought, felt, meant, imagined, held back or only mouthed: nobody would think "he's a lunatic," the usual feeling of "now I'm in trouble," a look that clearly said "goodbye," I stopped myself from screaming "no," she mouthed "later."
- words reported, paraphrased or summed up inside a narration sentence rather than said in the scene: grumbling something about "not an errand boy," his questions like "are you okay?", what he called "taking some time out," a chorus of "good luck" coming back, a "wait" gesture
- a translation or gloss of what someone said: literally saying "I speak a little Danish."

"spoken" - a character says these words aloud right then, even when the quote sits inside a narration sentence: she barked at him to "Run, you idiot!", getting a hurried "thank you" as she closed the door, a gasped "Yes.", he said, shaking his head, "They took her." When the words are said aloud in the scene, it is "spoken", however the sentence around them is built.`

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
// shouldPause/remaining/err follow AttributeChapter's own
// contract (see its doc comment) - nil-safe shouldPause, checked
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
			raw, berr := c.scareQuoteBatch(ctx, bookTitle, chapterTitle, batch, candidate)
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
// ScareQuoteCandidates is scareQuoteCandidates for callers outside the
// package (the scare-quote preview reports which lines were asked about).
func ScareQuoteCandidates(paragraphs []ParagraphInput) map[int]bool {
	return scareQuoteCandidates(paragraphs)
}

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

// scareQuoteUserPrompt is one batch's user turn and the lines it asks
// about (the batch's "(?)" lines, in order).
func (c *Client) scareQuoteUserPrompt(bookTitle, chapterTitle string, batch []ParagraphInput, candidate map[int]bool) (string, []int) {
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nChapter: %s\n", bookTitle, chapterTitle)
	user.WriteString("\nLines:\n")
	var asked []int
	for i, p := range batch {
		if i > 0 && !p.Inline {
			user.WriteString("\n")
		}
		if candidate[p.Idx] {
			asked = append(asked, p.Idx)
			fmt.Fprintf(&user, "%d (?): %s\n", p.Idx, oneLine(p.Text))
		} else {
			fmt.Fprintf(&user, "%d: %s\n", p.Idx, oneLine(p.Text))
		}
	}
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}
	return user.String(), asked
}

func (c *Client) scareQuoteBatch(ctx context.Context, bookTitle, chapterTitle string, batch []ParagraphInput, candidate map[int]bool) (map[int]bool, error) {
	user, asked := c.scareQuoteUserPrompt(bookTitle, chapterTitle, batch, candidate)
	return generateAndParse(ctx, c, scareQuoteSystemPrompt, user, 0, jsonPassMaxTokens, func(content string) (map[int]bool, error) {
		return parseScareQuoteVerdicts(content, asked)
	})
}

// ScareQuoteVerdict is one candidate's answer from ScareQuoteDecide.
type ScareQuoteVerdict struct {
	// NotSpoken is the model's probability that the quote is a scare quote
	// ("not spoken"), against "spoken". Uncalibrated.
	NotSpoken float64
	// Mass is how much of the model's next-token distribution the two
	// answers held (ttsproto.LLMDecision.Mass).
	Mass float64
}

// ScareQuoteDecide is ScareQuoteChapter's experimental sibling: the same
// batches, prompts and reply format, but each "N: spoken"/"N: not spoken"
// reply line is decided from the model's logits rather than generated - no
// reply to parse, and a probability for every candidate. Returns nothing
// for a paragraph that isn't a candidate. Errors if the backend can't
// decide.
//
// independent asks about each candidate in a call of its own, as the
// reply's first line, so no verdict is conditioned on the ones before it -
// at the cost of reading the batch once per candidate.
func (c *Client) ScareQuoteDecide(ctx context.Context, bookTitle, chapterTitle string, paragraphs []ParagraphInput, independent bool) (map[int]ScareQuoteVerdict, error) {
	decider, ok := c.llm.(llmDecider)
	if !ok {
		return nil, fmt.Errorf("speakerattr: this LLM backend has no decide call")
	}
	candidate := scareQuoteCandidates(paragraphs)
	out := map[int]ScareQuoteVerdict{}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
	)
	sem := make(chan struct{}, scareQuoteParallelBatches)
	for start := 0; start < len(paragraphs); start += scareQuoteBatchParagraphs {
		batch := paragraphs[start:min(start+scareQuoteBatchParagraphs, len(paragraphs))]
		user, all := c.scareQuoteUserPrompt(bookTitle, chapterTitle, batch, candidate)
		calls := [][]int{all}
		if independent {
			calls = nil
			for _, idx := range all {
				calls = append(calls, []int{idx})
			}
		}
		for _, asked := range calls {
			if len(asked) == 0 {
				continue
			}
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				verdicts, err := scareQuoteDecideCall(ctx, decider, user, asked)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("paragraphs %d-%d: %w", batch[0].Idx, batch[len(batch)-1].Idx, err)
					}
					return
				}
				for i, v := range verdicts {
					out[asked[i]] = v
				}
			}()
		}
	}
	wg.Wait()
	return out, firstErr
}

// scareQuoteDecideCall decides the asked lines of one batch, in order: the
// reply scareQuoteBatch would have generated, "N:" written for the model
// and the verdict after it read from its logits.
func scareQuoteDecideCall(ctx context.Context, decider llmDecider, user string, asked []int) ([]ScareQuoteVerdict, error) {
	steps := make([]ttsproto.LLMDecideStep, len(asked))
	for i, idx := range asked {
		prefix := fmt.Sprintf("%d:", idx)
		if i > 0 {
			prefix = "\n" + prefix
		}
		steps[i] = ttsproto.LLMDecideStep{Prefix: prefix, Options: []string{" spoken", " not spoken"}}
	}
	decisions, err := decider.LLMDecide(ctx, scareQuoteSystemPrompt, user, steps)
	if err != nil {
		return nil, err
	}
	out := make([]ScareQuoteVerdict, len(decisions))
	for i, d := range decisions {
		out[i] = ScareQuoteVerdict{NotSpoken: d.Probs[1], Mass: d.Mass}
	}
	return out, nil
}

// scareQuoteVerdictRe matches one reply line: "<idx>: spoken" / "<idx>:
// not spoken", tolerating a copied "(?)" marker.
var scareQuoteVerdictRe = regexp.MustCompile(`(?i)^\s*(\d+)\s*(?:\(\?\))?\s*[:\-–]\s*(not\s+spoken|spoken)\b`)

// parseScareQuoteVerdicts reads scareQuoteBatch's reply: the asked lines
// (the batch's "(?)" lines, in order) answered "not spoken". A reply with no
// verdict for any of them is an error (so generateAndParse retries it); a
// model falling back to the old JSON array of flagged lines still parses.
//
// A verdict whose number isn't an asked line still counts when the reply
// has exactly one verdict per asked line, in order: it's taken as the line
// in its position. Qwen3.6 miscopies a number now and then - a batch whose
// only "(?)" line was 214 answered "14: not spoken" on every retry, at
// every retry temperature, so the chapter's pass could never finish. A
// reply of only "spoken" verdicts for lines that weren't asked flags
// nothing; one saying "not spoken" for such a line is still an error.
func parseScareQuoteVerdicts(content string, asked []int) (map[int]bool, error) {
	isAsked := make(map[int]bool, len(asked))
	for _, idx := range asked {
		isAsked[idx] = true
	}
	type verdict struct {
		idx       int
		notSpoken bool
	}
	var verdicts []verdict
	for _, line := range strings.Split(content, "\n") {
		m := scareQuoteVerdictRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		idx, _ := strconv.Atoi(m[1])
		verdicts = append(verdicts, verdict{idx, strings.HasPrefix(strings.ToLower(m[2]), "not")})
	}
	if len(verdicts) == len(asked) {
		inOrder := true
		for i, v := range verdicts {
			if v.idx != asked[i] && isAsked[v.idx] {
				inOrder = false // a real asked number out of place - trust the numbers
			}
		}
		if inOrder {
			for i := range verdicts {
				verdicts[i].idx = asked[i]
			}
		}
	}
	out := map[int]bool{}
	answered := 0
	for _, v := range verdicts {
		if !isAsked[v.idx] {
			continue
		}
		answered++
		if v.notSpoken {
			out[v.idx] = true
		}
	}
	if answered == 0 {
		if items, err := parseScareQuoteItems(content); err == nil {
			return items, nil
		}
		// Verdicts, but none for an asked line and not one apiece - the
		// model miscopied a number and skipped a line. If every one says
		// "spoken", nothing is flagged whichever lines it meant, so that
		// is the answer rather than a failure: a batch asked about 122
		// and 152 answered only "132: spoken" on every retry, and the
		// chapter's pass failed for good.
		if len(verdicts) > 0 && !slices.ContainsFunc(verdicts, func(v verdict) bool { return v.notSpoken }) {
			return out, nil
		}
		return nil, fmt.Errorf("no verdict lines in response: %s", truncate(content, 200))
	}
	return out, nil
}

type scareQuoteItem struct {
	Idx int `json:"idx"`
}

// parseScareQuoteItems extracts the JSON array from content, tolerating
// models that wrap it in prose or a markdown code fence despite the system
// prompt asking for JSON only - the same tolerance parseAttributions
// applies. Also tolerates a model
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
