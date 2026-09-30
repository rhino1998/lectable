package httpapi

import (
	"net/http"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// TestRenameCharacter covers PUT .../characters/{id}/name: lines, speaker
// corrections, summary notes and the setting's narrator follow the new
// name, the old one becomes an alias, and clashes are refused.
func TestRenameCharacter(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{{
		Title: "One",
		Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Hi,”", IsQuote: true},
			{Kind: store.BlockText, Text: "“Bye,”", IsQuote: true},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	jamey, _, _ := s.UpsertCharacter(scope, "Jamey", false)
	sid, _, _ := s.UpsertCharacter(scope, "Sid", false)
	_ = s.SetCharacterAliases(sid.ID, []string{"Siddy"})
	ch, _ := s.GetChapterByIdx(bookID, 0)
	ps, _ := s.ListParagraphsRaw(ch.ID)
	if err := s.SetParagraphSpeakers(ch.ID, map[int]string{0: "Jamey", 1: "Sid"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordParagraphEdit(ps[0].ID, store.EditSpeaker, "Sid", "Jamey"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChapterNotes(ch.ID, store.ChapterNotes{
		Summary: "Talk.",
		Scenes:  []store.SummaryScene{{StartIdx: 0, Present: []string{"Jamey", "Sid"}}},
		Cast:    []store.CastMember{{Name: "jamey"}, {Name: "Sid"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBookSetting(bookID, store.BookSetting{Narrator: "Jamey"}, store.BookSettingAuto); err != nil {
		t.Fatal(err)
	}

	rename := func(charID, name string) int {
		resp := doJSON(t, http.MethodPut, ts.URL+"/api/books/"+bookID+"/characters/"+charID+"/name", map[string]string{"name": name}, nil)
		resp.Body.Close()
		return resp.StatusCode
	}
	for name, want := range map[string]int{"Sid": http.StatusConflict, "siddy": http.StatusConflict, "Narrator": http.StatusBadRequest, "Steve and Sid": http.StatusBadRequest, "  ": http.StatusBadRequest} {
		if got := rename(jamey.ID, name); got != want {
			t.Errorf("rename to %q: got %d, want %d", name, got, want)
		}
	}
	if got := rename(jamey.ID, "Steve  Atherton"); got != http.StatusNoContent {
		t.Fatalf("rename: %d", got)
	}

	got, _ := s.GetCharacter(jamey.ID)
	if got.Name != "Steve Atherton" || len(got.Aliases) != 1 || got.Aliases[0] != "Jamey" {
		t.Fatalf("character = %+v, want Steve Atherton aka Jamey", got)
	}
	ps, _ = s.ListParagraphsRaw(ch.ID)
	if ps[0].Speaker != "Steve Atherton" || ps[1].Speaker != "Sid" {
		t.Fatalf("speakers = %q, %q", ps[0].Speaker, ps[1].Speaker)
	}
	edits, _ := s.ParagraphEdits(bookID, store.EditSpeaker)
	if len(edits) != 1 || edits[0].Value != "Steve Atherton" || edits[0].Original != "Sid" {
		t.Fatalf("edits = %+v", edits)
	}
	notes, _ := s.GetChapterNotes(ch.ID)
	if notes.Cast[0].Name != "Steve Atherton" || notes.Scenes[0].Present[0] != "Steve Atherton" || notes.Cast[1].Name != "Sid" {
		t.Fatalf("notes = %+v", notes)
	}
	if st, _ := s.GetBookSetting(bookID); st == nil || st.Setting.Narrator != "Steve Atherton" {
		t.Fatalf("setting = %+v", st)
	}

	// Renaming to one of its own aliases promotes it: the alias goes, and
	// the previous name takes its place.
	if got := rename(jamey.ID, "Jamey"); got != http.StatusNoContent {
		t.Fatalf("rename back: %d", got)
	}
	got, _ = s.GetCharacter(jamey.ID)
	if got.Name != "Jamey" || len(got.Aliases) != 1 || got.Aliases[0] != "Steve Atherton" {
		t.Fatalf("character = %+v, want Jamey aka Steve Atherton", got)
	}
}
