package httpapi

import (
	"net/http"
	"slices"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/store"
)

func TestDedupeCharacters(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	speakers := []string{"Protagonist", "Steve", "woman", "Woman", "doctor", "Lars", "Lars Machmüller", "Neighbor 1", "Neighbor 2", "Mikhail", "Mikhail Fedorov", "Mikhail Ivanov"}
	blocks := make([]store.BlockInput, len(speakers))
	for i := range speakers {
		blocks[i] = store.BlockInput{Kind: store.BlockText, Text: "“Hi.”", IsQuote: true}
	}
	bookID, _, err := s.CreateBook("Arise", "A", "en", "", "", 0, []store.ChapterInput{{Title: "One", Blocks: blocks}})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	byIdx := map[int]string{}
	for i, n := range speakers {
		if _, _, err := s.UpsertCharacter(scope, n, false); err != nil {
			t.Fatal(err)
		}
		byIdx[i] = n
	}
	// "woman" and "Woman" only coexist in data made before the lookup
	// ignored case - make the lower-case one directly.
	ch, _ := s.GetChapterByIdx(bookID, 0)
	if err := s.SetParagraphSpeakers(ch.ID, byIdx); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBookSetting(bookID, store.BookSetting{Narrator: "Steve"}, store.BookSettingManual); err != nil {
		t.Fatal(err)
	}

	var resp dedupeResponse
	r := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/characters/dedupe", nil, &resp)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("dedupe: %d", r.StatusCode)
	}
	chars, _ := s.ListCharacters(scope)
	var names []string
	for _, c := range chars {
		names = append(names, c.Name)
		if c.Name == "Doctor" && !c.IsRole {
			t.Errorf("Doctor should have become a role")
		}
		if c.Name == "Lars Machmüller" && !slices.Contains(c.Aliases, "Lars") {
			t.Errorf("Lars Machmüller should keep Lars as an alias: %v", c.Aliases)
		}
	}
	slices.Sort(names)
	want := []string{"Doctor", "Lars Machmüller", "Mikhail", "Mikhail Fedorov", "Mikhail Ivanov", "Neighbor 1", "Neighbor 2", "Steve", "Woman"}
	if !slices.Equal(names, want) {
		t.Fatalf("roster after dedupe = %v\nwant %v\nmerges %+v", names, want, resp.Merges)
	}
	ps, _ := s.ListParagraphsRaw(ch.ID)
	got := map[string]int{}
	for _, p := range ps {
		got[p.Speaker]++
	}
	if got["Steve"] != 2 || got["Lars Machmüller"] != 2 || got["Doctor"] != 1 || got["Mikhail"] != 1 {
		t.Fatalf("speakers after dedupe = %v", got)
	}
}

func TestDedupeCharactersApostropheVariants(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	blocks := []store.BlockInput{
		{Kind: store.BlockText, Text: "“Hi.”", IsQuote: true},
		{Kind: store.BlockText, Text: "“Hello.”", IsQuote: true},
	}
	bookID, _, err := s.CreateBook("Inn", "A", "en", "", "", 0, []store.ChapterInput{{Title: "One", Blocks: blocks}})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	if _, _, err := s.UpsertCharacter(scope, "Ve’delina", false); err != nil {
		t.Fatal(err)
	}
	// The two spellings only coexist in a roster made before the lookup
	// ignored the apostrophe - rename a second row into the other one.
	straight, _, err := s.UpsertCharacter(scope, "Placeholder", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RenameCharacter(straight.ID, "Ve'delina"); err != nil {
		t.Fatal(err)
	}
	ch, _ := s.GetChapterByIdx(bookID, 0)
	if err := s.SetParagraphSpeakers(ch.ID, map[int]string{0: "Ve’delina", 1: "Ve'delina"}); err != nil {
		t.Fatal(err)
	}

	var resp dedupeResponse
	if r := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/characters/dedupe", nil, &resp); r.StatusCode != http.StatusOK {
		t.Fatalf("dedupe: %d", r.StatusCode)
	}
	chars, _ := s.ListCharacters(scope)
	if len(chars) != 1 || chars[0].Name != "Ve’delina" {
		t.Fatalf("roster after dedupe = %+v\nmerges %+v", chars, resp.Merges)
	}
	if len(resp.Merges) != 1 || resp.Merges[0].Reason != "same name, different apostrophe" {
		t.Fatalf("merges = %+v", resp.Merges)
	}
	ps, _ := s.ListParagraphsRaw(ch.ID)
	for _, p := range ps {
		if p.Speaker != "Ve’delina" {
			t.Fatalf("paragraph %d speaker = %q", p.Idx, p.Speaker)
		}
	}
	// And a new line under the other spelling lands on the same row.
	if c, created, err := s.UpsertCharacter(scope, "ve'delina", false); err != nil || created || c.Name != "Ve’delina" {
		t.Fatalf("UpsertCharacter = %+v, created %v, err %v", c, created, err)
	}
}
