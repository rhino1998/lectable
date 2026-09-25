package httpapi

import (
	"net/http"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/store"
)

func TestCharacterAliasesEndpointAndMerge(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{{
		Title: "One",
		Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Hi,”", IsQuote: true},
			{Kind: store.BlockText, Text: "Albert said.", Inline: true},
			{Kind: store.BlockText, Text: "“Bye,”", IsQuote: true},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	bert, _, _ := s.UpsertCharacter(scope, "Bert", false)
	sid, _, _ := s.UpsertCharacter(scope, "Sid", false)
	albert, _, _ := s.UpsertCharacter(scope, "Albert", false)
	base := ts.URL + "/api/books/" + bookID

	put := func(charID string, aliases ...string) int {
		resp := doJSON(t, http.MethodPut, base+"/characters/"+charID+"/aliases", map[string]any{"aliases": aliases}, nil)
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := put(sid.ID, "Siddy", "Scarf Hangman"); code != http.StatusOK {
		t.Fatalf("set aliases: %d", code)
	}
	if code := put(sid.ID, "Bert and Sid"); code != http.StatusBadRequest {
		t.Fatalf("group alias: want 400, got %d", code)
	}
	if code := put(sid.ID, "bert"); code != http.StatusConflict {
		t.Fatalf("another character's name as alias: want 409, got %d", code)
	}

	// Merging Albert into Bert keeps "Albert" as one of Bert's names.
	resp := doJSON(t, http.MethodPost, base+"/characters/"+albert.ID+"/merge", map[string]string{"targetName": "Bert"}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("merge: %d", resp.StatusCode)
	}
	if got, _ := s.GetCharacter(bert.ID); got == nil || len(got.Aliases) != 1 || got.Aliases[0] != "Albert" {
		t.Fatalf("merge didn't record alias: %+v", got)
	}

	// A manual speaker edit typing an alias lands on the real character;
	// a group is refused.
	resp = doJSON(t, http.MethodPut, base+"/chapters/0/paragraphs/0/speaker", map[string]string{"speaker": "albert"}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set speaker by alias: %d", resp.StatusCode)
	}
	ch, _ := s.GetChapterByIdx(bookID, 0)
	ps, _ := s.ListParagraphsRaw(ch.ID)
	if ps[0].Speaker != "Bert" {
		t.Fatalf("alias speaker stored as %q, want Bert", ps[0].Speaker)
	}
	resp = doJSON(t, http.MethodPut, base+"/chapters/0/paragraphs/2/speaker", map[string]string{"speaker": "Bert and Sid"}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("group speaker: want 400, got %d", resp.StatusCode)
	}
	if c, _ := s.GetCharacterByName(scope, "Bert and Sid"); c != nil {
		t.Fatalf("group speaker was registered as a character")
	}
}
