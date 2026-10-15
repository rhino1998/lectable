package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// Retag moves stored speakers to what the tag rules say - a named tag and
// an action beat here - but leaves a hand-corrected line and a chapter
// that was never attributed alone.
func TestRetagSpeakers(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{
		{Title: "One", Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Sixth floor,”", IsQuote: true},
			{Kind: store.BlockText, Text: "Mira said.", Inline: true},
			{Kind: store.BlockText, Text: "“Let’s get it done.”", IsQuote: true},
			{Kind: store.BlockText, Text: "Tomas nodded.", Inline: true},
			{Kind: store.BlockText, Text: "“Fine,”", IsQuote: true},
			{Kind: store.BlockText, Text: "Mira said.", Inline: true},
		}},
		{Title: "Two", Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Hi,”", IsQuote: true},
			{Kind: store.BlockText, Text: "Mira said.", Inline: true},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	for _, n := range []string{"Mira", "Tomas"} {
		if _, _, err := s.UpsertCharacter(store.SeriesScope(book), n, false); err != nil {
			t.Fatal(err)
		}
	}
	ch0, _ := s.GetChapterByIdx(bookID, 0)
	ch1, _ := s.GetChapterByIdx(bookID, 1)
	if err := s.SetParagraphSpeakers(ch0.ID, map[int]string{0: "Tomas", 2: "Mira", 4: "Tomas"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChapterAttributed(ch0.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphSpeakers(ch1.ID, map[int]string{0: "Tomas"}); err != nil {
		t.Fatal(err)
	}

	var out retagResponse
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/speakers/retag", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Chapters != 1 || out.Changed != 3 || out.Tag != 2 || out.Beat != 1 {
		t.Fatalf("response = %+v", out)
	}
	ps0, _ := s.ListParagraphsRaw(ch0.ID)
	ps1, _ := s.ListParagraphsRaw(ch1.ID)
	if got := []string{ps0[0].Speaker, ps0[2].Speaker, ps0[4].Speaker, ps1[0].Speaker}; got[0] != "Mira" || got[1] != "Tomas" || got[2] != "Mira" || got[3] != "Tomas" {
		t.Fatalf("speakers = %v, want Mira (tag), Tomas (beat), Mira (tag), Tomas (unattributed chapter, kept)", got)
	}

	// Once a reader corrects a line in chapter 0, the chapter is part of
	// a reviewed stretch: nothing in it moves again, whatever its tags say.
	if err := s.SetParagraphSpeakers(ch0.ID, map[int]string{2: "Mira"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordParagraphEdit(mustParagraphID(t, s, ch0.ID, 4), store.EditSpeaker, "Mira", "Tomas"); err != nil {
		t.Fatal(err)
	}
	resp2 := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/speakers/retag", nil, nil)
	defer resp2.Body.Close()
	var out2 retagResponse
	if err := json.NewDecoder(resp2.Body).Decode(&out2); err != nil {
		t.Fatal(err)
	}
	if out2.Reviewed != 1 || out2.Changed != 0 {
		t.Fatalf("second run = %+v, want chapter 0 left alone as reviewed", out2)
	}
	if ps, _ := s.ListParagraphsRaw(ch0.ID); ps[2].Speaker != "Mira" {
		t.Fatalf("reviewed chapter changed: %q", ps[2].Speaker)
	}
}

func mustParagraphID(t *testing.T, s store.Store, chapterID string, idx int) string {
	t.Helper()
	id, err := s.GetParagraphIDByIdx(chapterID, idx)
	if err != nil || id == "" {
		t.Fatalf("paragraph %d: %v", idx, err)
	}
	return id
}

func TestNameWithin(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"Torina", "Torina Milon", true},
		{"Petyr Groshenko", "Groshenko", true},
		{"Abel Perry Tudor", "Clive Van Able Tudor II", false},
		{"Mira", "Tomas", false},
		{"Mira", "", false},
	} {
		if got := nameWithin(tc.a, tc.b); got != tc.want {
			t.Errorf("nameWithin(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

// A setting's first-person narrator counts only when the book's speech
// tags say "I": a third-person book with one named loses it.
func TestWithFirstPersonNeedsFirstPersonTags(t *testing.T) {
	srv, s, _, _ := newTestServer(t)
	mk := func(tag string) *store.Book {
		var blocks []store.BlockInput
		for range 4 {
			blocks = append(blocks, store.BlockInput{Kind: store.BlockText, Text: "“Fine,”", IsQuote: true}, store.BlockInput{Kind: store.BlockText, Text: tag, Inline: true})
		}
		id, _, err := s.CreateBook("B "+tag, "A", "en", "", "", 0, []store.ChapterInput{{Title: "One", Blocks: blocks}})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetBookSetting(id, store.BookSetting{Narrator: "Jason"}, store.BookSettingAuto); err != nil {
			t.Fatal(err)
		}
		b, _ := s.GetBook(id)
		return b
	}
	if got := srv.withFirstPerson(speakerattr.Roster{}, mk("I said.")).FirstPerson; got != "Jason" {
		t.Errorf("first-person book: FirstPerson = %q", got)
	}
	if got := srv.withFirstPerson(speakerattr.Roster{}, mk("he said.")).FirstPerson; got != "" {
		t.Errorf("third-person book: FirstPerson = %q, want none", got)
	}
}

// Restore sets a line back only while it still has From, never over a
// reader's edit, and never to a name that isn't a character.
func TestRestoreSpeakers(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{{Title: "One", Blocks: []store.BlockInput{
		{Kind: store.BlockText, Text: "“A,”", IsQuote: true},
		{Kind: store.BlockText, Text: "“B,”", IsQuote: true},
		{Kind: store.BlockText, Text: "“C,”", IsQuote: true},
		{Kind: store.BlockText, Text: "“D,”", IsQuote: true},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	for _, n := range []string{"Mira", "Tomas"} {
		if _, _, err := s.UpsertCharacter(store.SeriesScope(book), n, false); err != nil {
			t.Fatal(err)
		}
	}
	ch, _ := s.GetChapterByIdx(bookID, 0)
	if err := s.SetParagraphSpeakers(ch.ID, map[int]string{0: "Tomas", 1: "Tomas", 2: "Tomas", 3: "Tomas"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordParagraphEdit(mustParagraphID(t, s, ch.ID, 2), store.EditSpeaker, "Mira", "Tomas"); err != nil {
		t.Fatal(err)
	}
	body := restoreSpeakersRequest{Lines: []restoreSpeakerLine{
		{ChapterIdx: 0, ParagraphIdx: 0, From: "Tomas", To: "Mira"},  // restored
		{ChapterIdx: 0, ParagraphIdx: 1, From: "Mira", To: "Tomas"},  // no longer has From
		{ChapterIdx: 0, ParagraphIdx: 2, From: "Tomas", To: "Mira"},  // edited
		{ChapterIdx: 0, ParagraphIdx: 3, From: "Tomas", To: "Ghost"}, // not a character
	}}
	var out restoreSpeakersResponse
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/speakers/restore", body, nil)
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Restored != 1 || out.Skipped != 3 {
		t.Fatalf("response = %+v", out)
	}
	ps, _ := s.ListParagraphsRaw(ch.ID)
	if ps[0].Speaker != "Mira" || ps[1].Speaker != "Tomas" || ps[2].Speaker != "Tomas" || ps[3].Speaker != "Tomas" {
		t.Fatalf("speakers = %q %q %q %q", ps[0].Speaker, ps[1].Speaker, ps[2].Speaker, ps[3].Speaker)
	}
}
