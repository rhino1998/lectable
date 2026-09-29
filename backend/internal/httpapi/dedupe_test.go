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
