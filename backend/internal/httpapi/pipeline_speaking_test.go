package httpapi

import (
	"testing"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// Preprocessing characterizes only the characters who speak in the book:
// not one who is only ever named, one from another book of the series,
// or an invalid one.
func TestSpeakingCharacters(t *testing.T) {
	srv, s, _, _ := newTestServer(t)
	chapter := []store.ChapterInput{{Title: "One", Blocks: []store.BlockInput{
		{Kind: store.BlockText, Text: "Mocar spoke of Ilsa, who was not there."},
		{Kind: store.BlockText, Text: "“Weigh anchor,”", IsQuote: true},
		{Kind: store.BlockText, Text: "“Gone,”", IsQuote: true},
	}}}
	bookID, _, err := s.CreateBook("Book Two", "A", "en", "", "Saga", 2, chapter)
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	ch, _ := s.GetChapterByIdx(bookID, 0)
	if err := s.SetParagraphSpeakers(ch.ID, map[int]string{1: "Mocar", 2: "Ghost"}); err != nil {
		t.Fatal(err)
	}
	scope := store.SeriesScope(book)
	var roster []store.Character
	for _, name := range []string{"Mocar", "Ilsa", "Earlier", "Ghost"} {
		c, _, err := s.UpsertCharacter(scope, name, false)
		if err != nil {
			t.Fatal(err)
		}
		c.Invalid = name == "Ghost"
		roster = append(roster, c)
	}
	got := srv.speakingCharacters(book, roster)
	if len(got) != 1 || got[0].Name != "Mocar" {
		t.Fatalf("speaking characters = %+v, want just Mocar", got)
	}
}
