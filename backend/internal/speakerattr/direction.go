package speakerattr

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"

	"github.com/rhino1998/lectable/backend/internal/direction"
	"github.com/rhino1998/lectable/backend/internal/emotions"
)

// The direction pass's second stage: free-form speech direction for a
// clone model that takes it as text (voices.DeliveryDirected, Fish Drama
// 3). The first stage (EmotionChapter) picks which lines are emotional at
// all; this writes how each of those - and each line a delivery verb
// beside it points at (DeliveryCue) - is performed. Asking for a direction
// on every line would direct every line: the label measurements in
// docs/speakerattr.md found a generated "only the emotional ones" list
// labelling a third of all dialogue. See backend/docs/drama3-direction-plan.md.

// directionConcurrency is how many lines DirectLines has in flight at
// once - each is its own small call, batched worker-side
// (llamacpp.Scheduler) with the others.
const directionConcurrency = 4

// directionStyleWords is the style length the prompt asks for - under
// direction.MaxStyleWords, which Parse enforces.
const directionStyleWords = 10

// directionTemp is the first attempt's temperature: 0.4 gave every line a
// distinct style (174 of 174) where greedy repeated a few (169-170).
const directionTemp = 0.4

// directionMaxTokens bounds one line's reply: the line itself plus a
// short style and a few tags, plus room for a thinking trace.
const directionMaxTokens = 2048

// directionSystemPrompt is the stage's one fixed prompt (primed - see
// SystemPrompts). Built from direction's tag lists so what the model is
// offered is what Parse keeps. Tuned 2026-10-15 with cmd/directprobe on
// 174 labeled or cued lines from 7 books (Qwen3.6-35B-A3B): a few worked
// examples beat rules alone (rules-only prompts reused their own example
// phrases up to 10 times in 174 lines, and pasted the label hint's
// description in as a style); asking for audible qualities only removed
// gestures ("leaning in"); 97-100% of styles came back distinct, words
// never changed. Telling it to avoid "sharp" did not stop "sharp" (~30
// lines). See "Free-form direction" in docs/speakerattr.md.
var directionSystemPrompt = buildDirectionSystemPrompt()

func buildDirectionSystemPrompt() string {
	var points, spans []string
	for _, t := range direction.PointTags {
		points = append(points, "["+t+"]")
	}
	for _, t := range direction.SpanTags {
		spans = append(spans, "<"+t+">...</"+t+">")
	}
	return fmt.Sprintf(`You are a voice director for an audiobook read by an expressive text-to-speech model. You are given one line of dialogue from a novel, the passage around it, and who says it. Write the line back exactly as it is, with direction added for how it is performed.

- A style: start with a short phrase in square brackets, at most %d words, describing how the voice sounds on this line - loudness, pace, breath, texture, and the feeling under it. Only what can be heard: no gestures, faces or eyes ("leaning in", "rolling her eyes", "grinning" can't be heard - write "a smile in the voice" instead). Read the passage for what the speaker actually feels at this moment - anger that is held back, amusement that is a cover, fear under bravado - and say that. Fresh words for each line: avoid stock phrases like "sharp", "cutting", "cutting through the noise", "dripping with".
- Point tags: put a sound the speaker makes where it happens, only from: %s. Only when the passage shows it. A point tag is never part of the style: write [weary] [sighs] Well..., not [sighs, weary] Well...
- Scoped tags: wrap a word or a few words in %s - emphasis where the speaker leans on them (often what the text puts in italics or capitals), whisper for words said under the breath, slow for words drawn out.

Change nothing else: every word stays exactly as written, in order.

Examples.

Passage: He slammed the ledger shut. >> "You lied to me. Every single day, you lied." << His knuckles were white on the desk.
Reply: [low and shaking, a shout held behind the teeth] "You lied to me. Every single day, you <emphasis>lied</emphasis>."

Passage: She laughed and wiped her eyes. >> "Oh, you didn't. Fine. Fine, I'll clean it up." <<
Reply: [fond and exasperated, a smile in the voice] [laughs] "Oh, you didn't. [sighs] Fine. Fine, I'll clean it up."

Passage: The guard glanced at the door, then leaned in. >> "Not here. Meet me at the well after dark." <<
Reply: [hushed and urgent, quick] "Not here. <whisper>Meet me at the well after dark.</whisper>"

Passage: The old knight lowered himself onto the bench. >> "I was young once. Younger than you." << He did not look at her.
Reply: [quiet, rough with age, far away] "I was young once. <emphasis>Younger</emphasis> than you."

Reply with ONLY the line, nothing before or after it.`, directionStyleWords, strings.Join(points, " "), strings.Join(spans, ", "))
}

// DirectionLine is one dialogue line DirectLines writes direction for,
// with what the prompt shows about it.
type DirectionLine struct {
	Idx  int
	Text string
	// Speaker is who says it ("" when unknown) and Profile their casting
	// summary (store.Character.Summary), so a line is directed in its
	// speaker's own manner.
	Speaker string
	Profile string
	// Label is the first stage's emotion id for it, "" for none - a hint,
	// not a constraint.
	Label string
	// Before and After are the paragraphs around it, in order, as the
	// prompt shows them ("Name: text" for attributed dialogue).
	Before []string
	After  []string
	// Scene is where and when it is said (the summary pass's scene for
	// it), "" when unknown.
	Scene string
}

// DirectionContext is what DirectLines shows about the whole chapter.
type DirectionContext struct {
	BookTitle    string
	ChapterTitle string
	// Setting is the book's setting profile, "" when it has none.
	Setting string
}

// DirectLines writes a direction for each line, one call per line,
// directionConcurrency at a time. A line whose reply never keeps its
// words (direction.ErrTextChanged, after generateAndParse's retries) gets
// no direction rather than failing the run; out holds an entry - the zero
// Direction for "spoken plainly" - for every line answered. err is the
// first failure that wasn't the reply's fault (a cancelled context, the
// model down); lines not answered by then are absent from out.
func (c *Client) DirectLines(ctx context.Context, dc DirectionContext, lines []DirectionLine) (out map[int]direction.Direction, err error) {
	out = make(map[int]direction.Direction, len(lines))
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
		sem      = make(chan struct{}, directionConcurrency)
	)
	for _, line := range lines {
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			d, err := c.directLine(ctx, dc, line, defaultDirectionOptions, nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				out[line.Idx] = d
			case errors.Is(err, direction.ErrTextChanged):
				log.Printf("speakerattr: direction for paragraph %d never kept its words; leaving it undirected", line.Idx)
				out[line.Idx] = direction.Direction{}
			case firstErr == nil:
				firstErr = fmt.Errorf("paragraph %d: %w", line.Idx, err)
			}
		}()
	}
	wg.Wait()
	return out, firstErr
}

// DirectionOptions are what a prompt-tuning run (cmd/directprobe) varies
// in DirectLinesProbe; DirectLines always uses the defaults.
type DirectionOptions struct {
	// SystemPrompt replaces directionSystemPrompt ("" keeps it).
	SystemPrompt string
	// Temp is the first attempt's temperature.
	Temp float32
	// LabelHint shows the first stage's label in the user prompt.
	LabelHint bool
}

var defaultDirectionOptions = DirectionOptions{Temp: directionTemp, LabelHint: true}

// DirectionProbeResult is one line's answer from DirectLinesProbe: every
// raw reply in order (a retry adds one), what was kept, and the error.
type DirectionProbeResult struct {
	Replies   []string            `json:"replies"`
	Direction direction.Direction `json:"direction"`
	Err       string              `json:"error,omitempty"`
}

// DirectLinesProbe is DirectLines under opts, keeping every reply - for
// tuning the prompt offline.
func (c *Client) DirectLinesProbe(ctx context.Context, opts DirectionOptions, dc DirectionContext, lines []DirectionLine) map[int]DirectionProbeResult {
	out := make(map[int]DirectionProbeResult, len(lines))
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, directionConcurrency)
	)
	for _, line := range lines {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			var replies []string
			d, err := c.directLine(ctx, dc, line, opts, &replies)
			r := DirectionProbeResult{Replies: replies, Direction: d}
			if err != nil {
				r.Err = err.Error()
			}
			mu.Lock()
			out[line.Idx] = r
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// directLine is one DirectLines call; replies, when set, collects each raw
// reply.
func (c *Client) directLine(ctx context.Context, dc DirectionContext, line DirectionLine, opts DirectionOptions, replies *[]string) (direction.Direction, error) {
	system := opts.SystemPrompt
	if system == "" {
		system = directionSystemPrompt
	}
	if !opts.LabelHint {
		line.Label = ""
	}
	user := c.directionUserPrompt(dc, line)
	attempt := 0
	d, err := generateAndParse(ctx, c, system, user, opts.Temp, directionMaxTokens, func(content string) (direction.Direction, error) {
		if replies != nil {
			*replies = append(*replies, content)
		}
		attempt++
		d, err := direction.Parse(line.Text, cleanDirectionReply(content))
		if err == nil && attempt <= bareStyleRetries && bareStyle(d.Style) {
			return d, errBareStyle
		}
		return d, err
	})
	if errors.Is(err, direction.ErrTextChanged) {
		// retryParse's "giving up" wraps the last parse error.
		return direction.Direction{}, direction.ErrTextChanged
	}
	return d, err
}

// bareStyleRetries is how many replies with a one-word style ("[weary]")
// directLine sends back for a phrase before taking one - the label hint
// pulls the model toward echoing a label, and a phrase for the moment is
// what Drama 3's direction is for.
const bareStyleRetries = 1

var errBareStyle = errors.New("style is a single word, wanted a phrase")

// bareStyle reports whether style is one word.
func bareStyle(style string) bool {
	return style != "" && len(strings.FieldsFunc(style, func(r rune) bool { return r == ' ' || r == ',' })) < 2
}

// directionUserPrompt is one line's user turn.
func (c *Client) directionUserPrompt(dc DirectionContext, line DirectionLine) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Book: %s\nChapter: %s\n", dc.BookTitle, dc.ChapterTitle)
	if dc.Setting != "" {
		fmt.Fprintf(&b, "Setting: %s\n", oneLine(dc.Setting))
	}
	if line.Scene != "" {
		fmt.Fprintf(&b, "Scene: %s\n", oneLine(line.Scene))
	}
	speaker := line.Speaker
	if speaker == "" {
		speaker = "unknown"
	}
	fmt.Fprintf(&b, "\nSpeaker: %s", speaker)
	if line.Profile != "" {
		fmt.Fprintf(&b, " - %s", oneLine(line.Profile))
	}
	b.WriteString("\n")
	if e, ok := emotions.Get(line.Label); ok {
		fmt.Fprintf(&b, "A first pass tagged this line %q - a rough hint that may be wrong; write your own phrase from the passage\n", e.ID)
	}
	b.WriteString("\nPassage:\n")
	for _, p := range line.Before {
		fmt.Fprintf(&b, "%s\n", oneLine(p))
	}
	fmt.Fprintf(&b, ">> %s <<\n", oneLine(line.Text))
	for _, p := range line.After {
		fmt.Fprintf(&b, "%s\n", oneLine(p))
	}
	fmt.Fprintf(&b, "\nThe line (between >> <<), to write back with its direction:\n%s\n", oneLine(line.Text))
	if c.cfg.NoThink {
		b.WriteString("\n/no_think")
	}
	return b.String()
}

// cleanDirectionReply takes the line out of whatever a model wraps it in:
// a code fence, a "Line:" label, the >> << markers, surrounding quotes
// added around a whole bracketed reply.
func cleanDirectionReply(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```text")
	s = strings.Trim(s, "`")
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		// One line was asked for; a preamble is dropped, the line kept.
		s = strings.TrimSpace(s[i+1:])
	}
	s = strings.TrimPrefix(s, "Line:")
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), ">>"), "<<"))
	return s
}

// deliveryCuePattern matches the narration around a line that says how it
// is delivered or what sound comes with it - "she laughed", "he sighed",
// "his voice breaking" - which can earn a line direction (a point tag,
// a scoped whisper) without its being one of the emotion labels.
var deliveryCuePattern = regexp.MustCompile(`(?i)\b(` +
	`laugh(?:s|ed|ing)?|chuckl(?:es|ed|ing)|giggl(?:es|ed|ing)|snicker(?:s|ed|ing)?|` +
	`sigh(?:s|ed|ing)?|sob(?:s|bed|bing)?|sniff(?:s|ed|ing|led|les)?|gasp(?:s|ed|ing)?|groan(?:s|ed|ing)?|cough(?:s|ed|ing)?|` +
	`whisper(?:s|ed|ing)?|murmur(?:s|ed|ing)?|mutter(?:s|ed|ing)?|hiss(?:es|ed|ing)?|breath(?:ed|less|lessly)|` +
	`shout(?:s|ed|ing)?|yell(?:s|ed|ing)?|scream(?:s|ed|ing)?|snap(?:s|ped|ping)?|growl(?:s|ed|ing)?|` +
	`stammer(?:s|ed|ing)?|stutter(?:s|ed|ing)?|hesitat(?:es|ed|ing)|` +
	`voice (?:break|broke|breaking|crack|cracked|cracking|trembl\w*|shak\w*|thick|low|hoarse)|` +
	`through (?:gritted )?teeth|under (?:his|her|their) breath|drawl(?:s|ed|ing)?` +
	`)\b`)

// DeliveryCue reports whether text - narration beside a dialogue line -
// says how the line is delivered.
func DeliveryCue(text string) bool {
	return deliveryCuePattern.MatchString(text)
}

// DirectionSystemPrompt is the stage's default system prompt, for a tuning
// run to start from.
func DirectionSystemPrompt() string { return directionSystemPrompt }

// DirectionUserPrompt is line's user turn as DirectLines sends it.
func (c *Client) DirectionUserPrompt(dc DirectionContext, line DirectionLine) string {
	return c.directionUserPrompt(dc, line)
}
