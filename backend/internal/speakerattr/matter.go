package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// matterMaxTokens bounds the matter call's JSON: one short entry per
// front or back matter section, of which a book has a dozen or two.
const matterMaxTokens = 1500

// matterMaxSections is the most sections shown whole; a longer book is
// shown its first and last matterEdgeSections, the only places front and
// back matter is (the longest epub in this library has 100 sections; a
// slot's context holds a few hundred of these lines).
const (
	matterMaxSections  = 160
	matterEdgeSections = 60
)

// matterOpeningChars is how much of a section's text its line shows.
const matterOpeningChars = 90

// matterEndPageChars is the length under which a section titled "The End"
// is that page and nothing more.
const matterEndPageChars = 60

// matterSystemPrompt is the front/back matter pass: one call per book over
// its section list, naming the sections that aren't story (a title page,
// the copyright page, "About the Author") so narration, the other passes
// and the recap leave them out.
//
// Measured on this library's 86 epubs (707 matter sections, 64 synopses of
// earlier books, 3629 story chapters), each shape with FindMatter's guard:
// titles alone left 22 title pages in, each named for the book or its
// series ("Expeditionary Force", "Citadel") and so read as a named
// chapter; titles and lengths kept 24 synopses for being long and skipped
// chapters whose title is an epub id ("cA10", opening "Chapter 84: Home
// Visit"); with the opening text too, 703 of 707. Answering with numbers
// alone, the model once named the section before the one it meant (a
// play's last scene for the licence after it); asked for each title beside
// its number it didn't, in any book. A fixed list of titles
// (httpapi's notStory) matched 223 of the 707: the rest is each
// publisher's own ("Don't Miss Out", "Join the Conversation", "LitRPG!").
//
// Deliberately not in SystemPrompts: one call per book isn't worth a
// permanently resident primed sequence.
const matterSystemPrompt = `You sort the sections of an ebook into the book's own story and everything else.

You are given every section of one book in reading order, one per line: its number, its title, its length in characters, and how its text begins.

Front and back matter is every section that is not this book's own story: a title page, copyright page, table of contents, dedication, epigraph, acknowledgements, an author's note or thanks, about the author or publisher, requests to review, subscribe, or follow on a website, Patreon or social media, adverts, blurbs and previews for other books, lists of other titles, a glossary or appendix, a "the end" page, and a synopsis or summary of earlier books in the series.

The story is everything a listener of the audiobook would want read to them: prologues, epilogues, interludes, numbered or named chapters, part headings between chapters, and bonus or after-credits scenes of this book. A title that is a character's name or a phrase is usually a chapter.

Front and back matter sits before the first chapter and after the last one. A section with story chapters on both sides of it is story. When you cannot tell, it is story.

Answer with JSON only, no other text: {"skip": [{"n": the number of a section, "title": "its title, copied exactly"}, ...]} - one entry for each section that is front or back matter.`

// MatterSection is one chapter as the matter pass sees it.
type MatterSection struct {
	Title string
	// Chars is the length of the section's text.
	Chars int
	// Opening is how the section's text begins (MatterOpening).
	Opening string
}

// MatterOpening is the start of a section's text for its MatterSection:
// its first paragraphs on one line, without the one that only repeats the
// title.
func MatterOpening(title string, paragraphs []string) string {
	var parts []string
	n := 0
	for _, p := range paragraphs {
		p = strings.Join(strings.Fields(p), " ")
		if p == "" || strings.EqualFold(p, strings.TrimSpace(title)) {
			continue
		}
		parts = append(parts, p)
		if n += len(p); n >= matterOpeningChars {
			break
		}
	}
	return truncateRunes(strings.Join(parts, " / "), matterOpeningChars)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// matterChapterTitle matches a title that says it is story: a numbered
// chapter, a prologue, an epilogue. FindMatter never skips one, nor
// anything between two.
var matterChapterTitle = regexp.MustCompile(`(?i)^\W*(chapter\b|prologue|epilogue|\d+\b)`)

// matterEndTitle matches "The End", "End of Book Three".
var matterEndTitle = regexp.MustCompile(`(?i)^\W*(the )?end\b`)

// FindMatter reports which of a book's sections are front or back matter,
// one bool per section. The model's answer is held to where matter can be
// (matterGuard), so a wrong one costs a title page read aloud or a bonus
// scene to un-skip, never a chapter out of the middle of the book.
func (c *Client) FindMatter(ctx context.Context, sections []MatterSection) ([]bool, error) {
	if len(sections) == 0 {
		return nil, nil
	}
	shown := matterShown(len(sections))
	named, err := generateAndParse(ctx, c, matterSystemPrompt, c.matterUserPrompt(sections, shown), 0, matterMaxTokens, func(content string) ([]bool, error) {
		return parseMatter(content, sections)
	})
	if err != nil {
		return nil, err
	}
	return matterGuard(sections, named), nil
}

// matterShown is which sections the prompt lists: all of a book of up to
// matterMaxSections, else the first and last matterEdgeSections.
func matterShown(n int) func(i int) bool {
	if n <= matterMaxSections {
		return func(int) bool { return true }
	}
	return func(i int) bool { return i < matterEdgeSections || i >= n-matterEdgeSections }
}

func (c *Client) matterUserPrompt(sections []MatterSection, shown func(int) bool) string {
	var user strings.Builder
	for i, s := range sections {
		if !shown(i) {
			if shown(i - 1) {
				fmt.Fprintf(&user, "(sections %d to %d, left out here, are chapters)\n", i, len(sections)-matterEdgeSections-1)
			}
			continue
		}
		fmt.Fprintf(&user, "%d. %s [%d chars]", i, truncateRunes(strings.Join(strings.Fields(s.Title), " "), 80), s.Chars)
		if s.Opening != "" {
			fmt.Fprintf(&user, " - %s", s.Opening)
		}
		user.WriteByte('\n')
	}
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}
	return user.String()
}

// matterEchoedRest is what the model sometimes copies after a title: the
// rest of the section's line ("Thanks [1985 chars] - Hi everyone...").
var matterEchoedRest = regexp.MustCompile(`(?s)\s*\[\d+ chars\].*$`)

// matterTitleKey is a title reduced to what a copy of it keeps.
func matterTitleKey(title string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			sb.WriteRune(r)
		}
	}
	return truncateRunes(sb.String(), 40)
}

// parseMatter reads the model's answer into one bool per section. An
// entry whose title isn't its numbered section's is dropped: the number
// is off, and the section it names stays story.
func parseMatter(content string, sections []MatterSection) ([]bool, error) {
	value, found := firstJSONValue(content, '{')
	if !found {
		return nil, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
	}
	var raw struct {
		Skip *[]struct {
			N     *int   `json:"n"`
			Title string `json:"title"`
		} `json:"skip"`
	}
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, fmt.Errorf("parse JSON object: %w", err)
	}
	if raw.Skip == nil {
		return nil, fmt.Errorf(`no "skip" list in response: %s`, truncate(content, 200))
	}
	out := make([]bool, len(sections))
	for _, e := range *raw.Skip {
		if e.N == nil || *e.N < 0 || *e.N >= len(sections) {
			continue
		}
		if matterTitleKey(matterEchoedRest.ReplaceAllString(e.Title, "")) != matterTitleKey(sections[*e.N].Title) {
			continue
		}
		out[*e.N] = true
	}
	return out, nil
}

// matterGuard holds the model's answer (named, one bool per section) to
// where front and back matter can be, and adds the "The End" pages it
// leaves in about one book in twenty.
//
// In a book with numbered chapters (85 of this library's 86 epubs) matter
// is before the first of them or after the last, and is never one of
// them: unguarded, the model skipped two epilogues, a "Chapter 45", three
// very short numbered chapters and 14 character sheets printed between
// chapters. A book with none (a play: acts and scenes) has only the
// model's own answer to go by, so there matter is the run of sections it
// named from the start and the run it named to the end - nothing with
// story on both sides.
func matterGuard(sections []MatterSection, named []bool) []bool {
	n := len(sections)
	out := make([]bool, n)
	first, last := -1, -1
	for i, s := range sections {
		if matterChapterTitle.MatchString(s.Title) {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		for i := 0; i < n && named[i]; i++ {
			out[i] = true
		}
		for i := n - 1; i >= 0 && named[i]; i-- {
			out[i] = true
		}
		return out
	}
	for i, s := range sections {
		switch {
		case i >= first && i <= last:
		case named[i]:
			out[i] = true
		case i > last && s.Chars < matterEndPageChars && matterEndTitle.MatchString(s.Title):
			out[i] = true
		}
	}
	return out
}
