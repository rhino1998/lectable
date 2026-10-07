package speakerattr

import (
	"context"
	"strings"
	"testing"
)

func TestParseRecap(t *testing.T) {
	body := strings.Repeat("Mocar sailed on through the storm with the crew. ", 6)
	cases := map[string]string{
		"prose":   body,
		"json":    `{"recap": "` + strings.TrimSpace(body) + `"}`,
		"opener":  "Previously, " + strings.ToLower(body[:1]) + body[1:],
		"think":   "<think>hm</think>\n" + body,
		"heading": "**" + strings.TrimSpace(body) + "**",
	}
	want := strings.TrimSpace(body)
	for name, in := range cases {
		got, err := parseRecap(in)
		if err != nil || got != want {
			t.Errorf("%s: parseRecap = %q, %v; want %q", name, got, err, want)
		}
	}
	if got, err := parseRecap("One. \n\n Two paragraphs here, " + body); err != nil || !strings.HasPrefix(got, "One.\n\nTwo paragraphs") {
		t.Errorf("paragraphs = %q, %v", got, err)
	}
	if _, err := parseRecap("I can't help with that."); err == nil {
		t.Error("a fragment was taken as a recap")
	}
}

// A book's notes are split into about equal groups, none over the limit.
func TestRecapGroups(t *testing.T) {
	var chapters []RecapChapter
	for range 50 {
		chapters = append(chapters, RecapChapter{Title: "Chapter", Summary: strings.Repeat("x", 600)})
	}
	groups := recapGroups(chapters) // ~30.5k of notes
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	if a, b := len(groups[0]), len(groups[1]); a+b != 50 || a-b > 6 || b-a > 6 {
		t.Fatalf("group sizes %d and %d, want about equal", a, b)
	}
	if got := recapGroups(chapters[:10]); len(got) != 1 {
		t.Fatalf("a short book split into %d groups", len(got))
	}
}

// recapLLM answers each recap prompt by kind, recording what it was asked.
type recapLLM struct {
	calls []string // "recap", "series", "digest", "shorten"
	users []string
	long  bool
}

func (l *recapLLM) LLMGenerate(_ context.Context, sys, user string, _ float32, _ int) (string, error) {
	kind := "recap"
	switch {
	case strings.Contains(sys, "shortening a recap"):
		kind = "shorten"
	case strings.Contains(sys, "condensing notes"):
		kind = "digest"
	case strings.Contains(sys, "the book before it"):
		kind = "series"
	}
	l.calls = append(l.calls, kind)
	l.users = append(l.users, user)
	n := 30
	if l.long && kind != "shorten" {
		n = 400
	}
	return strings.TrimSpace(strings.Repeat(kind+" ", n)) + ".", nil
}

func TestRecapPrompt(t *testing.T) {
	llm := &recapLLM{}
	c := NewClient(Config{}, llm)
	got, err := c.Recap(context.Background(), RecapInput{
		BookTitle:    "The Book",
		Chapters:     []RecapChapter{{Title: "One", Summary: "Mocar sails."}},
		CurrentTitle: "Two",
		CurrentParts: []string{"Mocar lands."},
		CurrentText:  []ParagraphInput{{Idx: 40, Text: "He walked inland."}, {Idx: 41, Text: "“Onward,”", IsQuote: true}, {Idx: 42, Text: "he said.", Inline: true}},
	}, nil)
	if err != nil || !strings.HasPrefix(got, "recap recap") {
		t.Fatalf("Recap = %q, %v", got, err)
	}
	user := llm.users[0]
	for _, want := range []string{"- One: Mocar sails.", "in the middle of (Two)", "- earlier in it: Mocar lands.", "He walked inland.\n“Onward,” he said.", "The listener stopped here.", recapAsk} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt lacks %q:\n%s", want, user)
		}
	}
	// A reader at a chapter's start has no current chapter.
	if _, err := c.Recap(context.Background(), RecapInput{BookTitle: "The Book", Chapters: []RecapChapter{{Title: "One", Summary: "Mocar sails."}}}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(llm.users[1], "stopped at the end of the last chapter above") {
		t.Errorf("prompt for a chapter's start:\n%s", llm.users[1])
	}
}

// A long book's notes are condensed group by group, and a recap that runs
// long is shortened.
func TestSeriesRecapCondensesAndShortens(t *testing.T) {
	var chapters []RecapChapter
	for range 50 {
		chapters = append(chapters, RecapChapter{Title: "Chapter", Summary: strings.Repeat("x", 600)})
	}
	llm := &recapLLM{long: true}
	got, err := NewClient(Config{}, llm).SeriesRecap(context.Background(), "Book One", "Book Two", chapters)
	if err != nil {
		t.Fatal(err)
	}
	if want := "digest digest series shorten"; strings.Join(llm.calls, " ") != want {
		t.Fatalf("calls = %v, want %s", llm.calls, want)
	}
	if !strings.HasPrefix(got, "shorten shorten") {
		t.Fatalf("recap = %.40q, want the shortened one", got)
	}
	if !strings.Contains(llm.users[2], "- Part 1 of 2: digest") || !strings.Contains(llm.users[2], seriesRecapAsk) {
		t.Errorf("series prompt:\n%.300s", llm.users[2])
	}
}
