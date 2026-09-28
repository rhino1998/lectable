package httpapi

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// summaryLLM answers the summary pass with a fixed cast and the
// attribution pass with the cast's reference for every asked line,
// recording the attribution prompts.
type summaryLLM struct {
	mu      sync.Mutex
	attribs []string
}

func (l *summaryLLM) LLMGenerate(_ context.Context, sys, user string, _ float32, _ int) (string, error) {
	switch {
	case strings.Contains(sys, "notes on one chapter"):
		return `{"summary": "Mocar sails.", "scenes": [{"start": 0, "location": "a ship's deck", "time": "dawn", "present": ["Mocar"]}], "cast": [{"name": "Mocar", "role": false, "pronoun": "he", "refs": ["the old sailor", "his hat"]}]}`, nil
	case strings.Contains(sys, "identify who speaks"):
		l.mu.Lock()
		l.attribs = append(l.attribs, user)
		l.mu.Unlock()
		var sb strings.Builder
		for _, line := range strings.Split(user, "\n") {
			if i := strings.Index(line, " (who?):"); i > 0 {
				fmt.Fprintf(&sb, "%s: the old sailor\n", line[:i])
			}
		}
		return sb.String(), nil
	}
	return "", nil
}

// TestSummaryFeedsAttribution: the summary pass stores the chapter's
// notes (a reference not in the text, or a possessive, dropped), and
// attribution then lists the cast with its reference and maps an answer
// naming the reference to the person.
func TestSummaryFeedsAttribution(t *testing.T) {
	srv, s, _, _ := newTestServer(t)
	llm := &summaryLLM{}
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, llm)
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{{
		Title: "One",
		Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "The old sailor squinted at the dawn and tugged his hat."},
			{Kind: store.BlockText, Text: "“Weigh anchor,”", IsQuote: true},
			{Kind: store.BlockText, Text: "he growled.", Inline: true},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	ch, _ := s.GetChapterByIdx(bookID, 0)

	if _, _, err := srv.summarizeChapter(context.Background(), book, ch); err != nil {
		t.Fatal(err)
	}
	notes, err := s.GetChapterNotes(ch.ID)
	if err != nil || notes == nil || notes.Summary != "Mocar sails." || len(notes.Scenes) != 1 || notes.Scenes[0].Location != "a ship's deck" {
		t.Fatalf("notes = %+v, %v", notes, err)
	}
	if len(notes.Cast) != 1 || notes.Cast[0].Pronouns != speakerattr.PronounsHe || len(notes.Cast[0].Refs) != 1 || notes.Cast[0].Refs[0] != "the old sailor" {
		t.Fatalf("cast = %+v, want Mocar with just \"the old sailor\"", notes.Cast)
	}
	ch, _ = s.GetChapterByID(ch.ID)
	if !ch.Passes.Summary {
		t.Fatal("passes.summary not set")
	}

	if _, _, err := srv.attributeChapter(context.Background(), book, ch, false); err != nil {
		t.Fatal(err)
	}
	if len(llm.attribs) == 0 || !strings.Contains(llm.attribs[0], "Mocar (he/him; in this chapter called the old sailor)") {
		t.Fatalf("attribution prompt didn't list the cast:\n%v", llm.attribs)
	}
	ps, _ := s.ListParagraphsRaw(ch.ID)
	if ps[1].Speaker != "Mocar" {
		t.Fatalf("speaker = %q, want Mocar (the reference resolved)", ps[1].Speaker)
	}
}
