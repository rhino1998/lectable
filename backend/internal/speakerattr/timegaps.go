package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Finding where time passes in a story, for time jumps (a book's "N years
// pass before this chapter", which ages its cast's voices). Nothing here
// makes a jump: what is found is proposed for the reader to accept, move
// or skip.
//
// A gap is rarely announced where it falls. Will's five years in The
// Legend of William Oh pass between an "Epilogue" and the next chapter
// with no word of it; what gives it away is the narration after - "Over
// the last few years, his brain had gradually adjusted" a chapter later,
// "the last five years or so" fifteen chapters on. So the pass reads for
// statements that time HAS passed, anywhere in the series, and places a
// jump where a run of them begins:
//
//  1. Candidates: paragraphs with a phrase of elapsed time (TimePhrase -
//     the vocabulary is small, so an exact search finds them all: 28 of
//     William Oh's 59,000 paragraphs, 372 of The Wandering Inn's 670,000)
//     and the closest by meaning to TimeGapQueries, for the wordings a
//     pattern misses.
//  2. FindTimeGaps reads them in batches and keeps the ones about story
//     time gone by, each with a length and its sentence - not backstory,
//     ages, plans or durations.
//  3. The caller groups what is left into runs of nearby chapters
//     (httpapi.timeGapRuns), and PlaceTimeJump is asked of each, with the
//     chapter summaries around its start, whether the story skipped that
//     time and before which chapter.
//
// Measured 2026-10-11 on William Oh (one jump set by hand, 5 years before
// chapter 201): 77 candidates, 19 statements, the run starting at chapter
// 202 placed as "5 years before chapter 202" - one chapter late, which is
// why the reader can move a proposal before accepting it. Scattered
// statements before it (backstory, "nearly a year gone by" of time the
// chapters told) were turned down by step 3. A second run 100 chapters
// later, still looking back over the same years, was proposed as its own
// 3-year jump until runs explained by an earlier jump were dropped.

// TimeGapQueries are the phrases the series is searched for by meaning.
var TimeGapQueries = []string{
	"years passed; it had been several years since then",
	"over the last few years, in the years since",
	"three years later; many months had gone by; a decade later",
}

const (
	timeAmount = `(?:a|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|fifteen|twenty|thirty|forty|fifty|hundred|few|couple(?: of)?|several|many|\d+|half a|dozen)`
	timeUnit   = `(?:years?|months|decades?|seasons|winters|summers)`
)

var timePhrase = regexp.MustCompile(`(?i)\b(?:` +
	timeAmount + `(?: \w+)? ` + timeUnit + ` (?:later|had passed|passed|went by|had gone by|gone by|since|on)\b` +
	`|(?:over|in|during|for|within) the (?:last|past|intervening) (?:` + timeAmount + ` )?` + timeUnit +
	`|(?:the|these) (?:last|past) (?:` + timeAmount + ` )?` + timeUnit +
	`|in the ` + timeUnit + ` since|` + timeUnit + ` (?:had|have) (?:passed|gone by)` +
	`|it(?:'|’)?s been (?:\w+ )?` + timeAmount + `(?: \w+)? ` + timeUnit +
	`|it had been (?:\w+ )?` + timeAmount + `(?: \w+)? ` + timeUnit +
	`)`)

// TimePhrase reports whether text has a phrase of elapsed time ("three
// years later", "over the last few years", "it had been two years") -
// what makes a paragraph a candidate for FindTimeGaps.
func TimePhrase(text string) bool {
	return timePhrase.MatchString(text)
}

// TimeGap is one statement that story time has gone by.
type TimeGap struct {
	// Passage indexes the passages FindTimeGaps was given.
	Passage int
	// Years is how long, by the passage - "a few years" 3, "several" 5.
	Years float64
	// Skip is whether the narration itself jumps ahead here ("Three years
	// later, ...") rather than looking back over time already gone.
	Skip bool
	// Evidence is the sentence, as the passage has it.
	Evidence string
}

const timeGapSystemPrompt = `You read passages from a novel and pick out the ones that show a long stretch of story time has gone by - a season or more that the story skipped over rather than told. A reader will review what you propose; it is used to make the characters' voices older.

It counts when the passage says time has passed in the story's present: "Three years later", "over the last few years", "it had been two years since the siege", "in the months since". Say how long in years (0.5 for half a year or "months"; a vague "a few years" is 3, "several years" is 5, "a decade" 10). It does not count when the time is backstory from before the story began ("he had left home ten years ago", "twenty years of service"), someone's age, a plan or prediction, how long something lasts or will take, a period shorter than three months, or a flashback. Most passages give nothing: an empty list is a normal answer.

"how" is "skip" when the narration itself jumps ahead at this point ("Three years later, ..."), and "lookback" when the passage looks back over time that has already gone by ("over the last few years").

Reply with a single JSON object and nothing else:
{"gaps": [{"n": <the passage's number>, "how": "skip" or "lookback", "years": <number>, "evidence": "<the one sentence it rests on, copied word for word>"}]}`

const timeGapMaxTokens = 1500

// FindTimeGaps reads passages (FactPassage lines) and returns the
// statements among them that story time has gone by. A gap whose quote is
// not in its passage is dropped.
func (c *Client) FindTimeGaps(ctx context.Context, passages []string) ([]TimeGap, error) {
	if len(passages) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	sb.WriteString("Passages:\n")
	for i, p := range passages {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, p)
	}
	return retryParse(func(attempt int) (string, error) {
		return c.generate(ctx, timeGapSystemPrompt, sb.String(), float32(attempt)*retryTempBump, timeGapMaxTokens)
	}, func(content string) ([]TimeGap, error) {
		entries, err := jsonList(content, "gaps")
		if err != nil {
			return nil, err
		}
		var out []TimeGap
		for _, e := range entries {
			n, years := intField(e, "n")-1, numberField(e, "years")
			evidence := trimQuote(textField(e, "evidence"))
			if n < 0 || n >= len(passages) || years <= 0 || evidence == "" || !strings.Contains(passages[n], evidence) {
				continue
			}
			out = append(out, TimeGap{Passage: n, Years: years, Skip: textField(e, "how") == "skip", Evidence: evidence})
		}
		return out, nil
	})
}

// jsonList parses a reply of the form {"<key>": [{...}, ...]} into its
// entries; a reply without the key has none.
func jsonList(content, key string) ([]map[string]any, error) {
	value, found := firstJSONValue(content, '{')
	if !found {
		return nil, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil, fmt.Errorf("parse JSON object: %w", err)
	}
	var entries []map[string]any
	if raw, ok := result[key]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("parse %q: %w", key, err)
		}
	}
	return entries, nil
}

// numberField is m[key] as a number - given as one or as a string of
// digits - 0 when it is neither.
func numberField(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%g", &f); err == nil {
			return f
		}
	}
	return 0
}

func intField(m map[string]any, key string) int {
	return int(math.Round(numberField(m, key)))
}

// GapStatement is one TimeGap as PlaceTimeJump is shown it: its sentence
// and the chapter it is in, counted along the series.
type GapStatement struct {
	Chapter int
	Text    string
}

// ChapterNote is one chapter around a run's start, for PlaceTimeJump.
type ChapterNote struct {
	Chapter int
	Title   string
	Summary string
}

// PlacedJump is PlaceTimeJump's answer. Jump is false when the statements
// don't show a skipped stretch of time.
type PlacedJump struct {
	Jump bool
	// Before is the chapter the skipped time falls before, numbered as the
	// statements and chapters were.
	Before int
	Years  int
}

const placeTimeJumpSystemPrompt = `You place a time jump in a novel, for the reader of its audiobook to review. Passages from a run of chapters each say that a long stretch of story time has gone by; chapter summaries around where they begin are given. Decide whether the story really skipped over that time - rather than telling it chapter by chapter - and if so before which chapter the skipped time falls and how many years it is.

The skipped time falls before the first chapter whose events come after it: usually the chapter of the earliest passage or the one before it; an epilogue or interlude often sits just before a jump. Take the years from the passages, preferring an exact figure ("five years") to a vague one ("a few years"). There is no jump when the passages are about different stretches of time, about time before the story began, or about time the chapters themselves told.

Reply with a single JSON object and nothing else:
{"jump": true or false, "before_chapter": <chapter number>, "years": <whole number>, "why": "<one sentence>"}`

// PlaceTimeJump decides whether a run of statements shows a stretch of
// time the story skipped, and before which chapter. chapters are the ones
// around the run's start; an answer naming a chapter outside
// [chapters[0].Chapter, last statement's chapter] is no jump.
func (c *Client) PlaceTimeJump(ctx context.Context, statements []GapStatement, chapters []ChapterNote) (PlacedJump, error) {
	if len(statements) == 0 || len(chapters) == 0 {
		return PlacedJump{}, nil
	}
	var sb strings.Builder
	sb.WriteString("Passages:\n")
	for _, s := range statements {
		fmt.Fprintf(&sb, "- [chapter %d] %s\n", s.Chapter, s.Text)
	}
	sb.WriteString("\nChapter summaries:\n")
	for _, ch := range chapters {
		fmt.Fprintf(&sb, "- chapter %d, %q: %s\n", ch.Chapter, oneLine(ch.Title), truncate(oneLine(ch.Summary), 400))
	}
	first, last := chapters[0].Chapter, statements[len(statements)-1].Chapter
	return retryParse(func(attempt int) (string, error) {
		return c.generate(ctx, placeTimeJumpSystemPrompt, sb.String(), float32(attempt)*retryTempBump, 300)
	}, func(content string) (PlacedJump, error) {
		value, found := firstJSONValue(content, '{')
		if !found {
			return PlacedJump{}, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(value), &result); err != nil {
			return PlacedJump{}, fmt.Errorf("parse JSON object: %w", err)
		}
		jump, _ := result["jump"].(bool)
		before, years := intField(result, "before_chapter"), intField(result, "years")
		if !jump || years < 1 || before < first || before > last {
			return PlacedJump{}, nil
		}
		return PlacedJump{Jump: true, Before: before, Years: years}, nil
	})
}
