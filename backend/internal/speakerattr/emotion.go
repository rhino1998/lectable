package speakerattr

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
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

// emotionSystemPrompt is the pass's prompt for a backend with no decide
// call (emotionBatch): a generated list of only the emotional lines. Like
// emotionDenseSystemPrompt it is built from internal/emotions so the labels the
// model is offered are always exactly the set the rest of the app knows
// how to render.
var emotionSystemPrompt = buildEmotionSystemPrompt(false)

// emotionDenseSystemPrompt is emotionSystemPrompt asking for a verdict on
// every dialogue line, "neutral" included, instead of a list of only the
// emotional ones - the reply shape a decide call needs, and what the pass
// runs on (emotionBatch).
var emotionDenseSystemPrompt = buildEmotionSystemPrompt(true)

// emotionNeutralLabel is the dense prompt's word for no emotion.
const emotionNeutralLabel = "neutral"

func buildEmotionSystemPrompt(dense bool) string {
	var b strings.Builder
	if dense {
		b.WriteString(`You are a voice director for an audiobook. Given numbered lines from a novel - a mix of narration and dialogue, where dialogue lines are marked "(dialogue)" - decide for each dialogue line whether it is clearly spoken with one of the emotions or speaking modes below, so the character's voice can be performed that way, or in a normal, neutral way.

Reply with ONLY plain text lines, no JSON, no code fence, no commentary: one line per dialogue line, in order, made of that line's number, a colon and a space, then exactly one label from the list below. For example:

12: angry
13: neutral
15: whisper

Most dialogue is spoken in a normal, neutral way - expect most of your lines to say neutral.

Labels (copy one verbatim):
- neutral: an ordinary speaking voice - no clear, strong emotion
`)
	} else {
		b.WriteString(`You are a voice director for an audiobook. Given numbered lines from a novel - a mix of narration and dialogue, where dialogue lines are marked "(dialogue)" - decide which dialogue lines are clearly spoken with one of the emotions or speaking modes below, so the character's voice can be performed that way.

Reply with ONLY plain text lines, no JSON, no code fence, no commentary: one line per emotional dialogue line, made of that line's number, a colon and a space, then exactly one label from the list below. For example:

12: angry
15: whisper

Omit every other line. Most dialogue is spoken in a normal, neutral way and must be omitted - expect this list to be short. If no line qualifies, reply with nothing at all.

Labels (copy one verbatim):
`)
	}
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
}

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

// emotionUserPrompt is one batch's user turn and its dialogue lines'
// numbers, in order.
func (c *Client) emotionUserPrompt(bookTitle, chapterTitle string, batch []ParagraphInput) (string, []int) {
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nChapter: %s\n", bookTitle, chapterTitle)
	user.WriteString("\nLines:\n")
	var dialogue []int
	for i, p := range batch {
		if i > 0 && !p.Inline {
			user.WriteString("\n")
		}
		if p.IsQuote {
			dialogue = append(dialogue, p.Idx)
			fmt.Fprintf(&user, "%d (dialogue): %s\n", p.Idx, oneLine(p.Text))
		} else {
			fmt.Fprintf(&user, "%d: %s\n", p.Idx, oneLine(p.Text))
		}
	}
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}
	return user.String(), dialogue
}

// emotionBatch labels one batch. With a backend that can decide, every
// dialogue line gets a verdict - neutral or one emotion - read from the
// model's logits (emotionDenseSystemPrompt, emotionSteps); a line is
// labeled when an emotion outscores neutral. Asking about every line,
// neutral included, labels about half as many lines as asking for a list
// of the emotional ones did, and the ones it drops were mostly wrong - see
// "The emotion pass" in docs/speakerattr.md. A backend that can't decide,
// or whose tokenizer starts two labels with the same token, generates that
// list instead (emotionSystemPrompt).
func (c *Client) emotionBatch(ctx context.Context, bookTitle, chapterTitle string, batch []ParagraphInput) (map[int]string, error) {
	decider, ok := c.llm.(llmDecider)
	if !ok {
		return c.emotionGenerate(ctx, emotionSystemPrompt, bookTitle, chapterTitle, batch)
	}
	user, asked := c.emotionUserPrompt(bookTitle, chapterTitle, batch)
	if len(asked) == 0 {
		return nil, nil
	}
	decisions, err := decider.LLMDecide(ctx, emotionDenseSystemPrompt, user, emotionSteps(asked))
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		log.Printf("speakerattr: emotion decide call failed, generating instead: %v", err)
		return c.emotionGenerate(ctx, emotionSystemPrompt, bookTitle, chapterTitle, batch)
	}
	if len(decisions) != len(asked) {
		return nil, fmt.Errorf("%d decisions for %d dialogue lines", len(decisions), len(asked))
	}
	out := make(map[int]string)
	for i, d := range decisions {
		if d.Choice > 0 && d.Choice <= len(emotions.All) {
			out[asked[i]] = emotions.All[d.Choice-1].ID
		}
	}
	return out, nil
}

// emotionGenerate asks systemPrompt about batch and parses the labels out
// of its generated reply.
func (c *Client) emotionGenerate(ctx context.Context, systemPrompt, bookTitle, chapterTitle string, batch []ParagraphInput) (map[int]string, error) {
	user, asked := c.emotionUserPrompt(bookTitle, chapterTitle, batch)
	if len(asked) == 0 {
		return nil, nil
	}
	dialogue := make(map[int]bool, len(asked))
	for _, idx := range asked {
		dialogue[idx] = true
	}
	return generateAndParse(ctx, c, systemPrompt, user, 0, emotionMaxTokens, func(content string) (map[int]string, error) {
		return parseEmotionLines(content, dialogue), nil
	})
}

// emotionSteps is the dense prompt's reply for the dialogue lines asked,
// as decide steps: "N:" written for the model, then its choice among
// neutral (option 0) and the emotions, in emotions.All order.
func emotionSteps(asked []int) []ttsproto.LLMDecideStep {
	options := []string{" " + emotionNeutralLabel}
	for _, e := range emotions.All {
		options = append(options, " "+e.ID)
	}
	steps := make([]ttsproto.LLMDecideStep, len(asked))
	for i, idx := range asked {
		prefix := fmt.Sprintf("%d:", idx)
		if i > 0 {
			prefix = "\n" + prefix
		}
		steps[i] = ttsproto.LLMDecideStep{Prefix: prefix, Options: options}
	}
	return steps
}

// EmotionVerdict is one dialogue line's answer from EmotionProbe.
type EmotionVerdict struct {
	// Label is the emotion id, "" for neutral.
	Label string `json:"label"`
	// Neutral is the model's probability of neutral, and Top its
	// probability of the likeliest emotion, TopLabel (decide mode only;
	// uncalibrated).
	Neutral  float64 `json:"neutral,omitempty"`
	Top      float64 `json:"top,omitempty"`
	TopLabel string  `json:"topLabel,omitempty"`
	Mass     float64 `json:"mass,omitempty"`
}

// EmotionProbe is EmotionChapter for benchmarking how the model answers:
//
//   - "gen": the pass as it ran before deciding (and still runs on a
//     backend that can't decide) - a generated list of the emotional lines.
//   - "gen-dense": emotionDenseSystemPrompt, a generated verdict for every
//     dialogue line.
//   - "decide": what the app runs - the dense prompt, each verdict read
//     from the model's logits over neutral and the emotion labels.
func (c *Client) EmotionProbe(ctx context.Context, bookTitle, chapterTitle string, paragraphs []ParagraphInput, mode string) (map[int]EmotionVerdict, error) {
	out := map[int]EmotionVerdict{}
	for start := 0; start < len(paragraphs); start += emotionBatchParagraphs {
		batch := paragraphs[start:min(start+emotionBatchParagraphs, len(paragraphs))]
		user, asked := c.emotionUserPrompt(bookTitle, chapterTitle, batch)
		if len(asked) == 0 {
			continue
		}
		switch mode {
		case "gen", "gen-dense":
			prompt := emotionSystemPrompt
			if mode == "gen-dense" {
				prompt = emotionDenseSystemPrompt
			}
			labels, err := c.emotionGenerate(ctx, prompt, bookTitle, chapterTitle, batch)
			if err != nil {
				return out, err
			}
			for _, idx := range asked {
				out[idx] = EmotionVerdict{Label: labels[idx]}
			}
		case "decide":
			decider, ok := c.llm.(llmDecider)
			if !ok {
				return out, fmt.Errorf("speakerattr: this LLM backend has no decide call")
			}
			decisions, err := decider.LLMDecide(ctx, emotionDenseSystemPrompt, user, emotionSteps(asked))
			if err != nil {
				return out, err
			}
			for i, d := range decisions {
				v := EmotionVerdict{Neutral: d.Probs[0], Mass: d.Mass}
				if d.Choice > 0 {
					v.Label = emotions.All[d.Choice-1].ID
				}
				for k, p := range d.Probs[1:] {
					if p > v.Top {
						v.Top, v.TopLabel = p, emotions.All[k].ID
					}
				}
				out[asked[i]] = v
			}
		default:
			return out, fmt.Errorf("speakerattr: unknown probe mode %q", mode)
		}
	}
	return out, nil
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
