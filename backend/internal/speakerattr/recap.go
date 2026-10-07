package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// recapMaxTokens bounds one recap (or digest) reply - a couple of short
// paragraphs.
const recapMaxTokens = 1024

// recapNotesChars caps the chapter notes one recap or digest call is shown
// (~6k tokens, summaryWindowChars' size). A book's worth of notes beyond
// that is condensed group by group first - see SeriesRecap.
const recapNotesChars = 24000

// recapAsk closes a recap's user prompt: the rules the model otherwise
// drops once it has read several thousand words of the current chapter -
// without it, it retold that chapter's text in the present tense, loot
// lists included, and never mentioned the chapters before.
const recapAsk = `Now write the recap: past tense throughout; first a sentence or two on the bigger picture from the earlier chapters, then the recent events, ending where the listener stopped; 90 to 140 words; no loot, levels or numbers.`

// seriesRecapAsk closes a series recap's user prompt - without it the
// model handed back the digests it was given, joined, at twice the length.
const seriesRecapAsk = `Now write the recap in your own words: at most 12 sentences, in two or three paragraphs, past tense throughout. It must be far shorter than the notes above - leave most of what they say out; copying them is wrong. Spend the last paragraph on how the book ended.`

// recapSystemPrompt is the "previously on" recap a listener hears when
// they come back to a book after a break (Recap). It is read aloud by the
// book's narrator right after a spoken "Previously, in <title>.", so it is
// plain prose and never opens with that itself.
const recapSystemPrompt = `You write the "previously on" recap a listener hears before an audiobook picks up again after a break. You will be given notes on the chapters they have already heard, oldest first, and possibly the text of the part of the current chapter they have heard so far. Everything you are given has already been heard; nothing else has.

Reply with the recap only - no title, no preamble, no notes after it.

- 90 to 140 words of plain prose that will be read aloud by the narrator, in the past tense and the third person (a first-person narrator is named, never "I").
- Help the listener pick the story back up. Open with the bigger picture in a sentence or two, drawn from the earlier chapters: what the main characters are in the middle of, where, and what they are trying to do. Then tell what has happened most recently and what is still unresolved, ending on the moment the listener stopped at - the last thing shown to you.
- Keep to what moves the story. Leave out loot, equipment, levels, numbers and other detail a listener doesn't need to follow what comes next.
- Skip anything that isn't this story: an author's note, a preview of another book, a publisher's advert, a list of links.
- Tell it as one continuous story, not chapter by chapter. Never mention chapters, notes, the book, the author or the listener.
- Only what you were given: no guesses about what happens next, no opinions, no teasers or questions.
- No lists, headings, quotation marks, stat blocks or game notifications, and no opening like "Previously" - it is spoken right after one.`

// seriesRecapSystemPrompt is the recap of a whole earlier book, heard at
// the start of the next one in its series (SeriesRecap).
const seriesRecapSystemPrompt = `You write the recap a listener hears at the start of an audiobook, telling them what happened in the book before it in the series. You will be given notes on that earlier book, in order, start to finish.

Reply with the recap only - no title, no preamble, no notes after it.

- 160 to 230 words of plain prose that will be read aloud by the narrator, in the past tense and the third person (a first-person narrator is named, never "I").
- Tell the earlier book's story from start to finish: who the main characters are, what they set out to do, the turning points, and above all how it ended - where everyone was left, what was won or lost, and what was still unresolved. Give the ending the most room.
- Leave out minor characters and side errands. Skip anything that isn't story (a title page, a dedication, an author's note, an advert for another book).
- Tell it as one continuous story, not chapter by chapter. Never mention chapters, notes, the author or the listener.
- Only what the notes state: no guesses about what comes next, no opinions, no teasers or questions.
- No lists, headings, quotation marks, stat blocks or game notifications, and no opening like "Previously" - it is spoken right after one.`

// recapDigestSystemPrompt condenses one stretch of a long book's chapter
// notes, so a whole book fits one SeriesRecap call.
const recapDigestSystemPrompt = `You are condensing notes on one stretch of a novel, given chapter by chapter in order, into a shorter account of that stretch.

Reply with the recap only - no title, no preamble, no notes after it.

- At most 8 sentences of plain prose in the past tense and the third person, telling what happened across the stretch in order: who did what, where, what changed, and how things stood at its end. Name the characters involved.
- Keep what later events could depend on - deaths, alliances, betrayals, discoveries, things gained or lost, plans made. Drop side errands. Skip anything that isn't story (a title page, an author's note).
- Only what the notes state.`

// recapShortenSystemPrompt cuts a recap that came out too long down to
// size (Client.shortenRecap). Asked for 160-230 words of a whole book, the
// model wrote 300-570 - at worst its digests handed back joined - but it
// does shorten a text it is given: 373 words came back as 169.
const recapShortenSystemPrompt = `You are shortening a recap that a listener hears before an audiobook continues, telling them what has happened so far. You will be given the recap as written; it is too long and lists too many events.

Reply with the shorter recap only - no title, no preamble, no notes after it.

- Plain prose in the past tense and the third person, in two paragraphs.
- Keep who the main characters are, what they set out to do, the few turning points everything else hangs on, and how things stood at the end - where everyone was left and what was still unresolved. The end gets the second paragraph.
- Cut side errands, minor characters, loot, levels, numbers, and anything a listener could follow what comes next without.
- Use one name for each character throughout.
- Add nothing that isn't in the recap you were given.`

// Word limits past which a recap is shortened, and the sentence count it
// is shortened to: a position recap runs about a minute spoken, a series
// recap a minute and a half.
const (
	recapMaxWords        = 190
	recapSentences       = 7
	seriesRecapMaxWords  = 260
	seriesRecapSentences = 9
)

// shortenRecap returns text cut down to at most sentences sentences when
// it runs over maxWords, text itself when it doesn't - or when the shorter
// version fails or isn't shorter, since a long recap beats none.
func (c *Client) shortenRecap(ctx context.Context, text string, maxWords, sentences int) string {
	words := len(strings.Fields(text))
	if words <= maxWords {
		return text
	}
	user := fmt.Sprintf("The recap as written:\n\n%s\n\nNow write the shorter recap: at most %d sentences.\n", text, sentences)
	if c.cfg.NoThink {
		user += "\n/no_think"
	}
	short, err := generateAndParse(ctx, c, recapShortenSystemPrompt, user, 0, recapMaxTokens, parseRecap)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("speakerattr: shortening a %d-word recap: %v", words, err)
		}
		return text
	}
	if len(strings.Fields(short)) >= words {
		return text
	}
	return short
}

// RecapChapter is one already-heard chapter's notes, as a recap is shown
// them.
type RecapChapter struct {
	Title   string
	Summary string
}

// RecapInput is what a position recap is written from - everything in it
// is before the reader's position.
type RecapInput struct {
	BookTitle string
	// Chapters are the chapters before the current one, oldest first.
	Chapters []RecapChapter
	// CurrentTitle is the chapter the reader is in; CurrentParts the
	// summaries of the windows of it they have finished, and CurrentText
	// the paragraphs after those, up to where they stopped. All empty for
	// a reader at a chapter's start.
	CurrentTitle string
	CurrentParts []string
	CurrentText  []ParagraphInput
	// Truncated says the start of the current chapter's heard part is
	// missing from CurrentParts/CurrentText (a long chapter whose notes
	// predate ChapterSummary.Parts).
	Truncated bool
}

// Recap writes the "previously on" recap for a reader coming back to a
// book at the position in describes - see recapSystemPrompt.
func (c *Client) Recap(ctx context.Context, in RecapInput, setting *BookSetting) (string, error) {
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\n", in.BookTitle)
	if lines := setting.promptLines(); lines != "" {
		fmt.Fprintf(&user, "\nBook setting notes:\n%s", lines)
	}
	if len(in.Chapters) > 0 {
		user.WriteString("\nNotes on the chapters already heard, oldest first:\n")
		writeRecapChapters(&user, in.Chapters)
	}
	if len(in.CurrentParts) > 0 || len(in.CurrentText) > 0 {
		fmt.Fprintf(&user, "\nThe chapter the listener is in the middle of (%s):\n", in.CurrentTitle)
		if in.Truncated {
			user.WriteString("(its opening isn't shown)\n")
		}
		for _, p := range in.CurrentParts {
			fmt.Fprintf(&user, "- earlier in it: %s\n", oneLine(p))
		}
		if len(in.CurrentText) > 0 {
			user.WriteString("\nIts text up to where they stopped:\n")
			for i, p := range in.CurrentText {
				if p.Inline && i > 0 {
					fmt.Fprintf(&user, " %s", oneLine(p.Text))
					continue
				}
				if i > 0 {
					user.WriteString("\n")
				}
				user.WriteString(oneLine(p.Text))
			}
			user.WriteString("\n\nThe listener stopped here.\n")
		}
	} else {
		user.WriteString("\nThe listener stopped at the end of the last chapter above.\n")
	}
	fmt.Fprintf(&user, "\n%s\n", recapAsk)
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}
	text, err := generateAndParse(ctx, c, recapSystemPrompt, user.String(), 0, recapMaxTokens, parseRecap)
	if err != nil {
		return "", err
	}
	return c.shortenRecap(ctx, text, recapMaxWords, recapSentences), nil
}

// SeriesRecap writes the recap of the whole of bookTitle from its chapters'
// notes, for a reader starting nextTitle, the book after it - see
// seriesRecapSystemPrompt. Notes longer than one call takes are condensed
// first, a recapNotesChars group at a time (recapDigestSystemPrompt), and
// the recap written from those digests - then shortened if it ran long
// (shortenRecap), which it usually does.
func (c *Client) SeriesRecap(ctx context.Context, bookTitle, nextTitle string, chapters []RecapChapter) (string, error) {
	groups := recapGroups(chapters)
	for len(groups) > 1 {
		digests := make([]RecapChapter, 0, len(groups))
		for i, g := range groups {
			var user strings.Builder
			fmt.Fprintf(&user, "Book: %s\nPart %d of %d of the book.\n\nNotes, chapter by chapter:\n", bookTitle, i+1, len(groups))
			writeRecapChapters(&user, g)
			if c.cfg.NoThink {
				user.WriteString("\n/no_think")
			}
			digest, err := generateAndParse(ctx, c, recapDigestSystemPrompt, user.String(), 0, recapMaxTokens, parseRecap)
			if err != nil {
				return "", fmt.Errorf("condense part %d of %d: %w", i+1, len(groups), err)
			}
			digests = append(digests, RecapChapter{Title: fmt.Sprintf("Part %d of %d", i+1, len(groups)), Summary: digest})
		}
		groups = recapGroups(digests)
	}
	var user strings.Builder
	fmt.Fprintf(&user, "The earlier book: %s\nThe book the listener is about to start: %s\n\nNotes on the earlier book, start to finish:\n", bookTitle, nextTitle)
	if len(groups) == 1 {
		writeRecapChapters(&user, groups[0])
	}
	fmt.Fprintf(&user, "\n%s\n", seriesRecapAsk)
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}
	text, err := generateAndParse(ctx, c, seriesRecapSystemPrompt, user.String(), 0, recapMaxTokens, parseRecap)
	if err != nil {
		return "", err
	}
	return c.shortenRecap(ctx, text, seriesRecapMaxWords, seriesRecapSentences), nil
}

func writeRecapChapters(sb *strings.Builder, chapters []RecapChapter) {
	for _, ch := range chapters {
		fmt.Fprintf(sb, "- %s: %s\n", oneLine(ch.Title), oneLine(ch.Summary))
	}
}

// recapGroups splits chapters, in order, into the fewest runs of at most
// recapNotesChars of notes each (a single longer one stands alone), about
// equal in size - a short last run would get as long a digest as the rest
// and so as much of the recap.
func recapGroups(chapters []RecapChapter) [][]RecapChapter {
	total := 0
	for _, ch := range chapters {
		total += recapNoteSize(ch)
	}
	target := recapNotesChars
	if n := (total + recapNotesChars - 1) / recapNotesChars; n > 1 {
		target = min(recapNotesChars, total/n+total/(n*10))
	}
	var out [][]RecapChapter
	start, size := 0, 0
	for i, ch := range chapters {
		n := recapNoteSize(ch)
		if size > 0 && size+n > target {
			out = append(out, chapters[start:i])
			start, size = i, 0
		}
		size += n
	}
	if start < len(chapters) {
		out = append(out, chapters[start:])
	}
	return out
}

func recapNoteSize(ch RecapChapter) int { return len(ch.Title) + len(ch.Summary) + 4 }

// parseRecap reads a recap reply - prose, or the same wrapped in a
// {"recap": ...} object - tidied for reading aloud: one line per
// paragraph, no markdown, no "Previously..." opener (it is spoken right
// after one).
func parseRecap(content string) (string, error) {
	var raw struct {
		Recap string `json:"recap"`
	}
	if i := strings.LastIndex(content, "</think>"); i >= 0 {
		content = content[i+len("</think>"):]
	}
	raw.Recap = content
	if strings.HasPrefix(strings.TrimSpace(content), "{") {
		value, _ := firstJSONValue(content, '{')
		if err := json.Unmarshal([]byte(value), &raw); err != nil {
			return "", fmt.Errorf("decode recap: %w", err)
		}
	}
	var paragraphs []string
	for _, p := range strings.Split(raw.Recap, "\n") {
		p = strings.NewReplacer("*", "", "#", "", "_", "").Replace(p)
		if p = oneLine(p); p != "" {
			paragraphs = append(paragraphs, p)
		}
	}
	if len(paragraphs) > 0 {
		paragraphs[0] = trimRecapOpener(paragraphs[0])
	}
	text := strings.Join(paragraphs, "\n\n")
	if len(strings.Fields(text)) < recapMinWords {
		return "", fmt.Errorf("recap too short (%d words)", len(strings.Fields(text)))
	}
	return text, nil
}

// recapMinWords is the shortest reply taken as a recap - under it the
// model answered with a refusal or a fragment.
const recapMinWords = 25

// trimRecapOpener drops a leading "Previously," / "Previously on <title>:"
// the model added despite the prompt, recapitalizing what follows.
func trimRecapOpener(s string) string {
	lower := strings.ToLower(s)
	if !strings.HasPrefix(lower, "previously") {
		return s
	}
	cut := strings.IndexAny(s, ",:")
	if cut < 0 || cut > 80 {
		return s
	}
	rest := strings.TrimSpace(s[cut+1:])
	if rest == "" {
		return s
	}
	return strings.ToUpper(rest[:1]) + rest[1:]
}
