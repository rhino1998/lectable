package speakerattr

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/emotions"
)

// emotionBatchParagraphs bounds one EmotionChapter call. Larger than the
// JSON-echoing passes' batches since the reply is only a short label per
// emotional line, and narration lines ride along as context (the "she
// snapped" beat right after a quote is the strongest evidence there is).
const emotionBatchParagraphs = 30

// emotionMaxTokens bounds one batch's reply: at most one short
// "idx: label" line per dialogue line, plus room for a hybrid-thinking
// model's own <think> trace (stripped worker-side - see
// llmworker.stripThinking).
const emotionMaxTokens = 8192

// emotionSystemPrompt is built from internal/emotions so the labels the
// model is offered are always exactly the set the rest of the app knows
// how to render.
var emotionSystemPrompt = func() string {
	var b strings.Builder
	b.WriteString(`You are a voice director for an audiobook. Given numbered lines from a novel - a mix of narration and dialogue, where dialogue lines are marked "(dialogue)" - decide which dialogue lines are clearly spoken with one of the emotions or speaking modes below, so the character's voice can be performed that way.

Reply with ONLY plain text lines, no JSON, no code fence, no commentary: one line per emotional dialogue line, made of that line's number, a colon and a space, then exactly one label from the list below. For example:

12: angry
15: whisper

Omit every other line. Most dialogue is spoken in a normal, neutral way and must be omitted - expect this list to be short. If no line qualifies, reply with nothing at all.

Labels (copy one verbatim):
`)
	for _, e := range emotions.All {
		fmt.Fprintf(&b, "- %s: %s\n", e.ID, e.Description)
	}
	b.WriteString(`
Rules:
- Only ever label a line marked "(dialogue)". Narration lines are there for context only and must never appear in your reply.
- Judge how the line is actually spoken: the words themselves, the narration around it ("she hissed", "he bellowed", "Maya said, fighting back tears"), and the scene. A narration beat naming a manner of speaking is strong evidence - "she whispered" -> whisper, "he roared" -> shout.
- whisper and shout are about volume and should win over an emotion when the narration says a line was whispered or shouted.
- Pick the closer of neighboring labels by intensity and kind: nervous is mild unease, afraid is real fear; pleading is begging someone for something, not fear itself; teasing is playful, cold is contemptuous; pained is physical hurt, weary is exhaustion.
- The bar is high: only label a line whose emotion is clear and strong, not merely plausible. Ordinary conversation, questions, explanations, and mild reactions stay neutral. An exclamation mark alone is not enough.
- Never label a line just because its topic is emotional - judge the delivery, not the subject.
- Each number must be copied exactly from the number printed before that line - never shift it to a neighboring line.`)
	return b.String()
}()

// emotionLineRe matches one reply line: "<idx>: <label>", tolerating
// surrounding whitespace, a trailing period, and extra words after the
// label (only the first word is taken).
var emotionLineRe = regexp.MustCompile(`^(\d+)\s*:\s*([A-Za-z]+)`)

// parseEmotionLines scans content for "<idx>: <label>" lines, keeping only
// those whose idx is one of dialogue's own (never a narration line or an
// index outside this batch) and whose label is a known emotion - anything
// else is dropped line by line, never failing the whole batch. An empty
// reply is the ordinary "everything's neutral" answer, not an error.
func parseEmotionLines(content string, dialogue map[int]bool) map[int]string {
	out := make(map[int]string)
	for _, line := range strings.Split(content, "\n") {
		m := emotionLineRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		idx, err := strconv.Atoi(m[1])
		if err != nil || !dialogue[idx] {
			continue
		}
		label := strings.ToLower(m[2])
		if !emotions.Valid(label) {
			continue
		}
		out[idx] = label
	}
	return out
}

func (c *Client) emotionBatch(ctx context.Context, bookTitle, chapterTitle string, batch []ParagraphInput) (map[int]string, error) {
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nChapter: %s\n", bookTitle, chapterTitle)
	user.WriteString("\nLines:\n")
	dialogue := make(map[int]bool)
	for i, p := range batch {
		if i > 0 && !p.Inline {
			user.WriteString("\n")
		}
		if p.IsQuote {
			dialogue[p.Idx] = true
			fmt.Fprintf(&user, "%d (dialogue): %s\n", p.Idx, oneLine(p.Text))
		} else {
			fmt.Fprintf(&user, "%d: %s\n", p.Idx, oneLine(p.Text))
		}
	}
	if len(dialogue) == 0 {
		return nil, nil
	}
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}
	return generateAndParse(ctx, c, emotionSystemPrompt, user.String(), 0, emotionMaxTokens, func(content string) (map[int]string, error) {
		return parseEmotionLines(content, dialogue), nil
	})
}

// EmotionChapter labels chapterTitle's dialogue paragraphs with an
// internal/emotions id each, batching in groups of emotionBatchParagraphs.
// paragraphs should be the whole chapter (or a paused run's remaining
// slice) in order, narration included: only IsQuote entries can be
// labeled, but narration is shown to the model as context. Returns a map
// from Idx to emotion id for every labeled line - a dialogue line absent
// from it is neutral.
//
// shouldPause/remaining/err follow AttributeChapter's own contract exactly
// (see its doc comment): nil-safe shouldPause, checked between batches
// only; remaining holds whatever wasn't reached (by a pause or a genuine
// failure) so the caller can persist real progress and requeue just the
// rest.
func (c *Client) EmotionChapter(ctx context.Context, bookTitle, chapterTitle string, paragraphs []ParagraphInput, shouldPause func() bool) (out map[int]string, remaining []ParagraphInput, err error) {
	out = make(map[int]string)
	for start := 0; start < len(paragraphs); start += emotionBatchParagraphs {
		if start > 0 && shouldPause != nil && shouldPause() {
			remaining = paragraphs[start:]
			break
		}
		end := min(start+emotionBatchParagraphs, len(paragraphs))
		batch := paragraphs[start:end]
		labels, err := c.emotionBatch(ctx, bookTitle, chapterTitle, batch)
		if err != nil {
			return out, paragraphs[start:], fmt.Errorf("paragraphs %d-%d: %w", batch[0].Idx, batch[len(batch)-1].Idx, err)
		}
		for idx, e := range labels {
			out[idx] = e
		}
	}
	return out, remaining, nil
}
